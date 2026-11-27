package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/billing"
	"github.com/talkincode/toughradius/v9/internal/domain"
)

// TestFinanceLedgerAndRefund verifies that direct settlement writes the
// double-entry ledger and that operator refunds are idempotent over HTTP.
func TestFinanceLedgerAndRefund(t *testing.T) {
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)
	e := setupTestEcho()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/billing/accounts",
		strings.NewReader(`{"username":"finance-user","cash_fen":"0"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, CreateBillingAccount(CreateTestContext(e, db, req, rec, appCtx)))
	require.Equal(t, http.StatusOK, rec.Code)

	var account domain.BillingAccount
	require.NoError(t, db.Where("username = ?", "finance-user").First(&account).Error)

	orderReq := httptest.NewRequest(http.MethodPost, "/api/v1/billing/orders",
		strings.NewReader(`{"account_id":"`+strconv.FormatInt(account.ID, 10)+`","amount_fen":"3000","channel":"alipay"}`))
	orderReq.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	orderReq.Header.Set("Idempotency-Key", "finance-order-key")
	orderRec := httptest.NewRecorder()
	require.NoError(t, CreateBillingOrder(CreateTestContext(e, db, orderReq, orderRec, appCtx)))
	require.Equal(t, http.StatusOK, orderRec.Code)

	var order domain.BillingOrder
	require.NoError(t, db.Where("idempotent_key = ?", "finance-order-key").First(&order).Error)
	_, settleErr := billing.NewEngine(db).MarkOrderPaid(t.Context(), order.OrderNo, "FINANCE-TXN", "alipay", "{}", true)
	require.NoError(t, settleErr)

	var entries int64
	require.NoError(t, db.Model(&domain.BillingLedgerEntry{}).Count(&entries).Error)
	assert.Equal(t, int64(2), entries, "settlement must post one balanced double-entry pair")

	body := `{"order_no":"` + order.OrderNo + `","amount_fen":"1000","reason":"test"}`
	for i := 0; i < 2; i++ {
		refundReq := httptest.NewRequest(http.MethodPost, "/api/v1/billing/refunds", strings.NewReader(body))
		refundReq.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		refundReq.Header.Set("Idempotency-Key", "finance-refund-key")
		refundRec := httptest.NewRecorder()
		require.NoError(t, RefundOrder(CreateTestContext(e, db, refundReq, refundRec, appCtx)))
		require.Equal(t, http.StatusOK, refundRec.Code, refundRec.Body.String())
	}

	var refunds int64
	require.NoError(t, db.Model(&domain.BillingRefund{}).Count(&refunds).Error)
	assert.Equal(t, int64(1), refunds, "repeat refund with same idempotency key must be a no-op")

	var after domain.BillingAccount
	require.NoError(t, db.First(&after, account.ID).Error)
	assert.Equal(t, int64(2000), after.CashFen)

	ledgerReq := httptest.NewRequest(http.MethodGet, "/api/v1/billing/ledger?account_id="+strconv.FormatInt(account.ID, 10), nil)
	ledgerRec := httptest.NewRecorder()
	require.NoError(t, ListLedgerEntries(CreateTestContext(e, db, ledgerReq, ledgerRec, appCtx)))
	require.Equal(t, http.StatusOK, ledgerRec.Code)
	var payload Response
	require.NoError(t, json.Unmarshal(ledgerRec.Body.Bytes(), &payload))
	raw, err := json.Marshal(payload.Data)
	require.NoError(t, err)
	var listed []domain.BillingLedgerEntry
	require.NoError(t, json.Unmarshal(raw, &listed))
	assert.Len(t, listed, 4, "two recharge entries plus two refund entries")
}

// TestFinanceReconciliationEndpoint closes a stale online session through the
// operator API and produces a synthetic Stop record.
func TestFinanceReconciliationEndpoint(t *testing.T) {
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)
	e := setupTestEcho()

	start := time.Now().UTC().Add(-2 * time.Hour)
	require.NoError(t, db.Create(&domain.RadiusOnline{
		ID:              222,
		Username:        "stale-user",
		AcctSessionId:   "finance-stale-session",
		NasAddr:         "10.0.0.1",
		AcctStartTime:   start,
		EventTimestamp:  start,
		AcctSessionTime: 60,
		AcctInputTotal:  1024,
		AcctOutputTotal: 2048,
		LastUpdate:      start,
	}).Error)

	cutoff := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/billing/reconciliation",
		strings.NewReader(`{"cutoff":"`+cutoff+`","batch_size":10}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, ReconcileSessions(CreateTestContext(e, db, req, rec, appCtx)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var online int64
	require.NoError(t, db.Model(&domain.RadiusOnline{}).Where("acct_session_id = ?", "finance-stale-session").Count(&online).Error)
	assert.Zero(t, online)

	var marker domain.RadiusAcctRecord
	require.NoError(t, db.Where("dedup_key = ?", "recon-stop:finance-stale-session").First(&marker).Error)
	assert.Equal(t, 2, marker.StatusType)
	assert.Equal(t, int64(1024), marker.InputOctets)
	assert.True(t, marker.Consumed)
}
