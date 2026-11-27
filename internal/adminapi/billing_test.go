package adminapi

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/config"
	"github.com/talkincode/toughradius/v9/internal/app"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"gorm.io/gorm"
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

// setupPaymentOrder creates a pending order together with an empty account.
func setupPaymentOrder(t *testing.T, orderNo string, amountFen int64) (*gorm.DB, *echo.Echo, app.AppContext, domain.BillingAccount) {
	t.Helper()
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)
	e := setupTestEcho()
	account := domain.BillingAccount{Username: "payer", Status: "normal"}
	require.NoError(t, db.Create(&account).Error)
	order := domain.BillingOrder{
		OrderNo: orderNo, AccountID: account.ID, AmountFen: amountFen,
		Status: "pending", IdempotentKey: "key-" + orderNo,
	}
	require.NoError(t, db.Create(&order).Error)
	return db, e, appCtx, account
}

func notifyContext(e *echo.Echo, db *gorm.DB, appCtx app.AppContext, body string, headers map[string]string) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/billing/payments/notify", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	return CreateTestContext(e, db, req, rec, appCtx), rec
}

func assertAccountCash(t *testing.T, db *gorm.DB, accountID int64, want int64) {
	t.Helper()
	var after domain.BillingAccount
	require.NoError(t, db.First(&after, accountID).Error)
	assert.Equal(t, want, after.CashFen)
}

// cardCallbackBody builds a valid campus-card HMAC callback for the given
// amount. Reusing the same nonce produces the same channel transaction id.
func cardCallbackBody(orderNo, nonce string, amountFen int64, secret string) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	amount := strconv.FormatInt(amountFen, 10)
	method, cardNo := "recharge", "CARD-1"
	mac := hmac.New(sha256.New, []byte(secret))
	for _, part := range []string{method, cardNo, amount, ts, nonce} {
		_, _ = mac.Write([]byte(part))
	}
	sign := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf(`{"method":%q,"card_no":%q,"amount":%q,"timestamp":%q,"nonce":%q,"order_no":%q,"sign":%q}`,
		method, cardNo, amount, ts, nonce, orderNo, sign)
}

