// Package billing implements the prepaid/postpaid charging engine described in
// SRS §12.4: integer fen/second/byte arithmetic, gift-first deduction,
// optimistic locking and idempotent asset flows.
package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/pkg/common"
	"gorm.io/gorm"
)

var (
	// ErrInsufficient is returned when an asset balance cannot cover a debit.
	ErrInsufficient = errors.New("billing: insufficient balance")
	// ErrNotFound is returned when a referenced account/plan does not exist.
	ErrNotFound = errors.New("billing: not found")
)

// PriceStep is one tier of cumulative tiered pricing (left-closed/right-open).
type PriceStep struct {
	UpToBytes     int64 `json:"up_to_bytes"`
	PricePerGBFen int64 `json:"price_per_gb_fen"`
}

// DiscountWindow discounts charges attributed to a recurring hour range.
type DiscountWindow struct {
	StartHour       int `json:"start_hour"`
	EndHour         int `json:"end_hour"`
	DiscountPercent int `json:"discount_percent"`
}

// Engine persists and charges account assets.
type Engine struct {
	db           *gorm.DB
	sessionLocks sync.Map // sessionID -> *sync.Mutex, serializes interim deltas
}

func NewEngine(db *gorm.DB) *Engine { return &Engine{db: db} }

// CreateAccount creates a subscriber asset account. It is idempotent on
// username: an existing account is returned unchanged.
func (e *Engine) CreateAccount(ctx context.Context, username, realName string, cashFen, timeSeconds, bytes int64) (*domain.BillingAccount, error) {
	var existing domain.BillingAccount
	err := e.db.WithContext(ctx).Where("username = ?", username).First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	acct := &domain.BillingAccount{
		ID: common.UUIDint64(), Username: username, RealName: realName,
		Status: "normal", CashFen: cashFen, TimeSeconds: timeSeconds, BytesQuota: bytes,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := e.db.WithContext(ctx).Create(acct).Error; err != nil {
		return nil, err
	}
	return acct, nil
}

// GetAccount loads an account by username.
func (e *Engine) GetAccount(ctx context.Context, username string) (*domain.BillingAccount, error) {
	var acct domain.BillingAccount
	if err := e.db.WithContext(ctx).Where("username = ?", username).First(&acct).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &acct, nil
}

// Credit adds funds/time/traffic to an account. Replaying the same
// idempotentKey is a no-op, so duplicate payment callbacks cannot double-credit.
func (e *Engine) Credit(ctx context.Context, accountID int64, assetType string, amount int64, bizType, refID, idempotentKey, operator string) error {
	if amount <= 0 || idempotentKey == "" {
		return fmt.Errorf("billing: credit requires positive amount and idempotent key")
	}
	return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return moveAsset(tx, accountID, assetType, "IN", amount, bizType, refID, idempotentKey, operator)
	})
}

// Debit subtracts funds/time/traffic. It returns ErrInsufficient if the
// balance is too low; replaying the same idempotentKey is a no-op.
func (e *Engine) Debit(ctx context.Context, accountID int64, assetType string, amount int64, bizType, refID, idempotentKey, operator string) error {
	if amount <= 0 || idempotentKey == "" {
		return fmt.Errorf("billing: debit requires positive amount and idempotent key")
	}
	return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return moveAsset(tx, accountID, assetType, "OUT", amount, bizType, refID, idempotentKey, operator)
	})
}

// balanceField maps an asset type to its optimistic-lock column.
func balanceField(assetType string) (string, error) {
	switch assetType {
	case "CASH":
		return "cash_fen", nil
	case "TIME":
		return "time_seconds", nil
	case "BYTES":
		return "bytes_quota", nil
	default:
		return "", fmt.Errorf("billing: unknown asset type %q", assetType)
	}
}

