package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"gorm.io/gorm"
)

func cardHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func testEngine(t *testing.T) (*Engine, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "billing.db")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(
		&domain.BillingAccount{}, &domain.BillingAssetFlow{}, &domain.BillingPlan{},
		&domain.BillingSubscription{}, &domain.BillingSessionUsage{}, &domain.BillingCharge{},
		&domain.BillingOrder{}, &domain.BillingPayment{}, &domain.BillingCard{},
		&domain.BillingCardBatch{},
	))
	return NewEngine(db), db
}

func TestCreditDebitIdempotent(t *testing.T) {
	e, _ := testEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "alice", "Alice", 10000, 0, 0)
	require.NoError(t, err)

	require.NoError(t, e.Credit(ctx, acct.ID, "CASH", 500, "recharge", "r1", "credit-r1", "test"))
	require.NoError(t, e.Credit(ctx, acct.ID, "CASH", 500, "recharge", "r1", "credit-r1", "test"))
	updated, err := e.GetAccount(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, int64(10500), updated.CashFen, "replayed callback must not double-credit")

	require.NoError(t, e.Debit(ctx, acct.ID, "CASH", 300, "charge", "s1", "debit-s1", "engine"))
	require.NoError(t, e.Debit(ctx, acct.ID, "CASH", 300, "charge", "s1", "debit-s1", "engine"))
	updated, err = e.GetAccount(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, int64(10200), updated.CashFen)
	assert.Equal(t, int64(2), updated.Version)
}

func TestGiftFirstAndInsufficient(t *testing.T) {
	e, _ := testEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "bob", "", 0, 0, 100)
	require.NoError(t, err)
	require.NoError(t, e.Credit(ctx, acct.ID, "BYTES", 400, "gift", "g1", "gift-g1", "test"))
	// Credit adds to principal; make the gift balance directly for this test.
	require.NoError(t, e.db.Model(&domain.BillingAccount{}).Where("id = ?", acct.ID).
		Updates(map[string]interface{}{"bytes_quota": 100, "gift_bytes": 400}).Error)

	require.NoError(t, e.Debit(ctx, acct.ID, "BYTES", 350, "charge", "s1", "bytes-s1", "engine"))
	updated, err := e.GetAccount(ctx, "bob")
	require.NoError(t, err)
	assert.Equal(t, int64(100), updated.BytesQuota, "principal untouched while gift bytes remain")
	assert.Equal(t, int64(50), updated.GiftBytes, "gift bytes consumed before principal")

	err = e.Debit(ctx, acct.ID, "BYTES", 200, "charge", "s2", "bytes-s2", "engine")
	assert.ErrorIs(t, err, ErrInsufficient)
}

func TestTimeChargingUsesCumulativeDeltas(t *testing.T) {
	e, db := testEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "time-user", "", 1000, 0, 0)
	require.NoError(t, err)
	plan := domain.BillingPlan{ID: 1, Name: "time", ChargeType: "time", PriceFen: 10, RoundSeconds: 60}
	require.NoError(t, db.Create(&plan).Error)

	t0 := time.Unix(1_700_000_000, 0).UTC()
	res, err := e.ChargeSession(ctx, ChargeRequest{AccountID: acct.ID, SessionID: "sess-A", PlanID: 1, EventTime: t0, Seconds: 100, Bytes: 0})
	require.NoError(t, err)
	assert.Equal(t, int64(20), res.AmountFen, "ceil(100/60)=2 units x 10 fen")
	updated, _ := e.GetAccount(ctx, "time-user")
	assert.Equal(t, int64(980), updated.CashFen)

	// Retransmitted cumulative value must not charge again.
	_, err = e.ChargeSession(ctx, ChargeRequest{AccountID: acct.ID, SessionID: "sess-A", PlanID: 1, EventTime: t0, Seconds: 100, Bytes: 0})
	require.NoError(t, err)
	updated, _ = e.GetAccount(ctx, "time-user")
	assert.Equal(t, int64(980), updated.CashFen)

	// Next interim: only the 80-second delta is charged => ceil(80/60)=2 units.
	_, err = e.ChargeSession(ctx, ChargeRequest{AccountID: acct.ID, SessionID: "sess-A", PlanID: 1, EventTime: t0.Add(5 * time.Minute), Seconds: 180, Bytes: 0})
	require.NoError(t, err)
	updated, _ = e.GetAccount(ctx, "time-user")
	assert.Equal(t, int64(960), updated.CashFen)

	// Out-of-order (older) interim must not charge.
	_, err = e.ChargeSession(ctx, ChargeRequest{AccountID: acct.ID, SessionID: "sess-A", PlanID: 1, EventTime: t0, Seconds: 100, Bytes: 0})
	require.NoError(t, err)
	updated, _ = e.GetAccount(ctx, "time-user")
	assert.Equal(t, int64(960), updated.CashFen)
}

