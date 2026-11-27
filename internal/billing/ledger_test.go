package billing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"gorm.io/gorm"
)

// testLedgerEngine 复用已有测试的 SQLite 迁移与清理，并补上账本两张新表。
func testLedgerEngine(t *testing.T) (*Engine, *gorm.DB) {
	t.Helper()
	e, db := testEngine(t)
	require.NoError(t, db.AutoMigrate(&domain.BillingLedgerEntry{}, &domain.BillingRefund{}))
	return e, db
}

func TestPostDoubleEntryRejectsInvalidBatches(t *testing.T) {
	e, db := testLedgerEngine(t)
	ctx := context.Background()
	validBatch := func() []domain.BillingLedgerEntry {
		return []domain.BillingLedgerEntry{
			{EntryNo: "manual:1:debit", Direction: DirectionDebit, AccountCode: AccountAssetCash, AmountFen: 100},
			{EntryNo: "manual:1:credit", Direction: DirectionCredit, AccountCode: AccountRevenueService, AmountFen: 100},
		}
	}

	// 借贷不平
	unbalanced := validBatch()
	unbalanced[1].AmountFen = 90
	assert.Error(t, e.PostDoubleEntry(ctx, unbalanced))

	// 金额非正
	zeroAmount := validBatch()
	zeroAmount[0].AmountFen = 0
	assert.Error(t, e.PostDoubleEntry(ctx, zeroAmount))

	// EntryNo 缺失
	missingNo := validBatch()
	missingNo[0].EntryNo = ""
	assert.Error(t, e.PostDoubleEntry(ctx, missingNo))

	// 方向非法
	badDirection := validBatch()
	badDirection[0].Direction = "SIDEWAYS"
	assert.Error(t, e.PostDoubleEntry(ctx, badDirection))

	// 空批次
	assert.Error(t, e.PostDoubleEntry(ctx, nil))

	// 批内 EntryNo 重复
	duplicateNo := validBatch()
	duplicateNo[1].EntryNo = duplicateNo[0].EntryNo
	assert.Error(t, e.PostDoubleEntry(ctx, duplicateNo))

	var count int64
	require.NoError(t, db.Model(&domain.BillingLedgerEntry{}).Count(&count).Error)
	assert.Zero(t, count, "被拒绝的批次不得写入任何分录")
}

func TestPostDoubleEntryIdempotent(t *testing.T) {
	e, db := testLedgerEngine(t)
	ctx := context.Background()
	entries := []domain.BillingLedgerEntry{
		{EntryNo: "manual:2:debit", BizType: "adjust", RefID: "manual-2", AccountID: 7, Direction: DirectionDebit, AccountCode: AccountAssetCash, AmountFen: 300, Operator: "tester"},
		{EntryNo: "manual:2:credit", BizType: "adjust", RefID: "manual-2", AccountID: 7, Direction: DirectionCredit, AccountCode: AccountRevenueService, AmountFen: 300, Operator: "tester"},
	}
	require.NoError(t, e.PostDoubleEntry(ctx, entries))
	require.NoError(t, e.PostDoubleEntry(ctx, entries))

	var count int64
	require.NoError(t, db.Model(&domain.BillingLedgerEntry{}).Count(&count).Error)
	assert.Equal(t, int64(2), count, "重复入账不得产生新分录")

	got, err := e.ListLedgerEntries(ctx, 7, "manual-2", 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, ledgerCurrency, got[0].Currency)
}

