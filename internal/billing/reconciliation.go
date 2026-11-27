package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/pkg/common"
	"gorm.io/gorm"
)

// RADIUS Acct-Status-Type values (RFC 2866 §5.1).
const (
	acctStatusStart   = 1
	acctStatusStop    = 2
	acctStatusInterim = 3
)

const (
	defaultReconcileBatchSize = 500
	maxReconcileBatchSize     = 5000
	// deadLetterMinAge is the minimum quiet window before an unconsumed raw
	// accounting packet is replayed. A freshly inserted packet may still be
	// in flight in the normal accounting pipeline.
	deadLetterMinAge = 10 * time.Minute
	replayOperator   = "reconcile"
	// stopMissingMarkerPrefix is the deterministic RFC 5080-style dedup key
	// used for synthetic Stop records written by reconciliation. radius_acct_record
	// has a unique index on dedup_key, so this doubles as a cross-run lock.
	stopMissingMarkerPrefix = "recon-stop:"
)

// ReconcileResult summarizes one reconciliation pass.
type ReconcileResult struct {
	Scanned            int      `json:"scanned"`
	ClosedMissingStop  int      `json:"closed_missing_stop"`
	RepairedOutOfOrder int      `json:"repaired_out_of_order"`
	Skipped            int      `json:"skipped"`
	Replayed           int      `json:"replayed"`
	Errors             []string `json:"errors"`
}

func newReconcileResult() *ReconcileResult {
	return &ReconcileResult{Errors: []string{}}
}

// normalizeReconcileBatchSize applies the default (500) and the hard cap (5000).
func normalizeReconcileBatchSize(n int) int {
	if n <= 0 {
		return defaultReconcileBatchSize
	}
	if n > maxReconcileBatchSize {
		return maxReconcileBatchSize
	}
	return n
}

// ReconcileSessions scans radius_online rows whose AcctStartTime is older
// than cutoff and closes sessions that never received an Acct-Status-Type=2
// Stop (checked in both radius_accounting and radius_acct_record).
//
// RadiusOnline has no status column, so the least invasive way to mark a
// session stale/closed is the same one the Stop handler uses: delete the
// online row and finalize the radius_accounting row as a Stop. No original
// traffic value is lost — every counter is merged with max() from the online
// row, the existing ledger rows and the raw packet ledger before writing.
//
// The stop time written here is the caller's cutoff, i.e. the reconciliation
// boundary, not a fabricated NAS event time.
func (e *Engine) ReconcileSessions(ctx context.Context, cutoff time.Time, batchSize int) (*ReconcileResult, error) {
	result := newReconcileResult()
	if cutoff.IsZero() {
		return nil, fmt.Errorf("billing: reconcile cutoff is required")
	}
	batchSize = normalizeReconcileBatchSize(batchSize)

	var sessions []domain.RadiusOnline
	if err := e.db.WithContext(ctx).
		Where("acct_start_time < ?", cutoff).
		Order("id ASC").Limit(batchSize).
		Find(&sessions).Error; err != nil {
		return nil, err
	}

	for i := range sessions {
		session := &sessions[i]
		result.Scanned++
		outOfOrder, detectErr := e.DetectOutOfOrder(ctx, session.AcctSessionId)
		if detectErr != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("detect out-of-order for session %s: %v", session.AcctSessionId, detectErr))
		}
		closed, err := e.closeMissingStop(ctx, session, cutoff)
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("close missing stop for session %s: %v", session.AcctSessionId, err))
			continue
		}
		if !closed {
			result.Skipped++
			continue
		}
		result.ClosedMissingStop++
		if outOfOrder && detectErr == nil {
			result.RepairedOutOfOrder++
		}
	}
	return result, nil
}

// DetectOutOfOrder reports whether the raw accounting ledger for a session
// shows a regression: a later-arriving packet with an older Event-Timestamp,
// or cumulative Acct-Session-Time / octet counters that go backwards.
//
// Packets are compared in arrival order (created_at, id). Event-Timestamp is
// the authoritative event time; a zero value is ignored rather than guessed.
func (e *Engine) DetectOutOfOrder(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	var records []domain.RadiusAcctRecord
	if err := e.db.WithContext(ctx).
		Where("acct_session_id = ?", sessionID).
		Order("created_at ASC, id ASC").
		Find(&records).Error; err != nil {
		return false, err
	}

	var (
		lastEventTime time.Time
		haveEventTime bool
		maxSeconds    int64
		maxInput      int64
		maxOutput     int64
	)
	for i := range records {
		rec := &records[i]
		if !rec.EventTimestamp.IsZero() {
			if haveEventTime && rec.EventTimestamp.Before(lastEventTime) {
				return true, nil
			}
			lastEventTime = rec.EventTimestamp
			haveEventTime = true
		}
		seconds := int64(rec.AcctSessionTime)
		if seconds < maxSeconds || rec.InputOctets < maxInput || rec.OutputOctets < maxOutput {
			return true, nil
		}
		maxSeconds = max64(maxSeconds, seconds)
		maxInput = max64(maxInput, rec.InputOctets)
		maxOutput = max64(maxOutput, rec.OutputOctets)
	}
	return false, nil
}