func TestTrafficTieredDiscountAndCap(t *testing.T) {
	e, db := testEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "traffic-user", "", 100000, 0, 1<<35)
	require.NoError(t, err)

	plan := domain.BillingPlan{
		ID: 2, Name: "tiered", ChargeType: "tiered", RoundBytes: 1 << 30,
		StepsJSON:    `[{"up_to_bytes":1073741824,"price_per_gb_fen":100},{"up_to_bytes":2147483648,"price_per_gb_fen":50}]`,
		DiscountJSON: `[{"start_hour":1,"end_hour":5,"discount_percent":50}]`,
	}
	require.NoError(t, db.Create(&plan).Error)
	night := time.Unix(1_700_000_000, 0).UTC()
	// Force event time into the discount window (UTC hour 2).
	night = time.Date(night.Year(), night.Month(), night.Day(), 2, 0, 0, 0, time.UTC)
	const gb = int64(1 << 30)
	res, err := e.ChargeSession(ctx, ChargeRequest{AccountID: acct.ID, SessionID: "sess-B", PlanID: 2, EventTime: night, Bytes: gb, Seconds: 60})
	require.NoError(t, err)
	assert.Equal(t, int64(50), res.AmountFen, "100 fen tier discounted 50% at night")
	updated, _ := e.GetAccount(ctx, "traffic-user")
	assert.Equal(t, int64(100000-50), updated.CashFen)

	// Next 1 GiB falls into the second tier (50 fen, discounted to 25).
	_, err = e.ChargeSession(ctx, ChargeRequest{AccountID: acct.ID, SessionID: "sess-B", PlanID: 2, EventTime: night.Add(time.Hour), Bytes: 2 * gb, Seconds: 120})
	require.NoError(t, err)
	updated, _ = e.GetAccount(ctx, "traffic-user")
	assert.Equal(t, int64(100000-75), updated.CashFen)

	// Capped plan stops charging once the cap is reached.
	capped := domain.BillingPlan{ID: 3, Name: "capped", ChargeType: "capped", PriceFen: 100, RoundBytes: 1 << 30, CapFen: 60}
	require.NoError(t, db.Create(&capped).Error)
	_, err = e.ChargeSession(ctx, ChargeRequest{AccountID: acct.ID, SessionID: "sess-C", PlanID: 3, EventTime: night, Bytes: gb, Seconds: 60})
	require.NoError(t, err)
	_, err = e.ChargeSession(ctx, ChargeRequest{AccountID: acct.ID, SessionID: "sess-C", PlanID: 3, EventTime: night, Bytes: 2 * gb, Seconds: 120})
	require.NoError(t, err)
	updated, _ = e.GetAccount(ctx, "traffic-user")
	assert.Equal(t, int64(100000-75-60), updated.CashFen, "cap fen reached, further usage is not charged")
}

func TestSubscribeMonthlyIdempotent(t *testing.T) {
	e, db := testEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "monthly-user", "", 1000, 0, 0)
	require.NoError(t, err)
	require.NoError(t, db.Create(&domain.BillingPlan{ID: 4, Name: "monthly", ChargeType: "monthly", PriceFen: 300, Status: "enabled"}).Error)
	end := time.Now().UTC().Add(30 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		_, err := e.Subscribe(ctx, acct.ID, 4, time.Now().UTC(), end, "sub-key-1", "test")
		require.NoError(t, err)
	}
	updated, _ := e.GetAccount(ctx, "monthly-user")
	assert.Equal(t, int64(700), updated.CashFen, "monthly fee charged exactly once")
}

func TestDuplicatePaymentCallbacksCreditOnce(t *testing.T) {
	e, _ := testEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "payer", "", 0, 0, 0)
	require.NoError(t, err)
	order, err := e.CreateOrder(ctx, acct.ID, 0, 5000, "wechat", "order-key-1")
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		_, err := e.MarkOrderPaid(ctx, order.OrderNo, "TXN-SAME", "wechat", "{}", true)
		require.NoError(t, err)
	}
	updated, _ := e.GetAccount(ctx, "payer")
	assert.Equal(t, int64(5000), updated.CashFen, "B7: 100 duplicate callbacks => one credit")

	_, err = e.MarkOrderPaid(ctx, order.OrderNo, "TXN-UNSIGNED", "wechat", "{}", false)
	assert.Error(t, err, "unsigned callback must be rejected")
}

func TestCardRedeemAndBillIssue(t *testing.T) {
	e, db := testEngine(t)
	ctx := context.Background()
	acct, err := e.CreateAccount(ctx, "card-user", "", 0, 0, 0)
	require.NoError(t, err)
	require.NoError(t, db.Create(&domain.BillingCardBatch{BatchNo: "B1", FaceValueFen: 2000, AssetType: "CASH", TotalQty: 1}).Error)
	require.NoError(t, db.Create(&domain.BillingCard{CardNo: "C1", BatchNo: "B1", CodeHash: cardHash("1234"), Status: "active"}).Error)
	require.NoError(t, e.RedeemCard(ctx, "C1", "1234", acct.ID))
	assert.Error(t, e.RedeemCard(ctx, "C1", "1234", acct.ID), "card cannot be redeemed twice")
	updated, _ := e.GetAccount(ctx, "card-user")
	assert.Equal(t, int64(2000), updated.CashFen)

	bill, err := e.IssueBill(ctx, acct.ID, 1, "sess-1", "2026-11", 100, 0, 10, false)
	require.NoError(t, err)
	again, err := e.IssueBill(ctx, acct.ID, 1, "sess-1", "2026-11", 100, 0, 10, false)
	require.NoError(t, err)
	assert.Equal(t, bill.BillNo, again.BillNo)
	assert.Equal(t, fmt.Sprintf("BILL-%s-%s", "sess-1", "2026-11"), bill.BillNo)
}
