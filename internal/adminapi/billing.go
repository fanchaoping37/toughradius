package adminapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/talkincode/toughradius/v9/internal/billing"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/internal/webserver"
	"github.com/talkincode/toughradius/v9/pkg/common"
)

// Billing management API. All amounts are integer fen, traffic is bytes and
// bandwidth is bps (SRS §10 naming conventions).

type billingAccountPayload struct {
	Username    string `json:"username" validate:"required,min=1,max=128"`
	RealName    string `json:"real_name" validate:"omitempty,max=100"`
	CashFen     int64  `json:"cash_fen,string" validate:"omitempty,gte=0"`
	TimeSeconds int64  `json:"time_seconds,string" validate:"omitempty,gte=0"`
	BytesQuota  int64  `json:"bytes_quota,string" validate:"omitempty,gte=0"`
}

type billingCreditPayload struct {
	AssetType string `json:"asset_type" validate:"required,oneof=CASH TIME BYTES"`
	Amount    int64  `json:"amount,string" validate:"required,gt=0"`
	BizType   string `json:"biz_type" validate:"omitempty,max=40"`
	RefID     string `json:"ref_id" validate:"omitempty,max=128"`
}

type billingPlanPayload struct {
	Name         string `json:"name" validate:"required,min=1,max=100"`
	ChargeType   string `json:"charge_type" validate:"required,oneof=monthly daily time traffic tiered capped combo"`
	PriceFen     int64  `json:"price_fen,string" validate:"omitempty,gte=0"`
	RoundSeconds int    `json:"round_seconds" validate:"omitempty,gte=0"`
	RoundBytes   int64  `json:"round_bytes,string" validate:"omitempty,gte=0"`
	CapFen       int64  `json:"cap_fen,string" validate:"omitempty,gte=0"`
	QuotaSeconds int64  `json:"quota_seconds,string" validate:"omitempty,gte=0"`
	QuotaBytes   int64  `json:"quota_bytes,string" validate:"omitempty,gte=0"`
	UpBps        int64  `json:"up_bps,string" validate:"omitempty,gte=0"`
	DownBps      int64  `json:"down_bps,string" validate:"omitempty,gte=0"`
	MaxTerminals int    `json:"max_terminals" validate:"omitempty,gte=0"`
	ValidDays    int    `json:"valid_days" validate:"omitempty,gte=0"`
	StepsJSON    string `json:"steps_json"`
	DiscountJSON string `json:"discount_json"`
	Status       string `json:"status" validate:"omitempty,oneof=enabled disabled"`
	Remark       string `json:"remark"`
}

type billingSubscriptionPayload struct {
	AccountID int64  `json:"account_id,string" validate:"required,gt=0"`
	PlanID    int64  `json:"plan_id,string" validate:"required,gt=0"`
	Days      int    `json:"days" validate:"omitempty,gt=0"`
	Source    string `json:"source"`
}

type billingOrderPayload struct {
	AccountID int64  `json:"account_id,string" validate:"required,gt=0"`
	PlanID    int64  `json:"plan_id,string" validate:"omitempty,gte=0"`
	AmountFen int64  `json:"amount_fen,string" validate:"required,gt=0"`
	Channel   string `json:"channel" validate:"required,min=1,max=20"`
}

func billingEngine(c echo.Context) *billing.Engine {
	return billing.NewEngine(GetDB(c))
}

// ListBillingAccounts supports paging and username filtering.
func ListBillingAccounts(c echo.Context) error {
	db := GetDB(c)
	page, size := pageParams(c)
	q := db.Model(&domain.BillingAccount{})
	if username := c.QueryParam("username"); username != "" {
		q = q.Where("username LIKE ?", "%"+username+"%")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to count accounts", err.Error())
	}
	var accounts []domain.BillingAccount
	if err := q.Order("id DESC").Offset((page - 1) * size).Limit(size).Find(&accounts).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to list accounts", err.Error())
	}
	return paged(c, accounts, total, page, size)
}

