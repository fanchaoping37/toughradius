// Package analytics optionally archives RADIUS accounting records into
// ClickHouse for high-volume reporting. It is disabled by default: when no
// DSN is configured, all operations are no-ops and the existing SQL database
// remains the sole store.
//
// The schema mirrors RFC 2866/2869 fields recorded by the accounting packet
// layer: Acct-Status-Type (1/2/3), Event-Timestamp (RFC 2869 §5.13),
// Acct-Delay-Time, Gigawords-combined octets and RFC 5080 dedup key.
package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
)

const createTableSQL = `
CREATE TABLE IF NOT EXISTS radius_accounting_archive (
    event_time      DateTime,
    account_date    Date,
    username        String,
    nas_ip          String,
    nas_port        String,
    session_id      String,
    status_type     UInt8,
    session_time    UInt32,
    input_octets    UInt64,
    output_octets   UInt64,
    input_gigawords UInt32,
    output_gigawords UInt32,
    dedup_key       String,
    framed_ip       String,
    inserted_at     DateTime
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(account_date)
ORDER BY (account_date, username, session_id)
`

const insertSQL = `INSERT INTO radius_accounting_archive
(event_time, account_date, username, nas_ip, nas_port, session_id, status_type,
 session_time, input_octets, output_octets, input_gigawords, output_gigawords,
 dedup_key, framed_ip, inserted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Record is one archived accounting event.
type Record struct {
	EventTime       time.Time
	Username        string
	NASIP           string
	NASPort         string
	SessionID       string
	StatusType      uint8 // 1 Start, 2 Stop, 3 Interim-Update (RFC 2866 §5.1)
	SessionTime     uint32
	InputOctets     uint64
	OutputOctets    uint64
	InputGigawords  uint32
	OutputGigawords uint32
	DedupKey        string
	FramedIP        string
}

// Config configures ClickHouse archiving.
type Config struct {
	Enabled     bool
	DSN         string // clickhouse://user:password@host:9000/db?dial_timeout=...
	BatchSize   int
	FlushPeriod time.Duration
}

// Archive buffers and batch-inserts records into ClickHouse.
type Archive struct {
	cfg  Config
	db   *sql.DB
	mu   sync.Mutex
	buf  []Record
	done chan struct{}
	once sync.Once
}

// NewArchive opens a ClickHouse connection (no-op when disabled).
func NewArchive(ctx context.Context, cfg Config) (*Archive, error) {
	if !cfg.Enabled || cfg.DSN == "" {
		return &Archive{cfg: Config{Enabled: false}, done: make(chan struct{})}, nil
	}
	db, err := sql.Open("clickhouse", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("analytics: open clickhouse: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("analytics: clickhouse unreachable: %w", err)
	}
	if _, err := db.ExecContext(ctx, createTableSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("analytics: create table: %w", err)
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.FlushPeriod <= 0 {
		cfg.FlushPeriod = 10 * time.Second
	}
	a := &Archive{cfg: cfg, db: db, done: make(chan struct{})}
	go a.flushLoop()
	return a, nil
}

// Enabled reports whether ClickHouse archiving is active.
func (a *Archive) Enabled() bool { return a != nil && a.cfg.Enabled && a.db != nil }

// Write buffers a record; no-op when disabled.
func (a *Archive) Write(ctx context.Context, rec Record) error {
	if !a.Enabled() {
		return nil
	}
	a.mu.Lock()
	a.buf = append(a.buf, rec)
	full := len(a.buf) >= a.cfg.BatchSize
	a.mu.Unlock()
	if full {
		return a.Flush(ctx)
	}
	return nil
}

// Flush writes buffered records in one batch.
func (a *Archive) Flush(ctx context.Context) error {
	if !a.Enabled() {
		return nil
	}
	a.mu.Lock()
	if len(a.buf) == 0 {
		a.mu.Unlock()
		return nil
	}
	batch := a.buf
	a.buf = nil
	a.mu.Unlock()

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		// Put records back so a transient failure does not lose data.
		a.restore(batch)
		return err
	}
	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		_ = tx.Rollback()
		a.restore(batch)
		return err
	}
	defer stmt.Close()
	now := time.Now()
	for _, r := range batch {
		event := r.EventTime
		if event.IsZero() {
			event = now
		}
		if _, err := stmt.ExecContext(ctx,
			event, event.Format("2006-01-02"), r.Username, r.NASIP, r.NASPort,
			r.SessionID, r.StatusType, r.SessionTime, r.InputOctets, r.OutputOctets,
			r.InputGigawords, r.OutputGigawords, r.DedupKey, r.FramedIP, now,
		); err != nil {
			_ = tx.Rollback()
			a.restore(batch)
			return err
		}
	}
	return tx.Commit()
}

func (a *Archive) restore(batch []Record) {
	a.mu.Lock()
	a.buf = append(batch, a.buf...)
	a.mu.Unlock()
}

func (a *Archive) flushLoop() {
	ticker := time.NewTicker(a.cfg.FlushPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), a.cfg.FlushPeriod)
			if err := a.Flush(ctx); err != nil {
				// Logging is done by the caller-facing Write path; avoid an
				// import cycle by keeping this layer silent.
				_ = err
			}
			cancel()
		case <-a.done:
			return
		}
	}
}

// Close flushes remaining records and releases the connection.
func (a *Archive) Close() error {
	if a == nil {
		return nil
	}
	var err error
	a.once.Do(func() {
		close(a.done)
		if a.db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = a.Flush(ctx)
			cancel()
			if closeErr := a.db.Close(); err == nil {
				err = closeErr
			}
		}
	})
	return err
}

// ErrDisabled is returned by query APIs when ClickHouse is not configured.
var ErrDisabled = errors.New("analytics: clickhouse is disabled")
