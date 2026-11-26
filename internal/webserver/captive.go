package webserver

import (
	"net"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// CaptiveRedirector is thesubset of the captive module needed by the HTTP
// layer. main.go installs the *captive.Handler when the module is enabled.
type CaptiveRedirector interface {
	IsAuthorized(ip net.IP) bool
	Redirect(w http.ResponseWriter, r *http.Request)
	LoginPage(w http.ResponseWriter, r *http.Request)
}

var captiveRedirector CaptiveRedirector

// SetCaptiveRedirector installs the optional captive portal redirector.
// Passing nil disables HTTP 302 redirection.
func SetCaptiveRedirector(h CaptiveRedirector) { captiveRedirector = h }

// captiveMiddleware redirects unauthenticated browsers to the login page.
// API paths and static assets are never redirected so the Portal bridge can
// still complete authentication.
func captiveMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	if captiveRedirector == nil {
		return next
	}
	return func(c echo.Context) error {
		path := c.Request().URL.Path
		if path == "/health" || path == "/ready" || path == "/realip" ||
			path == "/favicon.ico" ||
			strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/static/") {
			return next(c)
		}
		clientIP := net.ParseIP(c.RealIP())
		if captiveRedirector.IsAuthorized(clientIP) {
			return next(c)
		}
		if path == "/portal/login" {
			captiveRedirector.LoginPage(c.Response().Writer, c.Request())
			return nil
		}
		captiveRedirector.Redirect(c.Response().Writer, c.Request())
		return nil
	}
}