func pageParams(c echo.Context) (int, int) {
	page, _ := strconv.Atoi(c.QueryParam("page"))
	size, _ := strconv.Atoi(c.QueryParam("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 500 {
		size = 50
	}
	return page, size
}

// GetBillingAccount returns one account by primary key.
func GetBillingAccount(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid account id", nil)
	}
	var account domain.BillingAccount
	if err := GetDB(c).First(&account, id).Error; err != nil {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "Account not found", nil)
	}
	return ok(c, account)
}

// CreateBillingAccount opens a subscriber asset account.
func CreateBillingAccount(c echo.Context) error {
	var payload billingAccountPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	account, err := billingEngine(c).CreateAccount(c.Request().Context(), payload.Username, payload.RealName,
		payload.CashFen, payload.TimeSeconds, payload.BytesQuota)
	if err != nil {
		return fail(c, http.StatusInternalServerError, "CREATE_FAILED", "Failed to create account", err.Error())
	}
	return ok(c, account)
}

// SetBillingAccountStatus suspends/resumes an account (SRS §12.1 state machine).
func SetBillingAccountStatus(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid account id", nil)
	}
	var payload struct {
		Status string `json:"status" validate:"required,oneof=normal suspended frozen closed"`
	}
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	res := GetDB(c).Model(&domain.BillingAccount{}).Where("id = ?", id).
		Updates(map[string]interface{}{"status": payload.Status, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return fail(c, http.StatusInternalServerError, "UPDATE_FAILED", "Failed to update account", res.Error.Error())
	}
	if res.RowsAffected == 0 {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "Account not found", nil)
	}
	return ok(c, map[string]interface{}{"id": id, "status": payload.Status})
}

// CreditBillingAccount requires an Idempotency-Key header for safe recharge.
func CreditBillingAccount(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid account id", nil)
	}
	var payload billingCreditPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	key := c.Request().Header.Get("Idempotency-Key")
	if key == "" {
		return fail(c, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required", nil)
	}
	operator := ""
	if op, err := resolveOperatorFromContext(c); err == nil {
		operator = op.Username
	}
	if err := billingEngine(c).Credit(c.Request().Context(), id, payload.AssetType, payload.Amount,
		payload.BizType, payload.RefID, key, operator); err != nil {
		return fail(c, http.StatusBadRequest, "CREDIT_FAILED", "Failed to credit account", err.Error())
	}
	return ok(c, map[string]interface{}{"account_id": id, "credited": payload.Amount})
}

// ListBillingPlans lists product definitions.
func ListBillingPlans(c echo.Context) error {
	var plans []domain.BillingPlan
	if err := GetDB(c).Order("id DESC").Find(&plans).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to list plans", err.Error())
	}
	return ok(c, plans)
}

// CreateBillingPlan creates a product/price-rule definition.
func CreateBillingPlan(c echo.Context) error {
	var payload billingPlanPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	plan := domain.BillingPlan{
		ID: common.UUIDint64(), Name: payload.Name, ChargeType: payload.ChargeType,
		PriceFen: payload.PriceFen, RoundSeconds: payload.RoundSeconds, RoundBytes: payload.RoundBytes,
		CapFen: payload.CapFen, QuotaSeconds: payload.QuotaSeconds, QuotaBytes: payload.QuotaBytes,
		UpBps: payload.UpBps, DownBps: payload.DownBps, MaxTerminals: payload.MaxTerminals,
		ValidDays: payload.ValidDays, StepsJSON: payload.StepsJSON, DiscountJSON: payload.DiscountJSON,
		Status: payload.Status, Remark: payload.Remark, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if plan.Status == "" {
		plan.Status = "enabled"
	}
	if err := GetDB(c).Create(&plan).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "CREATE_FAILED", "Failed to create plan", err.Error())
	}
	return ok(c, plan)
}