func TestRecordRechargeAndUsageChargeEntries(t *testing.T) {
	e, db := testLedgerEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "ledger-user", "", 0, 0, 0)
	require.NoError(t, err)
	order, err := e.CreateOrder(ctx, acct.ID, 0, 5000, "wechat", "ledger-order-key")
	require.NoError(t, err)
	_, err = e.MarkOrderPaid(ctx, order.OrderNo, "TXN-LEDGER-1", "wechat", "{}", true)
	require.NoError(t, err)
	var payment domain.BillingPayment
	require.NoError(t, db.Where("order_no = ?", order.OrderNo).First(&payment).Error)

	require.NoError(t, e.RecordRecharge(ctx, order.OrderNo, payment.PaymentNo, "wechat", 5000))
	require.NoError(t, e.RecordRecharge(ctx, order.OrderNo, payment.PaymentNo, "wechat", 5000))
	rechargeEntries, err := e.ListLedgerEntries(ctx, acct.ID, order.OrderNo, 10)
	require.NoError(t, err)
	require.Len(t, rechargeEntries, 2, "重复回调只应写入一对充值分录")
	rechargeByCode := map[string]domain.BillingLedgerEntry{}
	for _, entry := range rechargeEntries {
		rechargeByCode[entry.AccountCode] = entry
	}
	assert.Equal(t, DirectionDebit, rechargeByCode["BANK_WECHAT"].Direction)
	assert.Equal(t, int64(5000), rechargeByCode["BANK_WECHAT"].AmountFen)
	assert.Equal(t, DirectionCredit, rechargeByCode[AccountLiabilityPrepay].Direction)
	assert.Equal(t, order.OrderNo, rechargeByCode[AccountLiabilityPrepay].RefID)

	require.NoError(t, e.RecordUsageCharge(ctx, acct.ID, "sess-ledger-1", 1200))
	require.NoError(t, e.RecordUsageCharge(ctx, acct.ID, "sess-ledger-1", 1200))
	usageEntries, err := e.ListLedgerEntries(ctx, acct.ID, "sess-ledger-1", 10)
	require.NoError(t, err)
	require.Len(t, usageEntries, 2, "同一会话重复记账不得产生新分录")
	usageByCode := map[string]domain.BillingLedgerEntry{}
	for _, entry := range usageEntries {
		usageByCode[entry.AccountCode] = entry
	}
	assert.Equal(t, DirectionDebit, usageByCode[AccountLiabilityPrepay].Direction)
	assert.Equal(t, DirectionCredit, usageByCode[AccountRevenueService].Direction)
	assert.Equal(t, int64(1200), usageByCode[AccountRevenueService].AmountFen)

	all, err := e.ListLedgerEntries(ctx, acct.ID, "", 10)
	require.NoError(t, err)
	assert.Len(t, all, 4)
	limited, err := e.ListLedgerEntries(ctx, acct.ID, "", 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)
}

func TestRefundOrderSuccess(t *testing.T) {
	e, db := testLedgerEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "refund-user", "", 0, 0, 0)
	require.NoError(t, err)
	order, err := e.CreateOrder(ctx, acct.ID, 0, 5000, "wechat", "refund-order-key")
	require.NoError(t, err)
	_, err = e.MarkOrderPaid(ctx, order.OrderNo, "TXN-REFUND-1", "wechat", "{}", true)
	require.NoError(t, err)
	var payment domain.BillingPayment
	require.NoError(t, db.Where("order_no = ?", order.OrderNo).First(&payment).Error)
	require.NoError(t, e.RecordRecharge(ctx, order.OrderNo, payment.PaymentNo, "wechat", 5000))

	refund, err := e.RefundOrder(ctx, order.OrderNo, 5000, "user request", "admin", "refund-key-1")
	require.NoError(t, err)
	assert.Equal(t, order.OrderNo, refund.OrderNo)
	assert.Equal(t, payment.PaymentNo, refund.PaymentNo)
	assert.Equal(t, int64(5000), refund.AmountFen)
	assert.Equal(t, "user request", refund.Reason)
	assert.Equal(t, "admin", refund.Operator)
	assert.Equal(t, "succeeded", refund.Status)
	assert.NotEmpty(t, refund.RefundNo)

	var updated domain.BillingAccount
	require.NoError(t, db.First(&updated, acct.ID).Error)
	assert.Equal(t, int64(0), updated.CashFen, "退款应全额冲回账户现金")
	var paid domain.BillingPayment
	require.NoError(t, db.Where("payment_no = ?", payment.PaymentNo).First(&paid).Error)
	assert.Equal(t, "refunded", paid.Status)
	var refreshedOrder domain.BillingOrder
	require.NoError(t, db.Where("order_no = ?", order.OrderNo).First(&refreshedOrder).Error)
	assert.Equal(t, "refunded", refreshedOrder.Status)

	entries, err := e.ListLedgerEntries(ctx, acct.ID, order.OrderNo, 10)
	require.NoError(t, err)
	require.Len(t, entries, 4, "充值和退款各一对分录")
	var refundDebit, refundCredit bool
	for _, entry := range entries {
		if entry.BizType != "refund" {
			continue
		}
		assert.Equal(t, int64(5000), entry.AmountFen)
		switch entry.AccountCode {
		case AccountExpenseRefund:
			refundDebit = entry.Direction == DirectionDebit
		case "BANK_WECHAT":
			refundCredit = entry.Direction == DirectionCredit
		}
	}
	assert.True(t, refundDebit, "退款借方应为 EXPENSE_REFUND")
	assert.True(t, refundCredit, "退款贷方应为 BANK_WECHAT")

	var flows int64
	require.NoError(t, db.Model(&domain.BillingAssetFlow{}).
		Where("idempotent_key = ?", "refund:refund-key-1").Count(&flows).Error)
	assert.Equal(t, int64(1), flows, "退款应写入一条幂等的现金流水")
}