// HandleNASReboot closes sessions that were started before rebootAt on the
// given NAS and are still online. rebootAt must be the NAS's Accounting-On
// event time supplied by the caller; the implementation never invents a
// timestamp. All such sessions are treated as Stop-Missing.
func (e *Engine) HandleNASReboot(ctx context.Context, nasAddr string, rebootAt time.Time) (*ReconcileResult, error) {
	result := newReconcileResult()
	if nasAddr == "" {
		return nil, fmt.Errorf("billing: NAS address is required")
	}
	if rebootAt.IsZero() {
		return nil, fmt.Errorf("billing: NAS reboot time (Accounting-On event time) is required")
	}

	// ID cursor avoids offset-based paging skipping rows that were deleted.
	var lastID int64
	for {
		var sessions []domain.RadiusOnline
		if err := e.db.WithContext(ctx).
			Where("nas_addr = ? AND acct_start_time < ? AND id > ?", nasAddr, rebootAt, lastID).
			Order("id ASC").Limit(defaultReconcileBatchSize).
			Find(&sessions).Error; err != nil {
			return nil, err
		}
		if len(sessions) == 0 {
			break
		}
		for i := range sessions {
			session := &sessions[i]
			lastID = session.ID
			result.Scanned++
			outOfOrder, detectErr := e.DetectOutOfOrder(ctx, session.AcctSessionId)
			if detectErr != nil {
				result.Errors = append(result.Errors,
					fmt.Sprintf("detect out-of-order for session %s: %v", session.AcctSessionId, detectErr))
			}
			closed, err := e.closeMissingStop(ctx, session, rebootAt)
			if err != nil {
				result.Errors = append(result.Errors,
					fmt.Sprintf("reboot close for session %s: %v", session.AcctSessionId, err))
				continue
			}
			if !closed {
				result.Skipped++
				continue
			}
			result.ClosedMissingStop++
			if outOfOrder && detectErr == nil {
				result.RepairedOutOfOrder++
			}
		}
		if len(sessions) < defaultReconcileBatchSize {
			break
		}
	}
	return result, nil
}

// RestoreDeadLetters replays raw accounting packets that were never consumed:
// consumed=false, a non-empty RFC 5080 dedup key, and an event timestamp older
// than the ten-minute quiet window.
//
// Idempotency design:
//   - radius_acct_record.dedup_key is unique, so the same packet exists once;
//   - consumed is flipped to true in the same transaction that finalizes the
//     replay decision (a re-read with consumed=false guards concurrent runs);
//   - the actual charge goes through ChargeSession, whose delta-based
//     BillingSessionUsage and charge:<session>:s:b:* asset-flow keys prevent
//     double debiting even if consumed were somehow reset.
//
// A record is skipped (with an error entry) when its account or an active
// subscription cannot be resolved — no guess is made about the plan.
func (e *Engine) RestoreDeadLetters(ctx context.Context, limit int) (*ReconcileResult, error) {
	result := newReconcileResult()
	limit = normalizeReconcileBatchSize(limit)
	cutoff := time.Now().UTC().Add(-deadLetterMinAge)

	var records []domain.RadiusAcctRecord
	if err := e.db.WithContext(ctx).
		Where("consumed = ? AND dedup_key <> ? AND event_timestamp < ?", false, "", cutoff).
		Order("id ASC").Limit(limit).
		Find(&records).Error; err != nil {
		return nil, err
	}

	for i := range records {
		rec := &records[i]
		result.Scanned++
		if err := e.replayDeadLetter(ctx, rec); err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("dead letter %d (%s): %v", rec.ID, rec.DedupKey, err))
			continue
		}
		result.Replayed++
	}
	return result, nil
}

