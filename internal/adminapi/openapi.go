package adminapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/internal/webserver"
	"github.com/talkincode/toughradius/v9/pkg/common"
)

// Northbound API response envelope (SRS §11.1) includes a trace id so every
// external call can be correlated with the audit log.
type openResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
	TraceID string      `json:"traceId"`
}

func traceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func openOK(c echo.Context, data interface{}, id string) error {
	return c.JSON(http.StatusOK, openResponse{Code: 0, Message: "ok", Data: data, TraceID: id})
}

func openFail(c echo.Context, status int, code string, message string, id string) error {
	return c.JSON(status, openResponse{Code: codeInt(code), Message: message, TraceID: id})
}

func codeInt(code string) int {
	switch code {
	case "OK":
		return 0
	case "UNAUTHORIZED":
		return 40101
	case "REPLAY":
		return 40102
	case "RATE_LIMITED":
		return 42901
	case "NOT_FOUND":
		return 40401
	case "IDEMPOTENCY_REQUIRED":
		return 40001
	default:
		return 40000
	}
}

// nonceCache enforces the SRS 5-minute Nonce reuse window.
type cacheEntry struct {
	value interface{}
	at    time.Time
}

type ttlCache struct {
	mu     sync.Mutex
	values map[string]cacheEntry
	ttl    time.Duration
	lastGC time.Time
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{values: make(map[string]cacheEntry), ttl: ttl, lastGC: time.Now()}
}

func (c *ttlCache) PutIfAbsent(key string, value interface{}) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gc()
	if _, exists := c.values[key]; exists {
		return false
	}
	c.values[key] = cacheEntry{value: value, at: time.Now()}
	return true
}

func (c *ttlCache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gc()
	entry, ok := c.values[key]
	if !ok {
		return nil, false
	}
	return entry.value, true
}

func (c *ttlCache) Set(key string, value interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gc()
	c.values[key] = cacheEntry{value: value, at: time.Now()}
}

func (c *ttlCache) gc() {
	now := time.Now()
	if c.lastGC.Add(c.ttl).Before(now) {
		for k, entry := range c.values {
			if entry.at.Add(c.ttl).Before(now) {
				delete(c.values, k)
			}
		}
		c.lastGC = now
	}
}

var (
	openNonces      = newTTLCache(5 * time.Minute)
	idempotencyData = newTTLCache(24 * time.Hour)
)

type appBucket struct {
	tokens float64
	last   time.Time
}

var openBuckets sync.Map // AppKey -> *appBucket

// allowRate implements a simple token bucket; a full bucket holds one second
// of burst in addition to the configured sustained QPS.
func allowRate(key string, qps float64) bool {
	if qps <= 0 {
		return true
	}
	now := time.Now()
	value, _ := openBuckets.LoadOrStore(key, &appBucket{tokens: qps, last: now})
	bucket := value.(*appBucket)
	mu := rateLocks.load(key)
	mu.Lock()
	defer mu.Unlock()
	bucket.tokens += now.Sub(bucket.last).Seconds() * qps
	if bucket.tokens > qps*2 {
		bucket.tokens = qps * 2
	}
	bucket.last = now
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

var rateLocks = &keyedLocks{values: make(map[string]*sync.Mutex)}

type keyedLocks struct {
	mu     sync.Mutex
	values map[string]*sync.Mutex
}

func (k *keyedLocks) load(key string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if m, ok := k.values[key]; ok {
		return m
	}
	m := &sync.Mutex{}
	k.values[key] = m
	return m
}

// openAPIAuth validates AppKey + Timestamp + Nonce + HMAC-SHA256 and applies
// replay, rate-limit and audit controls. It is fail-closed when disabled.
func openAPIAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		id := traceID()
		c.Set("trace_id", id)
		cfg := GetAppContext(c).Config().OpenAPI
		if !cfg.Enabled || len(cfg.Credentials) == 0 {
			return openFail(c, http.StatusServiceUnavailable, "DISABLED", "Open API is not enabled", id)
		}
		appKey := c.Request().Header.Get("X-App-Key")
		secret, ok := cfg.Credentials[appKey]
		if !ok || appKey == "" {
			recordOpenAPIAudit(c, appKey, http.StatusUnauthorized)
			return openFail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid AppKey", id)
		}
		timestamp := c.Request().Header.Get("X-Timestamp")
		ts, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil || absInt64(time.Now().Unix()-ts) > 300 {
			recordOpenAPIAudit(c, appKey, http.StatusUnauthorized)
			return openFail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Timestamp outside permitted window", id)
		}
		nonce := c.Request().Header.Get("X-Nonce")
		if nonce == "" || !openNonces.PutIfAbsent(appKey+":"+nonce, true) {
			recordOpenAPIAudit(c, appKey, http.StatusUnauthorized)
			return openFail(c, http.StatusUnauthorized, "REPLAY", "Nonce replay detected", id)
		}
		body, err := io.ReadAll(io.LimitReader(c.Request().Body, 1<<20))
		if err != nil {
			return openFail(c, http.StatusBadRequest, "BAD_REQUEST", "Unable to read request body", id)
		}
		c.Request().Body = io.NopCloser(bytes.NewReader(body))
		bodyHash := sha256.Sum256(body)
		canonical := strings.Join([]string{
			c.Request().Method, c.Request().URL.Path, timestamp, nonce, hex.EncodeToString(bodyHash[:]),
		}, "\n")
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(canonical))
		expected := hex.EncodeToString(mac.Sum(nil))
		provided := c.Request().Header.Get("X-Signature")
		if !hmac.Equal([]byte(strings.ToLower(provided)), []byte(expected)) {
			recordOpenAPIAudit(c, appKey, http.StatusUnauthorized)
			return openFail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Signature mismatch", id)
		}
		if !allowRate(appKey, float64(cfg.QPS)) {
			recordOpenAPIAudit(c, appKey, http.StatusTooManyRequests)
			c.Response().Header().Set("Retry-After", "1")
			return openFail(c, http.StatusTooManyRequests, "RATE_LIMITED", "App QPS quota exceeded", id)
		}
		c.Set("open_app_key", appKey)
		if err := next(c); err != nil {
			return err
		}
		recordOpenAPIAudit(c, appKey, c.Response().Status)
		return nil
	}
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// recordOpenAPIAudit writes a SysOprLog entry. Audit failure is logged but
// never changes the business response.
func recordOpenAPIAudit(c echo.Context, appKey string, status int) {
	db := GetDB(c)
	if db == nil {
		return
	}
	_ = db.Create(&domain.SysOprLog{
		ID: common.UUIDint64(), OprName: "openapi:" + appKey, OprIp: c.RealIP(),
		OptAction: c.Request().Method + " " + c.Request().URL.Path,
		OptDesc:   "status=" + strconv.Itoa(status) + " trace=" + traceIDFromContext(c),
		OptTime:   time.Now().UTC(),
	}).Error
}