func TestRefundOrderInsufficientBalance(t *testing.T) {
	e, db := testLedgerEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "spent-user", "", 0, 0, 0)
	require.NoError(t, err)
	order, err := e.CreateOrder(ctx, acct.ID, 0, 5000, "wechat", "spent-order-key")
	require.NoError(t, err)
	_, err = e.MarkOrderPaid(ctx, order.OrderNo, "TXN-SPENT-1", "wechat", "{}", true)
	require.NoError(t, err)
	// 模拟已消费：仅剩 100 分，无法全额退款。
	require.NoError(t, e.Debit(ctx, acct.ID, "CASH", 4900, "session_charge", "sess-spent", "spent-consume", "engine"))

	refund, err := e.RefundOrder(ctx, order.OrderNo, 5000, "user request", "admin", "refund-key-insufficient")
	assert.Nil(t, refund)
	assert.ErrorIs(t, err, ErrInsufficient)

	var updated domain.BillingAccount
	require.NoError(t, db.First(&updated, acct.ID).Error)
	assert.Equal(t, int64(100), updated.CashFen, "退款失败不得产生负余额或部分扣回")

	var refunds int64
	require.NoError(t, db.Model(&domain.BillingRefund{}).Count(&refunds).Error)
	assert.Zero(t, refunds, "退款失败必须整体回滚")
	var payment domain.BillingPayment
	require.NoError(t, db.Where("order_no = ?", order.OrderNo).First(&payment).Error)
	assert.Equal(t, "paid", payment.Status, "失败退款不得改动支付单状态")
	var refreshedOrder domain.BillingOrder
	require.NoError(t, db.Where("order_no = ?", order.OrderNo).First(&refreshedOrder).Error)
	assert.Equal(t, "paid", refreshedOrder.Status)
	var entries int64
	require.NoError(t, db.Model(&domain.BillingLedgerEntry{}).
		Where("biz_type = ?", "refund").Count(&entries).Error)
	assert.Zero(t, entries, "失败退款不得留下分录")
}

func TestRefundOrderIdempotent(t *testing.T) {
	e, db := testLedgerEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "idem-refund-user", "", 0, 0, 0)
	require.NoError(t, err)
	order, err := e.CreateOrder(ctx, acct.ID, 0, 3000, "alipay", "idem-refund-order")
	require.NoError(t, err)
	_, err = e.MarkOrderPaid(ctx, order.OrderNo, "TXN-IDEM-1", "alipay", "{}", true)
	require.NoError(t, err)

	first, err := e.RefundOrder(ctx, order.OrderNo, 3000, "duplicate", "admin", "refund-key-idem")
	require.NoError(t, err)
	second, err := e.RefundOrder(ctx, order.OrderNo, 3000, "duplicate", "admin", "refund-key-idem")
	require.NoError(t, err)
	assert.Equal(t, first.RefundNo, second.RefundNo, "重复退款应返回同一张退款单")

	var refunds int64
	require.NoError(t, db.Model(&domain.BillingRefund{}).Count(&refunds).Error)
	assert.Equal(t, int64(1), refunds)
	var flows int64
	require.NoError(t, db.Model(&domain.BillingAssetFlow{}).
		Where("idempotent_key = ?", "refund:refund-key-idem").Count(&flows).Error)
	assert.Equal(t, int64(1), flows)
	var updated domain.BillingAccount
	require.NoError(t, db.First(&updated, acct.ID).Error)
	assert.Equal(t, int64(0), updated.CashFen, "重复退款不得二次扣回")

	entries, err := e.ListLedgerEntries(ctx, acct.ID, order.OrderNo, 10)
	require.NoError(t, err)
	refundEntries := 0
	var alipayCredited bool
	for _, entry := range entries {
		if entry.BizType != "refund" {
			continue // recharge double-entry is recorded separately
		}
		refundEntries++
		if entry.AccountCode == "BANK_ALIPAY" {
			alipayCredited = entry.Direction == DirectionCredit
		}
	}
	require.Equal(t, 2, refundEntries, "重复退款不得生成第二组分录")
	assert.True(t, alipayCredited, "退款贷方应为 BANK_ALIPAY 且只有一条")
}

func TestRefundOrderValidation(t *testing.T) {
	e, db := testLedgerEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "refund-validation", "", 1000, 0, 0)
	require.NoError(t, err)
	order, err := e.CreateOrder(ctx, acct.ID, 0, 1000, "wechat", "refund-validation-order")
	require.NoError(t, err)

	_, err = e.RefundOrder(ctx, "NO-SUCH-ORDER", 100, "", "admin", "refund-missing")
	assert.ErrorIs(t, err, ErrNotFound, "支付单不存在时应拒绝退款")

	_, err = e.MarkOrderPaid(ctx, order.OrderNo, "TXN-VALID-1", "wechat", "{}", true)
	require.NoError(t, err)
	_, err = e.RefundOrder(ctx, order.OrderNo, 1001, "too much", "admin", "refund-too-much")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")

	var updated domain.BillingAccount
	require.NoError(t, db.First(&updated, acct.ID).Error)
	assert.Equal(t, int64(2000), updated.CashFen, "被拒绝的退款不得改动余额")
}
