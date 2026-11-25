package adminapi

import (
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/internal/radiusd"
	"github.com/talkincode/toughradius/v9/internal/webserver"
	"github.com/talkincode/toughradius/v9/pkg/common"
)

// startCoATask writes the durable outbox row BEFORE the packet is sent so a
// crash cannot lose an operator action (SRS §12.5 item 4).
func startCoATask(c echo.Context, sessionID int64, identity radiusd.SessionIdentity, target radiusd.CoATarget, action, filterID string, sessionTimeout int) (*domain.RadiusCoATask, error) {
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return nil, err
	}
	attrs, _ := json.Marshal(map[string]interface{}{
		"filter_id": filterID, "session_timeout": sessionTimeout,
	})
	task := &domain.RadiusCoATask{
		ID: common.UUIDint64(), SessionID: sessionID,
		AcctSessionID: identity.AcctSessionID, Username: identity.Username,
		NasAddr: target.Addr, NasPort: target.Port, Action: action,
		AttributesJSON: string(attrs), IdentityJSON: string(identityJSON),
		FilterID: filterID, SessionTimeoutSeconds: sessionTimeout,
		Status: "pending", CreatedAt: time.Now().UTC(),
	}
	operatorID := int64(0)
	operatorName := "unknown"
	if op, err := resolveOperatorFromContext(c); err == nil && op != nil {
		operatorID, operatorName = op.ID, op.Username
	}
	task.OperatorID, task.OperatorName = operatorID, operatorName
	if err := radiusd.NewCoATaskManager(GetDB(c)).Create(c.Request().Context(), task); err != nil {
		return nil, err
	}
	return task, nil
}

// finishCoATask records the exchange outcome and the stable Identifier.
func finishCoATask(c echo.Context, task *domain.RadiusCoATask, result *radiusd.CoAResult, sendErr error) error {
	if task == nil {
		return nil
	}
	return radiusd.NewCoATaskManager(GetDB(c)).Complete(c.Request().Context(), task.ID, result, sendErr)
}

// ListCoATasks returns the durable CoA/DM task history for operators.
func ListCoATasks(c echo.Context) error {
	sessionID := c.QueryParam("acct_session_id")
	if sessionID == "" {
		sessionID = c.QueryParam("session_id")
	}
	tasks, err := radiusd.NewCoATaskManager(GetDB(c)).List(c.Request().Context(), sessionID, 200)
	if err != nil {
		return fail(c, 500, "DATABASE_ERROR", "Failed to list CoA tasks", err.Error())
	}
	return ok(c, tasks)
}

// RetryCoATasks redelivers unfinished tasks with their original Identifier.
func RetryCoATasks(c echo.Context) error {
	client := radiusd.NewCoAService(nil,
		radiusd.WithCoATimeout(3*time.Second), radiusd.WithCoARetries(0))
	sent, err := radiusd.NewCoATaskManager(GetDB(c)).RetryPending(c.Request().Context(), client, 50)
	if err != nil {
		return fail(c, 500, "RETRY_FAILED", "Failed to retry CoA tasks", err.Error())
	}
	return ok(c, map[string]interface{}{"retried": sent})
}

// registerCoATaskRoutes exposes durable CoA task history and restart retry.
func registerCoATaskRoutes() {
	webserver.ApiGET("/coa-tasks", ListCoATasks)
	webserver.ApiPOST("/coa-tasks/retry", RetryCoATasks, requireAdmin())
}