// CreateBillingSubscription subscribes an account to a plan; period fees are
// charged exactly once thanks to the generated idempotency key.
func CreateBillingSubscription(c echo.Context) error {
	var payload billingSubscriptionPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	days := payload.Days
	if days <= 0 {
		days = 30
	}
	key := c.Request().Header.Get("Idempotency-Key")
	if key == "" {
		key = "subscription-api:" + strconv.FormatInt(payload.AccountID, 10) + ":" + strconv.FormatInt(payload.PlanID, 10)
	}
	now := time.Now().UTC()
	sub, err := billingEngine(c).Subscribe(c.Request().Context(), payload.AccountID, payload.PlanID,
		now, now.AddDate(0, 0, days), key, payload.Source)
	if err != nil {
		return fail(c, http.StatusBadRequest, "SUBSCRIBE_FAILED", "Failed to subscribe", err.Error())
	}
	return ok(c, sub)
}

// ListBillingCharges returns issued bills, optionally filtered by account.
func ListBillingCharges(c echo.Context) error {
	q := GetDB(c).Model(&domain.BillingCharge{})
	if accountID := c.QueryParam("account_id"); accountID != "" {
		q = q.Where("account_id = ?", accountID)
	}
	var charges []domain.BillingCharge
	if err := q.Order("id DESC").Limit(500).Find(&charges).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to list charges", err.Error())
	}
	return ok(c, charges)
}

// CreateBillingOrder creates a recharge order.
func CreateBillingOrder(c echo.Context) error {
	var payload billingOrderPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	key := c.Request().Header.Get("Idempotency-Key")
	if key == "" {
		return fail(c, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required", nil)
	}
	order, err := billingEngine(c).CreateOrder(c.Request().Context(), payload.AccountID, payload.PlanID,
		payload.AmountFen, payload.Channel, key)
	if err != nil {
		return fail(c, http.StatusInternalServerError, "CREATE_FAILED", "Failed to create order", err.Error())
	}
	return ok(c, order)
}

// PaymentNotify accepts signed payment callbacks (HMAC-SHA256 over the raw
// body using the web signing secret) and settles the order idempotently.
func PaymentNotify(c echo.Context) error {
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, 1<<20))
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to read callback", nil)
	}
	signature := c.Request().Header.Get("X-Signature")
	secret := GetAppContext(c).Config().Web.Secret
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if signature == "" || !hmac.Equal([]byte(signature), []byte(expected)) {
		return fail(c, http.StatusUnauthorized, "INVALID_SIGNATURE", "Payment callback signature mismatch", nil)
	}
	var payload struct {
		OrderNo      string `json:"orderNo"`
		ChannelTxnID string `json:"channelTxnId"`
		Channel      string `json:"channel"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Invalid callback payload", nil)
	}
	order, err := billingEngine(c).MarkOrderPaid(c.Request().Context(), payload.OrderNo,
		payload.ChannelTxnID, payload.Channel, string(body), true)
	if err != nil {
		return fail(c, http.StatusBadRequest, "PAYMENT_FAILED", "Failed to settle payment", err.Error())
	}
	return ok(c, order)
}

// registerBillingRoutes registers SRS billing management endpoints.
func registerBillingRoutes() {
	webserver.ApiGET("/billing/accounts", ListBillingAccounts)
	webserver.ApiGET("/billing/accounts/:id", GetBillingAccount)
	webserver.ApiPOST("/billing/accounts", CreateBillingAccount, requireAdmin())
	webserver.ApiPUT("/billing/accounts/:id/status", SetBillingAccountStatus, requireAdmin())
	webserver.ApiPOST("/billing/accounts/:id/credit", CreditBillingAccount, requireAdmin())

	webserver.ApiGET("/billing/plans", ListBillingPlans)
	webserver.ApiPOST("/billing/plans", CreateBillingPlan, requireAdmin())
	webserver.ApiPOST("/billing/subscriptions", CreateBillingSubscription, requireAdmin())
	webserver.ApiGET("/billing/charges", ListBillingCharges)
	webserver.ApiPOST("/billing/orders", CreateBillingOrder, requireAdmin())
	webserver.ApiPOST("/billing/payments/notify", PaymentNotify)
}
