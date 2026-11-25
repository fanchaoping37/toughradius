package adminapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/domain"
)

// TestBillingAccountAndCredit exercises account creation and the
// Idempotency-Key protection on credits through the management API.
func TestBillingAccountAndCredit(t *testing.T) {
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)
	e := setupTestEcho()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/billing/accounts",
		strings.NewReader(`{"username":"api-user","cash_fen":"1000"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := CreateTestContext(e, db, req, rec, appCtx)
	require.NoError(t, CreateBillingAccount(c))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	raw, _ := json.Marshal(resp.Data)
	var account domain.BillingAccount
	require.NoError(t, json.Unmarshal(raw, &account))

	for i := 0; i < 2; i++ {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/billing/accounts/"+strconv.FormatInt(account.ID, 10)+"/credit",
			strings.NewReader(`{"asset_type":"CASH","amount":"500","biz_type":"recharge"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set("Idempotency-Key", "api-credit-1")
		rec = httptest.NewRecorder()
		c = CreateTestContext(e, db, req, rec, appCtx)
		c.SetParamNames("id")
		c.SetParamValues(strconv.FormatInt(account.ID, 10))
		require.NoError(t, CreditBillingAccount(c))
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	var after domain.BillingAccount
	require.NoError(t, db.First(&after, account.ID).Error)
	assert.Equal(t, int64(1500), after.CashFen, "duplicate API credits must be idempotent")
}

// TestPaymentNotifySignature requires a valid HMAC-SHA256 signature over the
// raw callback body, matching SRS §11.1/§12.5.
func TestPaymentNotifySignature(t *testing.T) {
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)
	e := setupTestEcho()
	account := domain.BillingAccount{ID: 1, Username: "payer", Status: "normal"}
	require.NoError(t, db.Create(&account).Error)
	order := domain.BillingOrder{OrderNo: "ORD-1", AccountID: 1, AmountFen: 800, Status: "pending", IdempotentKey: "k1"}
	require.NoError(t, db.Create(&order).Error)

	body := `{"orderNo":"ORD-1","channelTxnID":"TX-1","channel":"wechat"}`
	mac := hmac.New(sha256.New, []byte(appCtx.Config().Web.Secret))
	_, _ = mac.Write([]byte(body))
	signature := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/billing/payments/notify", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("X-Signature", signature)
	rec := httptest.NewRecorder()
	c := CreateTestContext(e, db, req, rec, appCtx)
	require.NoError(t, PaymentNotify(c))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var after domain.BillingAccount
	require.NoError(t, db.First(&after, 1).Error)
	assert.Equal(t, int64(800), after.CashFen)
}
