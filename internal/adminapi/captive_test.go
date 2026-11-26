package adminapi

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptiveAuthorizeDisabled(t *testing.T) {
	old := captiveAuthorize
	captiveAuthorize = nil
	defer func() { captiveAuthorize = old }()

	e := setupTestEcho()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/captive/authorize",
		strings.NewReader(`{"user_ip":"192.168.1.10","minutes":30}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := CaptiveAuthorize(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "CAPTIVE_DISABLED")
}

func TestCaptiveAuthorizeSuccess(t *testing.T) {
	var calledIP net.IP
	var calledTTL time.Duration
	captiveAuthorize = func(ip net.IP, ttl time.Duration) bool {
		calledIP = ip
		calledTTL = ttl
		return true
	}
	defer func() { captiveAuthorize = nil }()

	e := setupTestEcho()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/captive/authorize",
		strings.NewReader(`{"user_ip":"192.168.1.20","minutes":45}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, CaptiveAuthorize(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "192.168.1.20", calledIP.String())
	assert.Equal(t, 45*time.Minute, calledTTL)
	assert.Contains(t, rec.Body.String(), "expires_in")
}

func TestCaptiveAuthorizeInvalidIP(t *testing.T) {
	captiveAuthorize = func(net.IP, time.Duration) bool { return true }
	defer func() { captiveAuthorize = nil }()
	e := setupTestEcho()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/captive/authorize",
		strings.NewReader(`{"user_ip":"not-an-ip"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, CaptiveAuthorize(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