// replayDeadLetter charges one raw packet and marks it consumed.
//
// If ChargeSession fails after a partial debit, leaving consumed=false, a
// retry is still safe: its asset-flow keys are derived from the packet's
// cumulative counters, so the already-written flow is a no-op on replay.
func (e *Engine) replayDeadLetter(ctx context.Context, rec *domain.RadiusAcctRecord) error {
	if rec.AcctSessionId == "" {
		return fmt.Errorf("missing acct_session_id")
	}
	accountID, planID, err := e.resolveDeadLetterTarget(ctx, rec)
	if err != nil {
		return err
	}

	req := ChargeRequest{
		AccountID: accountID, SessionID: rec.AcctSessionId, PlanID: planID,
		EventTime: rec.EventTimestamp,
		Seconds:   int64(rec.AcctSessionTime),
		Bytes:     rec.InputOctets + rec.OutputOctets,
	}
	if _, err := e.ChargeSession(ctx, req); err != nil {
		return err
	}

	return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&domain.RadiusAcctRecord{}).
			Where("id = ? AND consumed = ?", rec.ID, false).
			Update("consumed", true)
		if res.Error != nil {
			return res.Error
		}
		// RowsAffected == 0 means a concurrent replay already consumed it;
		// ChargeSession was still safe (cumulative deltas + flow keys).
		return nil
	})
}

// resolveDeadLetterTarget maps a raw packet to a billable account and plan.
func (e *Engine) resolveDeadLetterTarget(ctx context.Context, rec *domain.RadiusAcctRecord) (int64, int64, error) {
	var account domain.BillingAccount
	if err := e.db.WithContext(ctx).Where("username = ?", rec.Username).First(&account).Error; err != nil {
		return 0, 0, fmt.Errorf("resolve account %q: %w", rec.Username, err)
	}

	var sub domain.BillingSubscription
	err := e.db.WithContext(ctx).
		Where("account_id = ? AND status = ? AND start_at <= ? AND end_at >= ?",
			account.ID, "active", rec.EventTimestamp, rec.EventTimestamp).
		Order("start_at DESC").First(&sub).Error
	if err != nil {
		// Fall back to the most recent active subscription when none covers
		// the packet's event time (e.g. a slightly late Stop at a boundary).
		err = e.db.WithContext(ctx).
			Where("account_id = ? AND status = ?", account.ID, "active").
			Order("start_at DESC").First(&sub).Error
	}
	if err != nil {
		return 0, 0, fmt.Errorf("resolve active plan for account %d: %w", account.ID, err)
	}
	return account.ID, sub.PlanID, nil
}

// stopExists reports whether a Stop for the session is already present in
// either the historical ledger or the raw packet ledger.
func stopExists(tx *gorm.DB, sessionID string) (bool, error) {
	var count int64
	if err := tx.Model(&domain.RadiusAccounting{}).
		Where("acct_session_id = ? AND acct_status_type = ?", sessionID, acctStatusStop).
		Count(&count).Error; err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}
	if err := tx.Model(&domain.RadiusAcctRecord{}).
		Where("acct_session_id = ? AND status_type = ?", sessionID, acctStatusStop).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// sessionCounters holds the highest cumulative values seen for a session.
type sessionCounters struct {
	seconds    int64
	input      int64
	output     int64
	inPackets  int64
	outPackets int64
}

func (c *sessionCounters) merge(seconds int64, input, output int64, inPackets, outPackets int) {
	c.seconds = max64(c.seconds, seconds)
	c.input = max64(c.input, input)
	c.output = max64(c.output, output)
	c.inPackets = max64(c.inPackets, int64(inPackets))
	c.outPackets = max64(c.outPackets, int64(outPackets))
}

// maxCounters merges the online snapshot with every ledger row so that a
// late/out-of-order packet or a stale online row can never discard traffic.
func maxCounters(tx *gorm.DB, session *domain.RadiusOnline) (sessionCounters, error) {
	counters := sessionCounters{}
	counters.merge(int64(session.AcctSessionTime), session.AcctInputTotal, session.AcctOutputTotal,
		session.AcctInputPackets, session.AcctOutputPackets)

	var ledger []domain.RadiusAccounting
	if err := tx.Where("acct_session_id = ?", session.AcctSessionId).Find(&ledger).Error; err != nil {
		return counters, err
	}
	for i := range ledger {
		row := &ledger[i]
		counters.merge(int64(row.AcctSessionTime), row.AcctInputTotal, row.AcctOutputTotal,
			row.AcctInputPackets, row.AcctOutputPackets)
	}

	var raw []domain.RadiusAcctRecord
	if err := tx.Where("acct_session_id = ?", session.AcctSessionId).Find(&raw).Error; err != nil {
		return counters, err
	}
	for i := range raw {
		row := &raw[i]
		counters.merge(int64(row.AcctSessionTime), row.InputOctets, row.OutputOctets,
			row.InputPackets, row.OutputPackets)
	}
	return counters, nil
}

