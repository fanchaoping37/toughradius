package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/pkg/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 账本借贷方向与常用科目编码。银行科目由支付渠道动态生成（BANK_<CHANNEL>）。
const (
	DirectionDebit  = "DEBIT"
	DirectionCredit = "CREDIT"

	AccountAssetCash       = "ASSET_CASH"
	AccountRevenueService  = "REVENUE_SERVICE"
	AccountLiabilityPrepay = "LIABILITY_PREPAY"
	AccountExpenseRefund   = "EXPENSE_REFUND"

	ledgerCurrency = "CNY"
)

// bankAccountCode 把支付渠道映射为银行科目，例如 wechat -> BANK_WECHAT。
func bankAccountCode(channel string) string {
	var b strings.Builder
	b.WriteString("BANK_")
	for _, r := range strings.ToUpper(channel) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// validateLedgerEntries 校验一批分录：EntryNo 必填且批内唯一、金额为正、
// 方向合法，且 DEBIT 合计必须等于 CREDIT 合计。
func validateLedgerEntries(entries []domain.BillingLedgerEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("billing: ledger requires at least one debit and one credit entry")
	}
	var debit, credit int64
	seen := make(map[string]struct{}, len(entries))
	for i := range entries {
		entry := entries[i]
		if entry.EntryNo == "" {
			return fmt.Errorf("billing: ledger entry %d has empty entry_no", i)
		}
		if _, ok := seen[entry.EntryNo]; ok {
			return fmt.Errorf("billing: duplicate entry_no %q in ledger batch", entry.EntryNo)
		}
		seen[entry.EntryNo] = struct{}{}
		if entry.AmountFen <= 0 {
			return fmt.Errorf("billing: ledger entry %q amount must be positive", entry.EntryNo)
		}
		switch entry.Direction {
		case DirectionDebit:
			debit += entry.AmountFen
		case DirectionCredit:
			credit += entry.AmountFen
		default:
			return fmt.Errorf("billing: ledger entry %q has invalid direction %q", entry.EntryNo, entry.Direction)
		}
	}
	if debit != credit {
		return fmt.Errorf("billing: unbalanced ledger: debit %d != credit %d (fen)", debit, credit)
	}
	return nil
}

// postDoubleEntry 在给定事务内写入分录。ON CONFLICT DO NOTHING 使重复的
// EntryNo 成为无操作，保证回调重试不会重复入账。
func postDoubleEntry(tx *gorm.DB, entries []domain.BillingLedgerEntry) error {
	entryNos := make([]string, 0, len(entries))
	for _, entry := range entries {
		entryNos = append(entryNos, entry.EntryNo)
	}
	var existing int64
	if err := tx.Model(&domain.BillingLedgerEntry{}).
		Where("entry_no IN ?", entryNos).Count(&existing).Error; err != nil {
		return err
	}
	if existing == int64(len(entries)) {
		return nil // 整批已入账，重复提交为无操作
	}
	if existing > 0 {
		// 半批重放会让实际落账的借贷不再平衡，明确拒绝。
		return fmt.Errorf("billing: ledger batch partially posted (%d of %d entries already exist)",
			existing, len(entries))
	}
	for _, entry := range entries {
		if entry.Currency == "" {
			entry.Currency = ledgerCurrency
		}
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = time.Now().UTC()
		}
		entry.ID = common.UUIDint64()
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "entry_no"}},
			DoNothing: true,
		}).Create(&entry).Error; err != nil {
			return err
		}
	}
	return nil
}

// PostDoubleEntry 在单个事务内落账。借贷不平、金额非正、EntryNo 缺失或批内
// 重复都会被拒绝；已存在的 EntryNo 被跳过并返回 nil（幂等）。
func (e *Engine) PostDoubleEntry(ctx context.Context, entries []domain.BillingLedgerEntry) error {
	if err := validateLedgerEntries(entries); err != nil {
		return err
	}
	return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return postDoubleEntry(tx, entries)
	})
}

// postRechargeTx writes the recharge double-entry in the caller's transaction.
// Keeping it in the same transaction as the asset credit avoids an account
// balance that can be credited without a matching ledger entry.
func postRechargeTx(tx *gorm.DB, orderNo, paymentNo, channel string, accountID, amountFen int64) error {
	if orderNo == "" || paymentNo == "" || amountFen <= 0 {
		return fmt.Errorf("billing: record recharge requires order, payment and positive amount")
	}
	if channel == "" {
		return fmt.Errorf("billing: record recharge requires channel")
	}
	entryNo := fmt.Sprintf("recharge:%s:%s", orderNo, paymentNo)
	now := time.Now().UTC()
	summary := fmt.Sprintf("recharge %s via %s", orderNo, channel)
	return postDoubleEntry(tx, []domain.BillingLedgerEntry{
		{
			EntryNo: entryNo + ":debit", BizType: "recharge", RefID: orderNo,
			AccountID: accountID, Direction: DirectionDebit,
			AccountCode: bankAccountCode(channel), AmountFen: amountFen,
			Currency: ledgerCurrency, Summary: summary, Operator: "engine", CreatedAt: now,
		},
		{
			EntryNo: entryNo + ":credit", BizType: "recharge", RefID: orderNo,
			AccountID: accountID, Direction: DirectionCredit,
			AccountCode: AccountLiabilityPrepay, AmountFen: amountFen,
			Currency: ledgerCurrency, Summary: summary, Operator: "engine", CreatedAt: now,
		},
	})
}

