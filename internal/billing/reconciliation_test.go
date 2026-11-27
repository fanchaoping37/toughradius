package billing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"gorm.io/gorm"
)

// testReconcileEngine reuses the SQLite fixture from engine_test.go and adds
// the RADIUS accounting tables needed by reconciliation.
func testReconcileEngine(t *testing.T) (*Engine, *gorm.DB) {
	t.Helper()
	engine, db := testEngine(t)
	require.NoError(t, db.AutoMigrate(
		&domain.RadiusOnline{},
		&domain.RadiusAccounting{},
		&domain.RadiusAcctRecord{},
		&domain.RadiusAcctDedup{},
	))
	return engine, db
}

func insertOnlineSession(t *testing.T, db *gorm.DB, id int64, sessionID, nasAddr string, start time.Time) {
	t.Helper()
	require.NoError(t, db.Create(&domain.RadiusOnline{
		ID:                id,
		Username:          "recon-user",
		AcctSessionId:     sessionID,
		NasAddr:           nasAddr,
		AcctStartTime:     start,
		EventTimestamp:    start,
		AcctSessionTime:   100,
		AcctInputTotal:    1000,
		AcctOutputTotal:   2000,
		AcctInputPackets:  10,
		AcctOutputPackets: 20,
		LastUpdate:        start,
	}).Error)
}

func countRows(t *testing.T, db *gorm.DB, model interface{}) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(model).Count(&n).Error)
	return n
}

func TestReconcileSessionsSkipsExistingStop(t *testing.T) {
	e, db := testReconcileEngine(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-2 * time.Hour)

	// Stop in the historical ledger.
	insertOnlineSession(t, db, 1, "s-ledger-stop", "10.0.0.1", start)
	require.NoError(t, db.Create(&domain.RadiusAccounting{
		ID: 11, AcctSessionId: "s-ledger-stop", Username: "recon-user",
		AcctStatusType: acctStatusStop, AcctStartTime: start,
		AcctStopTime: start.Add(time.Hour), DedupKey: "stop-ledger",
	}).Error)

	// Stop only in the raw packet ledger.
	insertOnlineSession(t, db, 2, "s-raw-stop", "10.0.0.1", start)
	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 21, AcctSessionId: "s-raw-stop", Username: "recon-user",
		StatusType: acctStatusStop, EventTimestamp: start.Add(time.Hour),
		DedupKey: "stop-raw", Consumed: true, CreatedAt: start.Add(time.Hour),
	}).Error)

	res, err := e.ReconcileSessions(ctx, time.Now().UTC(), 0)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Scanned)
	assert.Equal(t, 2, res.Skipped)
	assert.Equal(t, 0, res.ClosedMissingStop)
	assert.Equal(t, int64(2), countRows(t, db, &domain.RadiusOnline{}))
	assert.Empty(t, res.Errors)
}

