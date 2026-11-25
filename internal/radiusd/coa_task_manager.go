package radiusd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/pkg/common"
	"gorm.io/gorm"
)

// CoATaskManager implements the durable outbox around the RFC 5176 client:
// a task row is written before transmission, and its terminal status and
// Identifier are persisted so restarts can retry with the same packet identity.
type CoATaskManager struct {
	db *gorm.DB
}

func NewCoATaskManager(db *gorm.DB) *CoATaskManager { return &CoATaskManager{db: db} }

// Create stores a task in pending state before any packet is sent.
func (m *CoATaskManager) Create(ctx context.Context, task *domain.RadiusCoATask) error {
	if task.ID == 0 {
		task.ID = common.UUIDint64()
	}
	if task.Status == "" {
		task.Status = "pending"
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	return m.db.WithContext(ctx).Create(task).Error
}

// MarkSending records the first transmission attempt.
func (m *CoATaskManager) MarkSending(ctx context.Context, id int64, identifier int) error {
	return m.db.WithContext(ctx).Model(&domain.RadiusCoATask{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"status": "sending", "identifier": identifier, "sent_at": time.Now().UTC(),
		}).Error
}

// Complete persists the exchange result. A NAK or timeout is a terminal task
// state, not a database error.
func (m *CoATaskManager) Complete(ctx context.Context, id int64, result *CoAResult, buildErr error) error {
	status := "failed"
	if result != nil {
		switch {
		case result.Success:
			status = "ack"
		case result.ResponseCode != "":
			status = "nak"
		case result.TimedOut:
			status = "timeout"
		}
	}
	updates := map[string]interface{}{
		"status": status, "finished_at": time.Now().UTC(),
	}
	if buildErr != nil {
		updates["error"] = buildErr.Error()
	}
	if result != nil {
		updates["identifier"] = result.Identifier
		updates["attempts"] = result.Attempts
		updates["error_cause"] = result.ErrorCause
		updates["error_cause_text"] = result.ErrorCauseText
		updates["rtt_ms"] = result.RTT.Milliseconds()
		if result.Err != "" {
			updates["error"] = result.Err
		}
	}
	return m.db.WithContext(ctx).Model(&domain.RadiusCoATask{}).Where("id = ?", id).Updates(updates).Error
}

// List returns tasks newest-first, optionally filtered by session.
func (m *CoATaskManager) List(ctx context.Context, acctSessionID string, limit int) ([]domain.RadiusCoATask, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := m.db.WithContext(ctx).Model(&domain.RadiusCoATask{})
	if acctSessionID != "" {
		q = q.Where("acct_session_id = ?", acctSessionID)
	}
	var tasks []domain.RadiusCoATask
	err := q.Order("id DESC").Limit(limit).Find(&tasks).Error
	return tasks, err
}

// Pending returns unfinished tasks eligible for redelivery. Only tasks with a
// stored identity and an Identifier can be safely retried.
func (m *CoATaskManager) Pending(ctx context.Context, limit int) ([]domain.RadiusCoATask, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var tasks []domain.RadiusCoATask
	err := m.db.WithContext(ctx).
		Where("status IN ? AND identity_json <> ? AND attempts > 0", []string{"sending", "timeout"}, "").
		Order("id ASC").Limit(limit).Find(&tasks).Error
	return tasks, err
}

// Execute sends one task with the given client, pinning the Identifier when
// one was already assigned. It writes all state transitions itself.
func (m *CoATaskManager) Execute(ctx context.Context, task *domain.RadiusCoATask, client *CoAService) (*CoAResult, error) {
	var id SessionIdentity
	if err := json.Unmarshal([]byte(task.IdentityJSON), &id); err != nil {
		return nil, fmt.Errorf("coa task: invalid identity: %w", err)
	}
	opts := []CoAOption{WithCoATimeout(5 * time.Second), WithCoARetries(1)}
	if task.Identifier > 0 {
		opts = append(opts, WithCoAIdentifier(task.Identifier))
	}
	retryClient := NewCoAService(nil, opts...)
	retryClient.exchange = client.exchange

	target := CoATarget{Addr: task.NasAddr, Port: task.NasPort}
	var result *CoAResult
	var err error
	switch task.Action {
	case "disconnect":
		result, err = retryClient.Disconnect(ctx, target, id)
	case "coa":
		changes := make([]AttributeSetter, 0, 2)
		if task.FilterID != "" {
			changes = append(changes, WithFilterID(task.FilterID))
		}
		if task.SessionTimeoutSeconds > 0 {
			changes = append(changes, WithSessionTimeout(uint32(task.SessionTimeoutSeconds)))
		}
		result, err = retryClient.CoA(ctx, target, id, changes...)
	default:
		return nil, fmt.Errorf("coa task: unknown action %q", task.Action)
	}
	if markErr := m.Complete(ctx, task.ID, result, err); markErr != nil {
		return result, markErr
	}
	if err != nil {
		return result, err
	}
	if !result.Success {
		return result, fmt.Errorf("coa task: %s", result.ResponseCode)
	}
	return result, nil
}

// RetryPending redelivers unfinished tasks after a restart. Failures are
// recorded on the task so an operator can inspect them; the loop continues
// with the remaining tasks.
func (m *CoATaskManager) RetryPending(ctx context.Context, client *CoAService, limit int) (int, error) {
	tasks, err := m.Pending(ctx, limit)
	if err != nil {
		return 0, err
	}
	sent := 0
	for i := range tasks {
		if ctx.Err() != nil {
			break
		}
		if _, execErr := m.Execute(ctx, &tasks[i], client); execErr == nil {
			sent++
		}
	}
	return sent, nil
}

// ErrNoPendingCoATasks is returned by callers that treat an empty retry batch
// as a distinct outcome.
var ErrNoPendingCoATasks = errors.New("coa: no pending tasks")
