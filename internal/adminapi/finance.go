package adminapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/talkincode/toughradius/v9/internal/billing"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/internal/webserver"
)

// Finance operations expose the commercial accounting loop to operators:
// reconciliation passes, NAS-reboot CDR repair, dead-letter replay, the
// double-entry ledger and refunds. All monetary values are integer fen.

type reconcilePayload struct {
	Cutoff    string `json:"cutoff"`
	BatchSize int    `json:"batch_size"`
}

type nasRebootPayload struct {
	NasAddr   string `json:"nas_addr" validate:"required,max=128"`
	EventTime string `json:"event_time" validate:"required"`
	BatchSize int    `json:"batch_size"`
}

type deadLetterPayload struct {
	Limit int `json:"limit"`
}

type refundPayload struct {
	OrderNo   string `json:"order_no" validate:"required,max=100"`
	AmountFen int64  `json:"amount_fen,string" validate:"required,gt=0"`
	Reason    string `json:"reason" validate:"omitempty,max=200"`
}

// ReconcileSessions closes stale online sessions that never produced a Stop
// and repairs out-of-order accounting. The cutoff is explicit: an operator
// must choose the time boundary, so the API never fabricates a NAS event time.
func ReconcileSessions(c echo.Context) error {
	var payload reconcilePayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", nil)
	}
	cutoff, err := parseTimeInput(payload.Cutoff, time.Now().UTC().Add(-30*time.Minute))
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_CUTOFF", "Invalid reconciliation cutoff", nil)
	}
	result, err := billing.NewEngine(GetDB(c)).ReconcileSessions(c.Request().Context(), cutoff, payload.BatchSize)
	if err != nil {
		return fail(c, http.StatusInternalServerError, "RECONCILE_FAILED", "Failed to reconcile sessions", nil)
	}
	return ok(c, result)
}

// ReconcileNASReboot handles an Accounting-On event with an explicit RFC 2869
// Event-Timestamp. The event time must be supplied by the caller.
func ReconcileNASReboot(c echo.Context) error {
	var payload nasRebootPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", nil)
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	eventTime, err := parseTimeInput(payload.EventTime, time.Time{})
	if err != nil || eventTime.IsZero() {
		return fail(c, http.StatusBadRequest, "INVALID_EVENT_TIME", "A valid NAS reboot event time is required", nil)
	}
	result, err := billing.NewEngine(GetDB(c)).HandleNASReboot(c.Request().Context(), payload.NasAddr, eventTime.UTC())
	if err != nil {
		return fail(c, http.StatusInternalServerError, "RECONCILE_FAILED", "Failed to reconcile NAS reboot", nil)
	}
	return ok(c, result)
}

// ReplayDeadLetters replays raw accounting packets that were not consumed by
// the normal handler. Idempotency is enforced by the billing engine.
func ReplayDeadLetters(c echo.Context) error {
	var payload deadLetterPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", nil)
	}
	result, err := billing.NewEngine(GetDB(c)).RestoreDeadLetters(c.Request().Context(), payload.Limit)
	if err != nil {
		return fail(c, http.StatusInternalServerError, "REPLAY_FAILED", "Failed to replay dead letters", nil)
	}
	return ok(c, result)
}

// ListLedgerEntries returns the double-entry ledger for audit and finance.
func ListLedgerEntries(c echo.Context) error {
	accountID, err := strconv.ParseInt(c.QueryParam("account_id"), 10, 64)
	if c.QueryParam("account_id") != "" && err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ACCOUNT", "Invalid account id", nil)
	}
	limit, err := strconv.Atoi(c.QueryParam("limit"))
	if err != nil {
		limit = 100
	}
	entries, err := billing.NewEngine(GetDB(c)).ListLedgerEntries(c.Request().Context(),
		accountID, c.QueryParam("ref_id"), limit)
	if err != nil {
		return fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to list ledger entries", nil)
	}
	return ok(c, entries)
}

// RefundOrder settles a refund with an Idempotency-Key. The engine verifies
// the paid amount, writes the reverse double-entry and debits back the cash
// balance atomically.
func RefundOrder(c echo.Context) error {
	var payload refundPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request", nil)
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	key := c.Request().Header.Get("Idempotency-Key")
	if key == "" {
		return fail(c, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required", nil)
	}
	operator := "admin"
	if op, err := resolveOperatorFromContext(c); err == nil && op != nil {
		operator = op.Username
	}
	refund, err := billing.NewEngine(GetDB(c)).RefundOrder(c.Request().Context(),
		payload.OrderNo, payload.AmountFen, payload.Reason, operator, key)
	if err != nil {
		return fail(c, http.StatusBadRequest, "REFUND_FAILED", "Failed to refund order", nil)
	}
	return ok(c, refund)
}

// ListRefunds returns operator-visible refund records for audit.
func ListRefunds(c echo.Context) error {
	q := GetDB(c).Model(&domain.BillingRefund{})
	if orderNo := c.QueryParam("order_no"); orderNo != "" {
		q = q.Where("order_no = ?", orderNo)
	}
	var refunds []domain.BillingRefund
	if err := q.Order("id DESC").Limit(500).Find(&refunds).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to list refunds", nil)
	}
	return ok(c, refunds)
}

// registerFinanceRoutes exposes the finance/reconciliation endpoints.
func registerFinanceRoutes() {
	webserver.ApiPOST("/billing/reconciliation", ReconcileSessions, requireAdmin())
	webserver.ApiPOST("/billing/reconciliation/nas-reboot", ReconcileNASReboot, requireAdmin())
	webserver.ApiPOST("/billing/reconciliation/dead-letters", ReplayDeadLetters, requireAdmin())
	webserver.ApiGET("/billing/ledger", ListLedgerEntries, requireAdmin())
	webserver.ApiGET("/billing/refunds", ListRefunds, requireAdmin())
	webserver.ApiPOST("/billing/refunds", RefundOrder, requireAdmin())
}