// alipayCallbackBody builds a valid Alipay RSA2 form callback.
func alipayCallbackBody(t *testing.T, key *rsa.PrivateKey, orderNo, txnID, amount, status string) string {
	t.Helper()
	params := map[string]string{
		"app_id":       "2021000000000001",
		"out_trade_no": orderNo,
		"trade_no":     txnID,
		"total_amount": amount,
		"trade_status": status,
		"sign_type":    "RSA2",
		"charset":      "utf-8",
	}
	// Alipay excludes sign and sign_type from the signed string.
	keys := make([]string, 0, len(params))
	for k := range params {
		if k != "sign" && k != "sign_type" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var signString strings.Builder
	for i, k := range keys {
		if i > 0 {
			signString.WriteByte('&')
		}
		signString.WriteString(k)
		signString.WriteByte('=')
		signString.WriteString(params[k])
	}
	hash := sha256.Sum256([]byte(signString.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	require.NoError(t, err)
	params["sign"] = base64.StdEncoding.EncodeToString(sig)
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	return values.Encode()
}

func alipayTestKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// TestPaymentNotifySignature exercises the legacy fallback: the test app has
// payment.enabled=false, so only the deprecated X-Signature HMAC path is used.
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

	// A bad legacy signature is still rejected.
	c, rec = notifyContext(e, db, appCtx, body, map[string]string{"X-Signature": "bad-signature"})
	require.NoError(t, PaymentNotify(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "INVALID_SIGNATURE")
}

// TestPaymentNotifyIgnoresLegacySignatureWhenEnabled ensures an X-Signature
// cannot bypass provider verification once payment verification is enabled.
func TestPaymentNotifyIgnoresLegacySignatureWhenEnabled(t *testing.T) {
	db, e, appCtx, account := setupPaymentOrder(t, "ORD-LEGACY-BYPASS", 800)
	appCtx.Config().Payment = config.PaymentConfig{Enabled: true, CardSecret: "card-secret"}

	body := `{"orderNo":"ORD-LEGACY-BYPASS","channelTxnID":"TX-BYPASS","channel":"wechat"}`
	mac := hmac.New(sha256.New, []byte(appCtx.Config().Web.Secret))
	_, _ = mac.Write([]byte(body))
	c, rec := notifyContext(e, db, appCtx, body, map[string]string{
		"X-Signature": hex.EncodeToString(mac.Sum(nil)),
	})
	require.NoError(t, PaymentNotify(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertAccountCash(t, db, account.ID, 0)
}

func TestPaymentNotifyRequiresConfiguredChannel(t *testing.T) {
	t.Run("no hint and no default", func(t *testing.T) {
		db, e, appCtx, account := setupPaymentOrder(t, "ORD-NOCHANNEL", 800)
		appCtx.Config().Payment = config.PaymentConfig{Enabled: true}

		c, rec := notifyContext(e, db, appCtx, `{"foo":"bar"}`, nil)
		require.NoError(t, PaymentNotify(c))
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "PAYMENT_CHANNEL_UNAVAILABLE")
		assertAccountCash(t, db, account.ID, 0)
	})

	t.Run("default channel without credentials", func(t *testing.T) {
		db, e, appCtx, account := setupPaymentOrder(t, "ORD-NOCRED", 800)
		appCtx.Config().Payment = config.PaymentConfig{Enabled: true, DefaultChannel: "wechat"}

		c, rec := notifyContext(e, db, appCtx, `{"foo":"bar"}`, nil)
		require.NoError(t, PaymentNotify(c))
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "PAYMENT_CHANNEL_UNAVAILABLE")
		assertAccountCash(t, db, account.ID, 0)
	})

	t.Run("verification disabled without legacy signature", func(t *testing.T) {
		db, e, appCtx, account := setupPaymentOrder(t, "ORD-DISABLED", 800)
		c, rec := notifyContext(e, db, appCtx, `{"orderNo":"ORD-DISABLED"}`, nil)
		require.NoError(t, PaymentNotify(c))
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assertAccountCash(t, db, account.ID, 0)
	})
}

func TestPaymentNotifyAmountMismatchRejected(t *testing.T) {
	db, e, appCtx, account := setupPaymentOrder(t, "ORD-AMOUNT", 800)
	const secret = "card-secret"
	appCtx.Config().Payment = config.PaymentConfig{Enabled: true, CardSecret: secret}

	body := cardCallbackBody("ORD-AMOUNT", "TX-AMOUNT", 1000, secret)
	c, rec := notifyContext(e, db, appCtx, body, nil)
	require.NoError(t, PaymentNotify(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "PAYMENT_AMOUNT_MISMATCH")
	assertAccountCash(t, db, account.ID, 0)
}

func TestPaymentNotifyUnpaidRejected(t *testing.T) {
	key, pemKey := alipayTestKey(t)
	db, e, appCtx, account := setupPaymentOrder(t, "ORD-UNPAID", 800)
	appCtx.Config().Payment = config.PaymentConfig{
		Enabled:         true,
		AlipayAppID:     "2021000000000001",
		AlipayPublicKey: pemKey,
	}

	body := alipayCallbackBody(t, key, "ORD-UNPAID", "ALI-UNPAID", "8.00", "WAIT_BUYER_PAY")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/billing/payments/notify", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	c := CreateTestContext(e, db, req, rec, appCtx)
	require.NoError(t, PaymentNotify(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "PAYMENT_NOT_PAID")
	assertAccountCash(t, db, account.ID, 0)
}

func TestPaymentNotifyDuplicateCallbackCreditsOnce(t *testing.T) {
	db, e, appCtx, account := setupPaymentOrder(t, "ORD-DUP", 800)
	const secret = "card-secret"
	appCtx.Config().Payment = config.PaymentConfig{Enabled: true, CardSecret: secret}

	body := cardCallbackBody("ORD-DUP", "TX-DUP", 800, secret)
	for i := 0; i < 2; i++ {
		c, rec := notifyContext(e, db, appCtx, body, nil)
		require.NoError(t, PaymentNotify(c))
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	assertAccountCash(t, db, account.ID, 800)

	var payments int64
	require.NoError(t, db.Model(&domain.BillingPayment{}).Where("order_no = ?", "ORD-DUP").Count(&payments).Error)
	assert.Equal(t, int64(1), payments, "duplicate callbacks must create only one payment record")
}