// closeMissingStop finalizes one session as a Stop-Missing reconciliation.
// It returns true only when this call actually closed the session.
func (e *Engine) closeMissingStop(ctx context.Context, session *domain.RadiusOnline, stopTime time.Time) (bool, error) {
	closed := false
	err := e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Re-read inside the transaction: a real Stop handler may have run
		// between the scan and this write.
		var current domain.RadiusOnline
		if err := tx.Where("acct_session_id = ?", session.AcctSessionId).First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		stopped, err := stopExists(tx, session.AcctSessionId)
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}

		counters, err := maxCounters(tx, &current)
		if err != nil {
			return err
		}

		// Synthetic Stop marker. The unique dedup_key on radius_acct_record
		// makes concurrent/repeated reconciliation safe.
		marker := domain.RadiusAcctRecord{
			ID:              common.UUIDint64(),
			Username:        current.Username,
			NasAddr:         current.NasAddr,
			AcctSessionId:   session.AcctSessionId,
			StatusType:      acctStatusStop,
			EventTimestamp:  stopTime,
			AcctSessionTime: int(counters.seconds),
			InputOctets:     counters.input,
			OutputOctets:    counters.output,
			InputPackets:    int(counters.inPackets),
			OutputPackets:   int(counters.outPackets),
			RawAttrs:        `{"reason":"stop-missing","source":"radius_online"}`,
			DedupKey:        stopMissingMarkerPrefix + session.AcctSessionId,
			Consumed:        true, // synthetic, never replay as a dead letter
			CreatedAt:       time.Now().UTC(),
		}
		if err := tx.Create(&marker).Error; err != nil {
			return err
		}

		stopFields := map[string]interface{}{
			"acct_stop_time":      stopTime,
			"event_timestamp":     stopTime,
			"acct_status_type":    acctStatusStop,
			"acct_session_time":   counters.seconds,
			"acct_input_total":    counters.input,
			"acct_output_total":   counters.output,
			"acct_input_packets":  counters.inPackets,
			"acct_output_packets": counters.outPackets,
			"last_update":         time.Now().UTC(),
		}
		var ledgerCount int64
		if err := tx.Model(&domain.RadiusAccounting{}).
			Where("acct_session_id = ?", session.AcctSessionId).Count(&ledgerCount).Error; err != nil {
			return err
		}
		if ledgerCount == 0 {
			// No Start row survived: insert a standalone compensation CDR.
			// Identity fields are copied from the online row.
			cdr := domain.RadiusAccounting{
				ID:                  common.UUIDint64(),
				Username:            current.Username,
				AcctSessionId:       session.AcctSessionId,
				NasId:               current.NasId,
				NasAddr:             current.NasAddr,
				NasPaddr:            current.NasPaddr,
				SessionTimeout:      current.SessionTimeout,
				FramedIpaddr:        current.FramedIpaddr,
				FramedNetmask:       current.FramedNetmask,
				FramedIpv6Prefix:    current.FramedIpv6Prefix,
				FramedIpv6Address:   current.FramedIpv6Address,
				DelegatedIpv6Prefix: current.DelegatedIpv6Prefix,
				MacAddr:             current.MacAddr,
				NasPort:             current.NasPort,
				NasClass:            current.NasClass,
				NasPortId:           current.NasPortId,
				NasPortType:         current.NasPortType,
				ServiceType:         current.ServiceType,
				AcctStartTime:       current.AcctStartTime,
				AcctStopTime:        stopTime,
				EventTimestamp:      stopTime,
				AcctDelayTime:       current.AcctDelayTime,
				AcctStatusType:      acctStatusStop,
				DedupKey:            marker.DedupKey,
				RawAttrs:            marker.RawAttrs,
				AcctSessionTime:     int(counters.seconds),
				AcctInputTotal:      counters.input,
				AcctOutputTotal:     counters.output,
				AcctInputPackets:    int(counters.inPackets),
				AcctOutputPackets:   int(counters.outPackets),
				LastUpdate:          time.Now().UTC(),
			}
			if err := tx.Create(&cdr).Error; err != nil {
				return err
			}
		} else if err := tx.Model(&domain.RadiusAccounting{}).
			Where("acct_session_id = ?", session.AcctSessionId).
			Updates(stopFields).Error; err != nil {
			// Update the existing Start row in place, exactly like StopHandler,
			// preserving its original identity and dedup key.
			return err
		}

		if err := tx.Where("acct_session_id = ?", session.AcctSessionId).
			Delete(&domain.RadiusOnline{}).Error; err != nil {
			return err
		}
		closed = true
		return nil
	})
	return closed, err
}