func TestReconcileSessionsClosesMissingStop(t *testing.T) {
	e, db := testReconcileEngine(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := start.Add(90 * time.Minute)

	insertOnlineSession(t, db, 1, "s-missing", "10.0.0.1", start)
	// Start row carries lower counters: reconciliation must keep the max.
	require.NoError(t, db.Create(&domain.RadiusAccounting{
		ID: 11, AcctSessionId: "s-missing", Username: "recon-user",
		AcctStatusType: acctStatusStart, AcctStartTime: start,
		AcctSessionTime: 50, AcctInputTotal: 500, AcctOutputTotal: 700,
		DedupKey: "start-s-missing",
	}).Error)

	res, err := e.ReconcileSessions(ctx, cutoff, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned)
	assert.Equal(t, 1, res.ClosedMissingStop)
	assert.Equal(t, 0, res.Skipped)

	var online domain.RadiusOnline
	assert.ErrorIs(t, db.Where("acct_session_id = ?", "s-missing").First(&online).Error, gorm.ErrRecordNotFound,
		"stale session must no longer be online")

	var cdr domain.RadiusAccounting
	require.NoError(t, db.Where("acct_session_id = ?", "s-missing").First(&cdr).Error)
	assert.Equal(t, acctStatusStop, cdr.AcctStatusType)
	assert.Equal(t, cutoff.UTC().Unix(), cdr.AcctStopTime.UTC().Unix())
	assert.Equal(t, 100, cdr.AcctSessionTime)
	assert.Equal(t, int64(1000), cdr.AcctInputTotal, "original traffic values must be preserved")
	assert.Equal(t, int64(2000), cdr.AcctOutputTotal)
	assert.Equal(t, int64(1), countRows(t, db, &domain.RadiusAccounting{}))

	var marker domain.RadiusAcctRecord
	require.NoError(t, db.Where("dedup_key = ?", "recon-stop:s-missing").First(&marker).Error)
	assert.Equal(t, acctStatusStop, marker.StatusType)
	assert.True(t, marker.Consumed, "synthetic marker must not be replayed as a dead letter")
	assert.Equal(t, int64(1000), marker.InputOctets)
}

func TestReconcileSessionsCreatesStandaloneCDR(t *testing.T) {
	e, db := testReconcileEngine(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-3 * time.Hour)

	insertOnlineSession(t, db, 1, "s-no-start", "10.0.0.9", start)

	res, err := e.ReconcileSessions(ctx, time.Now().UTC(), 0)
	require.NoError(t, err)
	assert.Equal(t, 1, res.ClosedMissingStop)

	var cdr domain.RadiusAccounting
	require.NoError(t, db.Where("acct_session_id = ?", "s-no-start").First(&cdr).Error)
	assert.Equal(t, acctStatusStop, cdr.AcctStatusType)
	assert.Equal(t, "10.0.0.9", cdr.NasAddr)
	assert.Equal(t, int64(2000), cdr.AcctOutputTotal)
	assert.Equal(t, "recon-stop:s-no-start", cdr.DedupKey)
}

func TestDetectOutOfOrder(t *testing.T) {
	e, db := testReconcileEngine(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-time.Hour)

	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 1, AcctSessionId: "s-ooo", StatusType: acctStatusStart,
		EventTimestamp: t0, AcctSessionTime: 100, InputOctets: 1000, OutputOctets: 2000,
		DedupKey: "ooo-1", CreatedAt: t0,
	}).Error)
	// Arrives later but reports an older event time and lower counters.
	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 2, AcctSessionId: "s-ooo", StatusType: acctStatusInterim,
		EventTimestamp: t0.Add(-time.Minute), AcctSessionTime: 50,
		InputOctets: 500, OutputOctets: 2000,
		DedupKey: "ooo-2", CreatedAt: t0.Add(time.Minute),
	}).Error)

	outOfOrder, err := e.DetectOutOfOrder(ctx, "s-ooo")
	require.NoError(t, err)
	assert.True(t, outOfOrder)

	// Counters that only advance are not out of order, even if one packet has
	// no Event-Timestamp.
	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 3, AcctSessionId: "s-ok", StatusType: acctStatusStart,
		AcctSessionTime: 10, InputOctets: 100, OutputOctets: 100,
		DedupKey: "ok-1", CreatedAt: t0,
	}).Error)
	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 4, AcctSessionId: "s-ok", StatusType: acctStatusInterim,
		EventTimestamp: t0.Add(time.Minute), AcctSessionTime: 20,
		InputOctets: 200, OutputOctets: 200,
		DedupKey: "ok-2", CreatedAt: t0.Add(time.Minute),
	}).Error)
	outOfOrder, err = e.DetectOutOfOrder(ctx, "s-ok")
	require.NoError(t, err)
	assert.False(t, outOfOrder)
}

