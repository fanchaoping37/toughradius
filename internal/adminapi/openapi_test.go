package adminapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/app"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"gorm.io/gorm"
)

func signedOpenRequest(t *testing.T, e *echo.Echo,
	method, path, body, nonce string) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("X-App-Key", "test-app")
	req.Header.Set("X-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-Nonce", nonce)
	req.Header.Set("Idempotency-Key", "idem-"+nonce)
	hash := sha256.Sum256([]byte(body))
	canonical := strings.Join([]string{method, path, req.Header.Get("X-Timestamp"), nonce, hex.EncodeToString(hash[:])}, "\n")
	mac := hmac.New(sha256.New, []byte("test-app-secret"))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	return rec, CreateTestContext(e, openTestDB, req, rec, openTestApp)
}

var (
	openTestDB  *gorm.DB
	openTestApp app.AppContext
)

func dbForOpenTest(t *testing.T) *gorm.DB { return openTestDB }

func enableOpenAPI(t *testing.T) (*echo.Echo, app.AppContext) {
	t.Helper()
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)
	openBuckets.Delete("test-app")
	openNonces = newTTLCache(5 * time.Minute)
	idempotencyData = newTTLCache(24 * time.Hour)
	cfg := appCtx.Config()
	cfg.OpenAPI.Enabled = true
	cfg.OpenAPI.Credentials = map[string]string{"test-app": "test-app-secret"}
	cfg.OpenAPI.QPS = 100
	openTestDB, openTestApp = db, appCtx
	return setupTestEcho(), appCtx
}

func TestOpenAPIHMACReplayAndIdempotency(t *testing.T) {
	e, _ := enableOpenAPI(t)
	body := `{"username":"open-user","cashFen":"1000"}`
	handler := openAPIAuth(OpenCreateAccount)

	rec, c := signedOpenRequest(t, e, http.MethodPost, "/api/v1/open/accounts", body, "nonce-1")
	require.NoError(t, handler(c))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "traceId")

	// Same nonce replayed: rejected before reaching the handler.
	rec2, c2 := signedOpenRequest(t, e, http.MethodPost, "/api/v1/open/accounts", body, "nonce-1")
	require.NoError(t, handler(c2))
	assert.Equal(t, http.StatusUnauthorized, rec2.Code)
	assert.Contains(t, rec2.Body.String(), "40102")

	// Fresh nonce but same Idempotency-Key: account created only once.
	rec3, c3 := signedOpenRequest(t, e, http.MethodPost, "/api/v1/open/accounts", body, "nonce-2")
	c3.Request().Header.Set("Idempotency-Key", "idem-nonce-1")
	require.NoError(t, handler(c3))
	assert.Equal(t, http.StatusOK, rec3.Code, rec3.Body.String())

	var count int64
	require.NoError(t, dbForOpenTest(t).Model(&domain.BillingAccount{}).Where("username = ?", "open-user").Count(&count).Error)
	assert.Equal(t, int64(1), count)

	var audits int64
	require.NoError(t, dbForOpenTest(t).Model(&domain.SysOprLog{}).Where("opr_name LIKE ?", "openapi:%").Count(&audits).Error)
	assert.Greater(t, audits, int64(0), "every open API call must be audited")
}

func TestOpenAPIRejectsBadSignatureAndDisabled(t *testing.T) {
	e, appCtx := enableOpenAPI(t)
	handler := openAPIAuth(OpenCreateAccount)
	body := `{"username":"bad-sig"}`
	rec, c := signedOpenRequest(t, e, http.MethodPost, "/api/v1/open/accounts", body, "nonce-bad")
	c.Request().Header.Set("X-Signature", strings.Repeat("0", 64))
	require.NoError(t, handler(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	appCtx.Config().OpenAPI.Enabled = false
	rec2, c2 := signedOpenRequest(t, e, http.MethodPost, "/api/v1/open/accounts", body, "nonce-disabled")
	require.NoError(t, handler(c2))
	assert.Equal(t, http.StatusServiceUnavailable, rec2.Code)

	var count int64
	require.NoError(t, dbForOpenTest(t).Model(&domain.BillingAccount{}).Where("username = ?", "bad-sig").Count(&count).Error)
	assert.Equal(t, int64(0), count)
}

func TestOpenAPIRateLimit(t *testing.T) {
	e, appCtx := enableOpenAPI(t)
	appCtx.Config().OpenAPI.QPS = 1
	handler := openAPIAuth(OpenListPlans)
	for i := 0; i < 3; i++ {
		rec, c := signedOpenRequest(t, e, http.MethodGet, "/api/v1/open/plans", "", "rate-nonce-"+strconv.Itoa(i))
		require.NoError(t, handler(c))
		if i == 0 {
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		} else {
			assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		}
	}
}