func traceIDFromContext(c echo.Context) string {
	if v, ok := c.Get("trace_id").(string); ok {
		return v
	}
	return ""
}

// idempotentOpen caches the first successful response for 24 hours. Write
// endpoints must supply Idempotency-Key (SRS §11.1).
func idempotentOpen(c echo.Context, work func() (interface{}, error)) error {
	id := traceIDFromContext(c)
	key := c.Request().Header.Get("Idempotency-Key")
	if key == "" {
		return openFail(c, http.StatusBadRequest, "IDEMPOTENCY_REQUIRED", "Idempotency-Key header is required", id)
	}
	cacheKey := c.Request().Method + ":" + c.Request().URL.Path + ":" + key
	if cached, ok := idempotencyData.Get(cacheKey); ok {
		resp := cached.(openResponse)
		resp.TraceID = id
		return c.JSON(http.StatusOK, resp)
	}
	data, err := work()
	if err != nil {
		return openFail(c, http.StatusBadRequest, "BAD_REQUEST", err.Error(), id)
	}
	resp := openResponse{Code: 0, Message: "ok", Data: data, TraceID: id}
	idempotencyData.Set(cacheKey, resp)
	return c.JSON(http.StatusOK, resp)
}

// OpenCreateAccount opens a subscriber account through the northbound API.
func OpenCreateAccount(c echo.Context) error {
	var payload struct {
		Username    string `json:"username" validate:"required,min=1,max=128"`
		RealName    string `json:"realName"`
		CashFen     int64  `json:"cashFen,string"`
		TimeSeconds int64  `json:"timeSeconds,string"`
		BytesQuota  int64  `json:"bytesQuota,string"`
	}
	if err := c.Bind(&payload); err != nil {
		return openFail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request", traceIDFromContext(c))
	}
	if err := c.Validate(&payload); err != nil {
		return handleOpenValidation(c, err)
	}
	return idempotentOpen(c, func() (interface{}, error) {
		return billingEngine(c).CreateAccount(c.Request().Context(), payload.Username, payload.RealName,
			payload.CashFen, payload.TimeSeconds, payload.BytesQuota)
	})
}

func handleOpenValidation(c echo.Context, err error) error {
	return openFail(c, http.StatusBadRequest, "BAD_REQUEST", err.Error(), traceIDFromContext(c))
}