// postUsageChargeTx writes one usage double-entry in the caller's transaction.
// usageKey must be stable for the specific cumulative-meter reading so that a
// retried Interim-Update cannot create a second revenue entry.
func postUsageChargeTx(tx *gorm.DB, accountID int64, sessionID, usageKey string, amountFen int64) error {
	if sessionID == "" || usageKey == "" || amountFen <= 0 {
		return nil
	}
	entryNo := "usage:" + usageKey
	now := time.Now().UTC()
	summary := "usage charge " + sessionID
	return postDoubleEntry(tx, []domain.BillingLedgerEntry{
		{
			EntryNo: entryNo + ":debit", BizType: "usage", RefID: sessionID,
			AccountID: accountID, Direction: DirectionDebit,
			AccountCode: AccountLiabilityPrepay, AmountFen: amountFen,
			Currency: ledgerCurrency, Summary: summary, Operator: "engine", CreatedAt: now,
		},
		{
			EntryNo: entryNo + ":credit", BizType: "usage", RefID: sessionID,
			AccountID: accountID, Direction: DirectionCredit,
			AccountCode: AccountRevenueService, AmountFen: amountFen,
			Currency: ledgerCurrency, Summary: summary, Operator: "engine", CreatedAt: now,
		},
	})
}

// RecordRecharge 在 MarkOrderPaid 成功后调用，记录充值双分录：
// 借 BANK_<CHANNEL> / 贷 LIABILITY_PREPAY。EntryNo 由订单号与支付单号组成，
// 因此相同回调重放不会产生重复分录。
func (e *Engine) RecordRecharge(ctx context.Context, orderNo, paymentNo, channel string, amountFen int64) error {
	if orderNo == "" || paymentNo == "" || amountFen <= 0 {
		return fmt.Errorf("billing: record recharge requires order, payment and positive amount")
	}
	var order domain.BillingOrder
	if err := e.db.WithContext(ctx).Where("order_no = ?", orderNo).First(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if channel == "" {
		channel = order.Channel
	}
	if channel == "" {
		return fmt.Errorf("billing: record recharge requires channel")
	}
	entryNo := fmt.Sprintf("recharge:%s:%s", orderNo, paymentNo)
	now := time.Now().UTC()
	summary := fmt.Sprintf("recharge %s via %s", orderNo, channel)
	return e.PostDoubleEntry(ctx, []domain.BillingLedgerEntry{
		{
			EntryNo: entryNo + ":debit", BizType: "recharge", RefID: orderNo,
			AccountID: order.AccountID, Direction: DirectionDebit,
			AccountCode: bankAccountCode(channel), AmountFen: amountFen,
			Currency: ledgerCurrency, Summary: summary, Operator: "engine", CreatedAt: now,
		},
		{
			EntryNo: entryNo + ":credit", BizType: "recharge", RefID: orderNo,
			AccountID: order.AccountID, Direction: DirectionCredit,
			AccountCode: AccountLiabilityPrepay, AmountFen: amountFen,
			Currency: ledgerCurrency, Summary: summary, Operator: "engine", CreatedAt: now,
		},
	})
}

// RecordUsageCharge 记录消费双分录：借 LIABILITY_PREPAY / 贷 REVENUE_SERVICE。
// 幂等键由 sessionID 派生，同一会话重复使用不会重复确认收入。
func (e *Engine) RecordUsageCharge(ctx context.Context, accountID int64, sessionID string, amountFen int64) error {
	if sessionID == "" || amountFen <= 0 {
		return fmt.Errorf("billing: usage charge requires session and positive amount")
	}
	entryNo := "usage:" + sessionID
	now := time.Now().UTC()
	summary := "usage charge " + sessionID
	return e.PostDoubleEntry(ctx, []domain.BillingLedgerEntry{
		{
			EntryNo: entryNo + ":debit", BizType: "usage", RefID: sessionID,
			AccountID: accountID, Direction: DirectionDebit,
			AccountCode: AccountLiabilityPrepay, AmountFen: amountFen,
			Currency: ledgerCurrency, Summary: summary, Operator: "engine", CreatedAt: now,
		},
		{
			EntryNo: entryNo + ":credit", BizType: "usage", RefID: sessionID,
			AccountID: accountID, Direction: DirectionCredit,
			AccountCode: AccountRevenueService, AmountFen: amountFen,
			Currency: ledgerCurrency, Summary: summary, Operator: "engine", CreatedAt: now,
		},
	})
}

// RefundOrder 在单个事务内完成退款：校验支付单与可退金额、按 idempotentKey
// 幂等、冲回账户现金、写退款单与反向分录（借 EXPENSE_REFUND / 贷 BANK_<CHANNEL>），
// 并把支付单和订单标记为 refunded。现金扣回复用 moveAsset 的 version CAS，
// 余额不足时返回 ErrInsufficient 并整体回滚，绝不产生负数余额。
func (e *Engine) RefundOrder(ctx context.Context, orderNo string, amountFen int64, reason, operator, idempotentKey string) (*domain.BillingRefund, error) {
	if orderNo == "" || amountFen <= 0 || idempotentKey == "" {
		return nil, fmt.Errorf("billing: refund requires order, positive amount and idempotent key")
	}
	var result *domain.BillingRefund
	err := e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing domain.BillingRefund
		if err := tx.Where("idempotent_key = ?", idempotentKey).First(&existing).Error; err == nil {
			result = &existing
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var payments []domain.BillingPayment
		if err := tx.Where("order_no = ? AND status = ?", orderNo, "paid").
			Order("created_at ASC").Find(&payments).Error; err != nil {
			return err
		}
		if len(payments) == 0 {
			return ErrNotFound
		}
		var order domain.BillingOrder
		if err := tx.Where("order_no = ?", orderNo).First(&order).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}

		var paidFen int64
		for i := range payments {
			paidFen += payments[i].AmountFen
		}
		var refundedFen int64
		if err := tx.Model(&domain.BillingRefund{}).
			Where("order_no = ? AND status = ?", orderNo, "succeeded").
			Select("COALESCE(SUM(amount_fen), 0)").Scan(&refundedFen).Error; err != nil {
			return err
		}
		if amountFen > paidFen-refundedFen {
			return fmt.Errorf("billing: refund %d fen exceeds remaining paid amount %d fen of order %s",
				amountFen, paidFen-refundedFen, orderNo)
		}

		// 不能在这里调用 e.Debit：它会另开事务。moveAsset 接收当前 tx，
		// 在同一事务内完成 version CAS 扣回与 BillingAssetFlow 幂等写入。
		if err := moveAsset(tx, order.AccountID, "CASH", "OUT", amountFen,
			"refund", orderNo, "refund:"+idempotentKey, operator); err != nil {
			return err
		}

		now := time.Now().UTC()
		created := &domain.BillingRefund{
			ID:            common.UUIDint64(),
			RefundNo:      fmt.Sprintf("RF%d", common.UUIDint64()),
			OrderNo:       orderNo,
			PaymentNo:     payments[0].PaymentNo,
			AmountFen:     amountFen,
			Reason:        reason,
			Status:        "succeeded",
			IdempotentKey: idempotentKey,
			Operator:      operator,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := tx.Create(created).Error; err != nil {
			return err
		}

		entryNo := "refund:" + idempotentKey
		summary := "refund order " + orderNo
		if reason != "" {
			summary += ": " + reason
		}
		if err := postDoubleEntry(tx, []domain.BillingLedgerEntry{
			{
				EntryNo: entryNo + ":debit", BizType: "refund", RefID: orderNo,
				AccountID: order.AccountID, Direction: DirectionDebit,
				AccountCode: AccountExpenseRefund, AmountFen: amountFen,
				Currency: ledgerCurrency, Summary: summary, Operator: operator, CreatedAt: now,
			},
			{
				EntryNo: entryNo + ":credit", BizType: "refund", RefID: orderNo,
				AccountID: order.AccountID, Direction: DirectionCredit,
				AccountCode: bankAccountCode(payments[0].Channel), AmountFen: amountFen,
				Currency: ledgerCurrency, Summary: summary, Operator: operator, CreatedAt: now,
			},
		}); err != nil {
			return err
		}

		if err := tx.Model(&domain.BillingPayment{}).
			Where("order_no = ? AND status = ?", orderNo, "paid").
			Update("status", "refunded").Error; err != nil {
			return err
		}
		if err := tx.Model(&domain.BillingOrder{}).
			Where("order_no = ?", orderNo).
			Update("status", "refunded").Error; err != nil {
			return err
		}
		result = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListLedgerEntries 按账户与业务引用查询账本分录，按 ID 倒序返回，limit <= 0 时取 100 条。
func (e *Engine) ListLedgerEntries(ctx context.Context, accountID int64, refID string, limit int) ([]domain.BillingLedgerEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	query := e.db.WithContext(ctx).Model(&domain.BillingLedgerEntry{})
	if accountID != 0 {
		query = query.Where("account_id = ?", accountID)
	}
	if refID != "" {
		query = query.Where("ref_id = ?", refID)
	}
	entries := make([]domain.BillingLedgerEntry, 0)
	if err := query.Order("id DESC").Limit(limit).Find(&entries).Error; err != nil {
		return nil, err
	}
	return entries, nil
}