// moveAsset performs a CAS-based balance update and writes the append-only
// flow. For BYTES debits, gift balance is consumed before principal balance
// (SRS §12.4). Retries resolve optimistic-lock conflicts.
func moveAsset(tx *gorm.DB, accountID int64, assetType, direction string, amount int64, bizType, refID, key, operator string) error {
	field, err := balanceField(assetType)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		var flowCount int64
		if err := tx.Model(&domain.BillingAssetFlow{}).Where("idempotent_key = ?", key).Count(&flowCount).Error; err != nil {
			return err
		}
		if flowCount > 0 {
			return nil
		}

		var acct domain.BillingAccount
		if err := tx.First(&acct, accountID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if acct.Status == "frozen" || acct.Status == "closed" {
			return fmt.Errorf("billing: account %d is %s", accountID, acct.Status)
		}

		principal := int64(0)
		switch field {
		case "cash_fen":
			principal = acct.CashFen
		case "time_seconds":
			principal = acct.TimeSeconds
		case "bytes_quota":
			principal = acct.BytesQuota
		}
		gift := int64(0)
		if field == "bytes_quota" {
			gift = acct.GiftBytes
		} else if field == "time_seconds" {
			gift = acct.GiftTimeSeconds
		}

		if direction == "OUT" && gift+principal < amount {
			return ErrInsufficient
		}

		giftUsed := int64(0)
		if direction == "OUT" && gift > 0 {
			giftUsed = min64(gift, amount)
		}
		principalDelta := amount - giftUsed
		nextPrincipal := principal
		if direction == "IN" {
			nextPrincipal += amount
		} else {
			nextPrincipal -= principalDelta
		}
		nextGift := gift - giftUsed

		updates := map[string]interface{}{
			field: nextPrincipal, "version": acct.Version + 1, "updated_at": time.Now().UTC(),
		}
		if giftUsed > 0 {
			if field == "bytes_quota" {
				updates["gift_bytes"] = nextGift
			} else {
				updates["gift_time_seconds"] = nextGift
			}
		}
		res := tx.Model(&domain.BillingAccount{}).
			Where("id = ? AND version = ?", accountID, acct.Version).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			continue // concurrent update: reload and retry
		}

		balanceAfter := nextPrincipal
		if err := tx.Create(&domain.BillingAssetFlow{
			ID: common.UUIDint64(), AccountID: accountID, AssetType: assetType,
			Direction: direction, Amount: amount, BalanceAfter: balanceAfter,
			BizType: bizType, RefID: refID, IdempotentKey: key,
			Operator: operator, CreatedAt: time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		if giftUsed > 0 {
			if err := tx.Create(&domain.BillingAssetFlow{
				ID: common.UUIDint64(), AccountID: accountID, AssetType: assetType,
				Direction: "OUT", Amount: giftUsed, BalanceAfter: nextGift,
				BizType: "gift_offset", RefID: refID, IdempotentKey: key + ":gift",
				Operator: operator, CreatedAt: time.Now().UTC(),
			}).Error; err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("billing: optimistic lock contention on account %d", accountID)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// ChargeRequest carries cumulative session usage. Charging is delta-based:
// replayed or out-of-order Interim-Update packets never double-charge.
type ChargeRequest struct {
	AccountID int64
	SessionID string
	PlanID    int64
	EventTime time.Time
	Seconds   int64
	Bytes     int64
}

// ChargeResult describes what was charged for the usage delta.
type ChargeResult struct {
	AmountFen   int64 `json:"amount_fen,string"`
	DiscountFen int64 `json:"discount_fen,string"`
	Seconds     int64 `json:"seconds,string"`
	Bytes       int64 `json:"bytes,string"`
	Capped      bool  `json:"capped"`
	Noop        bool  `json:"noop"`
}

// ChargeSession charges the delta between the request's cumulative counters
// and the last metered counters for the same Acct-Session-Id.
func (e *Engine) ChargeSession(ctx context.Context, req ChargeRequest) (*ChargeResult, error) {
	if req.SessionID == "" || req.AccountID == 0 || req.PlanID == 0 {
		return nil, fmt.Errorf("billing: session id, account id and plan id are required")
	}
	lock, _ := e.sessionLocks.LoadOrStore(req.SessionID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	var plan domain.BillingPlan
	if err := e.db.WithContext(ctx).First(&plan, req.PlanID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	var usage domain.BillingSessionUsage
	err := e.db.WithContext(ctx).Where("session_id = ?", req.SessionID).First(&usage).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		usage = domain.BillingSessionUsage{SessionID: req.SessionID, AccountID: req.AccountID}
	}

	deltaSeconds := max64(0, req.Seconds-usage.Seconds)
	deltaBytes := max64(0, req.Bytes-usage.Bytes)
	if req.EventTime.Before(usage.LastEventTime) && usage.LastEventTime.After(time.Time{}) {
		// Out-of-order Interim-Update: keep the highest seen counters and
		// do not re-charge already metered usage (SRS §12.5).
		deltaSeconds, deltaBytes = 0, 0
	}

	result := &ChargeResult{Seconds: deltaSeconds, Bytes: deltaBytes}
	cashFen, timeSeconds, byteAmount := computeCharge(&plan, deltaSeconds, deltaBytes, usage.Bytes, usage.ChargedFen, req.EventTime, result)
	// Time plans may be cash-funded (no prepaid time account). Only debit the
	// time asset when the subscriber actually holds one.
	var account domain.BillingAccount
	if err := e.db.WithContext(ctx).First(&account, req.AccountID).Error; err != nil {
		return nil, err
	}
	if timeSeconds > 0 && account.TimeSeconds+account.GiftTimeSeconds == 0 {
		timeSeconds = 0
	}
	if result.Noop {
		return result, nil
	}

	if cashFen > 0 {
		key := fmt.Sprintf("charge:%s:s%d:b%d:cash", req.SessionID, req.Seconds, req.Bytes)
		if err := e.Debit(ctx, req.AccountID, "CASH", cashFen, "session_charge", req.SessionID, key, "engine"); err != nil {
			return nil, err
		}
	}
	if timeSeconds > 0 {
		key := fmt.Sprintf("charge:%s:s%d:b%d:time", req.SessionID, req.Seconds, req.Bytes)
		if err := e.Debit(ctx, req.AccountID, "TIME", timeSeconds, "session_charge", req.SessionID, key, "engine"); err != nil {
			return nil, err
		}
	}
	if byteAmount > 0 {
		key := fmt.Sprintf("charge:%s:s%d:b%d:bytes", req.SessionID, req.Seconds, req.Bytes)
		if err := e.Debit(ctx, req.AccountID, "BYTES", byteAmount, "session_charge", req.SessionID, key, "engine"); err != nil {
			return nil, err
		}
	}

	result.AmountFen = cashFen
	if err := e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current domain.BillingSessionUsage
		foundErr := tx.Where("session_id = ?", req.SessionID).First(&current).Error
		if foundErr != nil && !errors.Is(foundErr, gorm.ErrRecordNotFound) {
			return foundErr
		}
		if errors.Is(foundErr, gorm.ErrRecordNotFound) {
			current = domain.BillingSessionUsage{SessionID: req.SessionID, AccountID: req.AccountID}
		}
		updates := map[string]interface{}{
			"account_id":      req.AccountID,
			"seconds":         max64(current.Seconds, req.Seconds),
			"bytes":           max64(current.Bytes, req.Bytes),
			"charged_fen":     current.ChargedFen + cashFen,
			"last_event_time": req.EventTime,
			"updated_at":      time.Now().UTC(),
		}
		if foundErr != nil {
			if err := tx.Create(&domain.BillingSessionUsage{
				SessionID: req.SessionID, AccountID: req.AccountID,
				Seconds: req.Seconds, Bytes: req.Bytes,
				ChargedFen:    current.ChargedFen + cashFen,
				LastEventTime: req.EventTime, UpdatedAt: time.Now().UTC(),
			}).Error; err != nil {
				return err
			}
		} else if err := tx.Model(&domain.BillingSessionUsage{}).Where("session_id = ?", req.SessionID).Updates(updates).Error; err != nil {
			return err
		}
		// Revenue recognition is persisted with the usage watermark in the same
		// transaction. The cumulative counters make this replay-safe.
		usageKey := fmt.Sprintf("%s:s%d:b%d", req.SessionID, req.Seconds, req.Bytes)
		return postUsageChargeTx(tx, req.AccountID, req.SessionID, usageKey, cashFen)
	}); err != nil {
		return nil, err
	}
	return result, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// computeCharge applies the plan's charging rule to the usage delta and
// returns (cash fen, time seconds, traffic bytes) to debit.
func computeCharge(plan *domain.BillingPlan, deltaSeconds, deltaBytes, priorBytes, priorChargedFen int64, eventTime time.Time, result *ChargeResult) (int64, int64, int64) {
	switch plan.ChargeType {
	case "monthly", "daily":
		result.Noop = true // period fee is charged once by Subscribe
		return 0, 0, 0
	case "time", "combo":
		unit := plan.RoundSeconds
		if unit <= 0 {
			unit = 60
		}
		units := int64(0)
		if deltaSeconds > 0 {
			units = int64(math.Ceil(float64(deltaSeconds) / float64(unit)))
		}
		cash := units * plan.PriceFen
		timeSeconds := units * int64(unit)
		if plan.ChargeType == "time" {
			return applyDiscount(cash, priorChargedFen, plan, eventTime, result), timeSeconds, 0
		}
		// combo: also charge traffic below
		cash = applyDiscount(cash, priorChargedFen, plan, eventTime, result)
		trafficCash := computeTrafficCash(plan, deltaBytes, priorBytes, priorChargedFen, eventTime, result)
		return cash + trafficCash, timeSeconds, deltaBytes
	case "traffic":
		return computeTrafficCash(plan, deltaBytes, priorBytes, priorChargedFen, eventTime, result), 0, deltaBytes
	case "tiered":
		steps := parseSteps(plan.StepsJSON)
		total := priorBytes + deltaBytes
		cash := tieredCost(total, steps) - tieredCost(priorBytes, steps)
		return applyDiscount(cash, priorChargedFen, plan, eventTime, result), 0, deltaBytes
	case "capped":
		cash := computeTrafficCash(plan, deltaBytes, priorBytes, priorChargedFen, eventTime, result)
		if plan.CapFen > 0 {
			remaining := plan.CapFen - priorChargedFen
			if remaining <= 0 {
				return 0, 0, deltaBytes
			}
			if cash > remaining {
				cash = remaining
				result.Capped = true
			}
		}
		return cash, 0, deltaBytes
	default:
		result.Noop = true
		return 0, 0, 0
	}
}

func computeTrafficCash(plan *domain.BillingPlan, deltaBytes, priorBytes, priorChargedFen int64, eventTime time.Time, result *ChargeResult) int64 {
	unit := plan.RoundBytes
	if unit <= 0 {
		unit = 1024 * 1024 // 1 MiB default rounding unit
	}
	units := int64(0)
	if deltaBytes > 0 {
		units = int64(math.Ceil(float64(deltaBytes) / float64(unit)))
	}
	return applyDiscount(units*plan.PriceFen, priorChargedFen, plan, eventTime, result)
}

func applyDiscount(cash, priorChargedFen int64, plan *domain.BillingPlan, eventTime time.Time, result *ChargeResult) int64 {
	if cash <= 0 {
		return 0
	}
	for _, w := range parseDiscounts(plan.DiscountJSON) {
		hour := eventTime.Hour()
		hit := false
		if w.StartHour <= w.EndHour {
			hit = hour >= w.StartHour && hour < w.EndHour
		} else {
			hit = hour >= w.StartHour || hour < w.EndHour // wraps midnight
		}
		if hit && w.DiscountPercent > 0 && w.DiscountPercent <= 100 {
			discount := cash * int64(w.DiscountPercent) / 100
			result.DiscountFen += discount
			cash -= discount
			break
		}
	}
	_ = priorChargedFen
	return cash
}

// Subscribe creates an idempotent subscription and charges the period fee
// once (monthly/daily). It is the only place a period fee is levied.
func (e *Engine) Subscribe(ctx context.Context, accountID, planID int64, start, end time.Time, idempotentKey, source string) (*domain.BillingSubscription, error) {
	if idempotentKey == "" {
		return nil, fmt.Errorf("billing: subscription requires idempotent key")
	}
	var existing domain.BillingSubscription
	err := e.db.WithContext(ctx).Where("idempotent_key = ?", idempotentKey).First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	var plan domain.BillingPlan
	if err := e.db.WithContext(ctx).First(&plan, planID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if plan.ChargeType == "monthly" || plan.ChargeType == "daily" {
		if err := e.Debit(ctx, accountID, "CASH", plan.PriceFen, "subscription",
			fmt.Sprintf("plan:%d", planID), "subscription:"+idempotentKey, "engine"); err != nil {
			return nil, err
		}
	}
	sub := &domain.BillingSubscription{
		ID: common.UUIDint64(), AccountID: accountID, PlanID: planID,
		StartAt: start, EndAt: end, Status: "active", Source: source,
		IdempotentKey: idempotentKey, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := e.db.WithContext(ctx).Create(sub).Error; err != nil {
		return nil, err
	}
	return sub, nil
}