// OpenAccountQuota returns the account's funded balances.
func OpenAccountQuota(c echo.Context) error {
	username := c.Param("id")
	account, err := billingEngine(c).GetAccount(c.Request().Context(), username)
	if err != nil {
		return openFail(c, http.StatusNotFound, "NOT_FOUND", "Account not found", traceIDFromContext(c))
	}
	return openOK(c, map[string]interface{}{
		"accountId": account.ID, "username": account.Username, "status": account.Status,
		"cashFen": account.CashFen, "timeSeconds": account.TimeSeconds,
		"giftTimeSeconds": account.GiftTimeSeconds, "bytesQuota": account.BytesQuota,
		"giftBytes": account.GiftBytes, "expireAt": account.ExpireAt,
	}, traceIDFromContext(c))
}

// OpenSetAccountStatus suspends/resumes an account by username.
func OpenSetAccountStatus(c echo.Context) error {
	username := c.Param("id")
	var payload struct {
		Status string `json:"status" validate:"required,oneof=normal suspended frozen closed"`
	}
	if err := c.Bind(&payload); err != nil {
		return openFail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request", traceIDFromContext(c))
	}
	if err := c.Validate(&payload); err != nil {
		return handleOpenValidation(c, err)
	}
	return idempotentOpen(c, func() (interface{}, error) {
		res := GetDB(c).Model(&domain.BillingAccount{}).Where("username = ?", username).
			Updates(map[string]interface{}{"status": payload.Status, "updated_at": time.Now().UTC()})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			return nil, errors.New("account not found")
		}
		return map[string]interface{}{"username": username, "status": payload.Status}, nil
	})
}

// OpenOnlineSessions lists live sessions filtered by username.
func OpenOnlineSessions(c echo.Context) error {
	q := GetDB(c).Model(&domain.RadiusOnline{})
	if username := c.QueryParam("username"); username != "" {
		q = q.Where("username = ?", username)
	}
	var sessions []domain.RadiusOnline
	if err := q.Order("id DESC").Limit(500).Find(&sessions).Error; err != nil {
		return openFail(c, http.StatusInternalServerError, "BAD_REQUEST", "Database error", traceIDFromContext(c))
	}
	return openOK(c, sessions, traceIDFromContext(c))
}

// OpenDisconnectSession resolves an Acct-Session-Id to the local session row
// and reuses the existing (already audited) disconnect handler.
func OpenDisconnectSession(c echo.Context) error {
	sessionID := c.Param("id")
	var session domain.RadiusOnline
	if err := GetDB(c).Where("acct_session_id = ?", sessionID).First(&session).Error; err != nil {
		return openFail(c, http.StatusNotFound, "NOT_FOUND", "Session not found", traceIDFromContext(c))
	}
	c.SetParamNames("id")
	c.SetParamValues(strconv.FormatInt(session.ID, 10))
	return DisconnectOnlineSession(c)
}

// OpenListPlans returns product definitions for external billing systems.
func OpenListPlans(c echo.Context) error {
	return ListBillingPlans(c)
}

// OpenCreateOrder creates a recharge order idempotently.
func OpenCreateOrder(c echo.Context) error {
	var payload billingOrderPayload
	if err := c.Bind(&payload); err != nil {
		return openFail(c, http.StatusBadRequest, "BAD_REQUEST", "Invalid request", traceIDFromContext(c))
	}
	if err := c.Validate(&payload); err != nil {
		return handleOpenValidation(c, err)
	}
	key := c.Request().Header.Get("Idempotency-Key")
	if key == "" {
		return openFail(c, http.StatusBadRequest, "IDEMPOTENCY_REQUIRED", "Idempotency-Key header is required", traceIDFromContext(c))
	}
	order, err := billingEngine(c).CreateOrder(c.Request().Context(), payload.AccountID, payload.PlanID,
		payload.AmountFen, payload.Channel, key)
	if err != nil {
		return openFail(c, http.StatusBadRequest, "BAD_REQUEST", err.Error(), traceIDFromContext(c))
	}
	return openOK(c, order, traceIDFromContext(c))
}

// registerOpenAPIRoutes installs the signed northbound API with JWT skipped
// (see webserver.JwtSkipPrefix) and HMAC auth applied per route.
func registerOpenAPIRoutes() {
	guard := openAPIAuth
	webserver.ApiPOST("/open/accounts", OpenCreateAccount, guard)
	webserver.ApiPUT("/open/accounts/:id/status", OpenSetAccountStatus, guard)
	webserver.ApiGET("/open/accounts/:id/quota", OpenAccountQuota, guard)
	webserver.ApiGET("/open/sessions/online", OpenOnlineSessions, guard)
	webserver.ApiPOST("/open/sessions/:id/disconnect", OpenDisconnectSession, guard)
	webserver.ApiGET("/open/plans", OpenListPlans, guard)
	webserver.ApiPOST("/open/orders", OpenCreateOrder, guard)
}
