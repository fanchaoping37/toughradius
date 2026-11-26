package adminapi

import (
	"net"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/talkincode/toughradius/v9/internal/webserver"
)

// captiveGrantHandler is implemented by the captive module; adminapi uses an
// injected function to avoid a compile-time dependency on the optional module.
var captiveAuthorize func(ip net.IP, ttl time.Duration) bool

// SetCaptiveAuthorizer installs the callback used by /captive/authorize.
func SetCaptiveAuthorizer(fn func(net.IP, time.Duration) bool) { captiveAuthorize = fn }

type captiveAuthorizePayload struct {
	UserIP  string `json:"user_ip" validate:"required,ip"`
	Minutes int    `json:"minutes"`
}

// CaptiveAuthorize grants a short-lived authorization to a client IP after a
// successful Portal authentication. Called by the built-in login page and the
// mini program; the Portal auth_ticket has already authenticated the user.
func CaptiveAuthorize(c echo.Context) error {
	if captiveAuthorize == nil {
		return fail(c, http.StatusServiceUnavailable, "CAPTIVE_DISABLED", "Captive portal module is not enabled", nil)
	}
	var payload captiveAuthorizePayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request parameters", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	ip := net.ParseIP(payload.UserIP)
	if ip == nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Invalid user_ip", nil)
	}
	ttl := time.Duration(payload.Minutes) * time.Minute
	if ttl <= 0 || ttl > 24*time.Hour {
		ttl = 24 * time.Hour
	}
	if !captiveAuthorize(ip, ttl) {
		return fail(c, http.StatusInternalServerError, "CAPTIVE_ERROR", "Failed to grant authorization", nil)
	}
	return ok(c, map[string]interface{}{
		"user_ip":    payload.UserIP,
		"expires_in": int(ttl.Seconds()),
	})
}

func registerCaptiveRoutes() {
	webserver.ApiPOST("/captive/authorize", CaptiveAuthorize)
}