func TestReconcileSessionsIsIdempotent(t *testing.T) {
	e, db := testReconcileEngine(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-2 * time.Hour)
	insertOnlineSession(t, db, 1, "s-idem", "10.0.0.1", start)

	first, err := e.ReconcileSessions(ctx, time.Now().UTC(), 0)
	require.NoError(t, err)
	assert.Equal(t, 1, first.ClosedMissingStop)

	second, err := e.ReconcileSessions(ctx, time.Now().UTC(), 0)
	require.NoError(t, err)
	assert.Equal(t, 0, second.Scanned)
	assert.Equal(t, 0, second.ClosedMissingStop)
	assert.Equal(t, int64(1), countRows(t, db, &domain.RadiusAccounting{}))
	assert.Equal(t, int64(1), countRows(t, db, &domain.RadiusAcctRecord{}))

	// An out-of-order session is repaired once and reported.
	insertOnlineSession(t, db, 2, "s-ooo-close", "10.0.0.1", start)
	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 1, AcctSessionId: "s-ooo-close", StatusType: acctStatusStart,
		EventTimestamp: start, AcctSessionTime: 100, InputOctets: 1000, OutputOctets: 2000,
		DedupKey: "ooo-close-1", CreatedAt: start,
	}).Error)
	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 2, AcctSessionId: "s-ooo-close", StatusType: acctStatusInterim,
		EventTimestamp: start.Add(-time.Minute), AcctSessionTime: 50,
		InputOctets: 500, OutputOctets: 1000,
		DedupKey: "ooo-close-2", CreatedAt: start.Add(time.Minute),
	}).Error)
	third, err := e.ReconcileSessions(ctx, time.Now().UTC(), 0)
	require.NoError(t, err)
	assert.Equal(t, 1, third.ClosedMissingStop)
	assert.Equal(t, 1, third.RepairedOutOfOrder)
}

func TestHandleNASReboot(t *testing.T) {
	e, db := testReconcileEngine(t)
	ctx := context.Background()
	rebootAt := time.Now().UTC().Add(-30 * time.Minute)

	insertOnlineSession(t, db, 1, "nas-before-1", "10.0.0.2", rebootAt.Add(-2*time.Hour))
	insertOnlineSession(t, db, 2, "nas-before-2", "10.0.0.2", rebootAt.Add(-time.Hour))
	insertOnlineSession(t, db, 3, "nas-after", "10.0.0.2", rebootAt.Add(time.Hour))
	insertOnlineSession(t, db, 4, "other-nas", "10.0.0.3", rebootAt.Add(-2*time.Hour))

	res, err := e.HandleNASReboot(ctx, "10.0.0.2", rebootAt)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Scanned, "only sessions started before the Accounting-On time are scanned")
	assert.Equal(t, 2, res.ClosedMissingStop)
	assert.Equal(t, int64(2), countRows(t, db, &domain.RadiusOnline{}))

	for _, sessionID := range []string{"nas-before-1", "nas-before-2"} {
		var cdr domain.RadiusAccounting
		require.NoError(t, db.Where("acct_session_id = ?", sessionID).First(&cdr).Error)
		assert.Equal(t, acctStatusStop, cdr.AcctStatusType)
		assert.Equal(t, rebootAt.UTC().Unix(), cdr.AcctStopTime.UTC().Unix(),
			"stop time comes from the caller's Accounting-On event time")
	}

	var afterReboot domain.RadiusOnline
	require.NoError(t, db.Where("acct_session_id = ?", "nas-after").First(&afterReboot).Error)
	var otherNas domain.RadiusOnline
	require.NoError(t, db.Where("acct_session_id = ?", "other-nas").First(&otherNas).Error)

	again, err := e.HandleNASReboot(ctx, "10.0.0.2", rebootAt)
	require.NoError(t, err)
	assert.Equal(t, 0, again.Scanned)
	assert.Equal(t, int64(2), countRows(t, db, &domain.RadiusAcctRecord{}),
		"one synthetic Stop marker per closed session")

	_, err = e.HandleNASReboot(ctx, "10.0.0.2", time.Time{})
	assert.Error(t, err, "a reboot time must be supplied, never fabricated")
}

