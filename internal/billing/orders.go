package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/pkg/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CreateOrder creates a recharge/subscription order. Replaying the same
// idempotentKey returns the original order (SRS §12.5).
func (e *Engine) CreateOrder(ctx context.Context, accountID, planID, amountFen int64, channel, idempotentKey string) (*domain.BillingOrder, error) {
	if idempotentKey == "" || amountFen <= 0 {
		return nil, fmt.Errorf("billing: order requires positive amount and idempotent key")
	}
	var existing domain.BillingOrder
	err := e.db.WithContext(ctx).Where("idempotent_key = ?", idempotentKey).First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	order := &domain.BillingOrder{
		OrderNo: fmt.Sprintf("TR%d", common.UUIDint64()), AccountID: accountID,
		PlanID: planID, AmountFen: amountFen, Status: "pending", Channel: channel,
		IdempotentKey: idempotentKey, ExpiredAt: time.Now().UTC().Add(30 * time.Minute),
		CreatedAt: time.Now().UTC(),
	}
	if err := e.db.WithContext(ctx).Create(order).Error; err != nil {
		return nil, err
	}
	return order, nil
}

// MarkOrderPaid records a payment-channel callback and credits the account.
// Both the channel transaction id and the idempotent key protect against
// duplicate settlement — B7 acceptance requires 100 duplicate callbacks to
// produce exactly one credit.
func (e *Engine) MarkOrderPaid(ctx context.Context, orderNo, channelTxnID, channel, rawNotify string, signVerified bool) (*domain.BillingOrder, error) {
	if !signVerified {
		return nil, fmt.Errorf("billing: payment callback signature not verified")
	}
	if orderNo == "" || channelTxnID == "" {
		return nil, fmt.Errorf("billing: payment callback requires order and transaction id")
	}
	var order domain.BillingOrder
	if err := e.db.WithContext(ctx).Where("order_no = ?", orderNo).First(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var existing domain.BillingPayment
	err := e.db.WithContext(ctx).Where("channel_txn_id = ?", channelTxnID).First(&existing).Error
	if err == nil {
		return &order, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if channel == "" {
		channel = order.Channel
	}
	if channel == "" {
		return nil, fmt.Errorf("billing: payment callback requires channel")
	}

	payment := &domain.BillingPayment{
		PaymentNo: fmt.Sprintf("PAY%d", common.UUIDint64()), OrderNo: orderNo,
		Channel: channel, ChannelTxnID: channelTxnID, AmountFen: order.AmountFen,
		RawNotify: rawNotify, SignVerified: true, Status: "paid",
		NotifiedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	err = e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current domain.BillingOrder
		if err := tx.Where("order_no = ?", orderNo).First(&current).Error; err != nil {
			return err
		}
		if current.Status == "paid" || current.Status == "refunded" {
			order = current
			return nil
		}
		if current.Status != "pending" {
			return fmt.Errorf("billing: order %s is %s, cannot settle", orderNo, current.Status)
		}
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "channel_txn_id"}},
			DoNothing: true,
		}).Create(payment)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// A concurrent callback with the same transaction id won the race.
			order = current
			return nil
		}
		if err := moveAsset(tx, current.AccountID, "CASH", "IN", current.AmountFen,
			"recharge", orderNo, "payment:"+payment.PaymentNo, channel); err != nil {
			return err
		}
		// The ledger entry is committed together with the asset movement so a
		// balance can never exist without its double-entry counterpart.
		if err := postRechargeTx(tx, orderNo, payment.PaymentNo, channel,
			current.AccountID, current.AmountFen); err != nil {
			return err
		}
		if err := tx.Model(&domain.BillingOrder{}).
			Where("order_no = ? AND status = ?", orderNo, "pending").
			Updates(map[string]interface{}{"status": "paid", "paid_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		order = current
		order.Status = "paid"
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// RedeemCard atomically consumes a prepaid card and credits its face value.
func (e *Engine) RedeemCard(ctx context.Context, cardNo, code string, accountID int64) error {
	sum := sha256.Sum256([]byte(code))
	hash := hex.EncodeToString(sum[:])
	var card domain.BillingCard
	if err := e.db.WithContext(ctx).Where("card_no = ?", cardNo).First(&card).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if card.CodeHash != hash {
		return fmt.Errorf("billing: invalid card code")
	}
	if card.Status != "active" || (!card.ExpireAt.IsZero() && time.Now().UTC().After(card.ExpireAt)) {
		return fmt.Errorf("billing: card unavailable")
	}
	var batch domain.BillingCardBatch
	if err := e.db.WithContext(ctx).Where("batch_no = ?", card.BatchNo).First(&batch).Error; err != nil {
		return err
	}
	res := e.db.WithContext(ctx).Model(&domain.BillingCard{}).
		Where("card_no = ? AND status = ?", cardNo, "active").
		Updates(map[string]interface{}{"status": "used", "used_by": accountID, "used_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("billing: card already redeemed")
	}
	return e.Credit(ctx, accountID, batch.AssetType, batch.FaceValueFen, "card_redeem",
		cardNo, "card:"+cardNo, "card")
}

// IssueBill writes a bill line for a completed usage period. BillNo is unique
// so repeated closing of the same session cannot duplicate the invoice.
func (e *Engine) IssueBill(ctx context.Context, accountID, planID int64, sessionID, period string, usageAmount, discountFen, amountFen int64, capped bool) (*domain.BillingCharge, error) {
	billNo := fmt.Sprintf("BILL-%s-%s", sessionID, period)
	var existing domain.BillingCharge
	err := e.db.WithContext(ctx).Where("bill_no = ?", billNo).First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	charge := &domain.BillingCharge{
		ID: common.UUIDint64(), AccountID: accountID, SessionID: sessionID,
		PlanID: planID, BillNo: billNo, Period: period, UsageAmount: usageAmount,
		DiscountFen: discountFen, AmountFen: amountFen, Capped: capped,
		Status: "issued", CreatedAt: time.Now().UTC(),
	}
	if err := e.db.WithContext(ctx).Create(charge).Error; err != nil {
		return nil, err
	}
	return charge, nil
}
