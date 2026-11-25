package domain

import "time"

// RadiusAcctRecord is the append-only raw accounting packet ledger required
// for idempotent charging, out-of-order replay and packet-level diagnostics.
// One row is stored for every accepted Start / Interim-Update / Stop packet;
// duplicates (same RFC 5080 dedup key) are ignored via the unique index.
type RadiusAcctRecord struct {
	ID              int64     `json:"id,string"`
	Username        string    `gorm:"index" json:"username"`
	NasAddr         string    `gorm:"index" json:"nas_addr"`
	AcctSessionId   string    `gorm:"index" json:"acct_session_id"`
	StatusType      int       `gorm:"index" json:"status_type"`
	Identifier      int       `json:"identifier"`
	EventTimestamp  time.Time `gorm:"index" json:"event_timestamp"`
	AcctDelayTime   int       `json:"acct_delay_time"`
	AcctSessionTime int       `json:"acct_session_time"`
	InputOctets     int64     `json:"input_octets,string"`
	OutputOctets    int64     `json:"output_octets,string"`
	InputPackets    int       `json:"input_packets"`
	OutputPackets   int       `json:"output_packets"`
	RawAttrs        string    `json:"raw_attrs"`
	DedupKey        string    `gorm:"uniqueIndex" json:"dedup_key"`
	Consumed        bool      `gorm:"index" json:"consumed"`
	CreatedAt       time.Time `gorm:"index" json:"created_at"`
}

// TableName specifies the raw accounting record table name.
func (RadiusAcctRecord) TableName() string { return "radius_acct_record" }

// RadiusAcctDedup is the short-lived RFC 5080 §2.2.2 response cache key.
// Entries older than 30 seconds are pruned; a repeated request with the same
// key is answered from the cached response instead of being reprocessed.
type RadiusAcctDedup struct {
	DedupKey  string    `gorm:"primaryKey" json:"dedup_key"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// TableName specifies the accounting dedup cache table name.
func (RadiusAcctDedup) TableName() string { return "radius_acct_dedup" }