func TestRestoreDeadLettersReplaysOnce(t *testing.T) {
	e, db := testReconcileEngine(t)
	ctx := context.Background()

	account, err := e.CreateAccount(ctx, "dead-letter-user", "", 100000, 0, 1<<30)
	require.NoError(t, err)
	require.NoError(t, db.Create(&domain.BillingPlan{
		ID: 101, Name: "traffic", ChargeType: "traffic", PriceFen: 10, RoundBytes: 1 << 20,
	}).Error)
	require.NoError(t, db.Create(&domain.BillingSubscription{
		AccountID: account.ID, PlanID: 101, Status: "active",
		StartAt:       time.Now().UTC().Add(-24 * time.Hour),
		EndAt:         time.Now().UTC().Add(24 * time.Hour),
		IdempotentKey: "sub-dead-letter",
	}).Error)

	old := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 1, Username: "dead-letter-user", AcctSessionId: "dl-s1",
		StatusType: acctStatusInterim, EventTimestamp: old,
		AcctSessionTime: 60, InputOctets: 2 << 20,
		DedupKey: "dead-letter-1", CreatedAt: old,
	}).Error)
	// Inside the quiet window: must not be touched.
	recent := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 2, Username: "dead-letter-user", AcctSessionId: "dl-s2",
		StatusType: acctStatusInterim, EventTimestamp: recent,
		AcctSessionTime: 60, InputOctets: 2 << 20,
		DedupKey: "dead-letter-recent", CreatedAt: recent,
	}).Error)

	res, err := e.RestoreDeadLetters(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned)
	assert.Equal(t, 1, res.Replayed, "errors: %v", res.Errors)

	var replayed domain.RadiusAcctRecord
	require.NoError(t, db.First(&replayed, 1).Error)
	assert.True(t, replayed.Consumed)
	var pending domain.RadiusAcctRecord
	require.NoError(t, db.First(&pending, 2).Error)
	assert.False(t, pending.Consumed)

	updated, err := e.GetAccount(ctx, "dead-letter-user")
	require.NoError(t, err)
	assert.Equal(t, int64(100000-20), updated.CashFen, "2 MiB at 10 fen/MiB")

	again, err := e.RestoreDeadLetters(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, again.Replayed)
	updated, err = e.GetAccount(ctx, "dead-letter-user")
	require.NoError(t, err)
	assert.Equal(t, int64(100000-20), updated.CashFen, "replay must not double charge")
	assert.Equal(t, int64(2), countRows(t, db, &domain.BillingAssetFlow{}),
		"one cash and one byte flow for the traffic charge")
}

func TestRestoreDeadLettersSkipsUnresolvableAccount(t *testing.T) {
	e, db := testReconcileEngine(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, db.Create(&domain.RadiusAcctRecord{
		ID: 1, Username: "no-such-user", AcctSessionId: "dl-unknown",
		StatusType: acctStatusInterim, EventTimestamp: old,
		DedupKey: "dead-letter-unknown", CreatedAt: old,
	}).Error)

	res, err := e.RestoreDeadLetters(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned)
	assert.Equal(t, 0, res.Replayed)
	assert.Len(t, res.Errors, 1, "unresolvable dead letters are reported, not guessed")

	var rec domain.RadiusAcctRecord
	require.NoError(t, db.First(&rec, 1).Error)
	assert.False(t, rec.Consumed, "failed replay stays available for retry")
}

func TestNormalizeReconcileBatchSize(t *testing.T) {
	assert.Equal(t, 500, normalizeReconcileBatchSize(0))
	assert.Equal(t, 500, normalizeReconcileBatchSize(-7))
	assert.Equal(t, 7, normalizeReconcileBatchSize(7))
	assert.Equal(t, 5000, normalizeReconcileBatchSize(99999))
}
