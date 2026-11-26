package captive

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testHandler(t *testing.T) (*Handler, *time.Time) {
	return testHandlerAt(t, time.Now())
}

func testHandlerAt(t *testing.T, start time.Time) (*Handler, *time.Time) {
	t.Helper()
	now := start
	h, err := NewHandler(Config{
		Enabled:       true,
		LoginPath:     "/portal/login",
		PortalURL:     "http://10.0.0.1/login",
		AuthorizedTTL: time.Hour,
		Secret:        "test-secret",
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, &now
}

func TestDisabledHandlerIsTransparent(t *testing.T) {
	h, _ := NewHandler(Config{Enabled: false})
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
	rec := httptest.NewRecorder()
	h.Middleware(next).ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("disabled handler blocked request: called=%v code=%d", called, rec.Code)
	}
	if !h.IsAuthorized(net.ParseIP("1.2.3.4")) {
		t.Fatal("disabled handler should treat all clients as authorized")
	}
}

func TestRedirectUnauthenticated(t *testing.T) {
	h, _ := testHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/original?q=1", nil)
	req.RemoteAddr = "192.168.1.10:12345"
	rec := httptest.NewRecorder()
	h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler must not be called")
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d", rec.Code)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/portal/login?return=") {
		t.Fatalf("location = %s", location)
	}
	if !strings.Contains(location, "sig=") {
		t.Fatal("signature missing")
	}
}

func TestAuthorizedClientPasses(t *testing.T) {
	h, _ := testHandler(t)
	h.Authorize(net.ParseIP("192.168.1.10"), 0)
	called := false
	req := httptest.NewRequest(http.MethodGet, "http://example.com/page", nil)
	req.RemoteAddr = "192.168.1.10:12345"
	rec := httptest.NewRecorder()
	h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })).ServeHTTP(rec, req)
	if !called {
		t.Fatal("authorized client was redirected")
	}
}

func TestAuthorizeExpiryAndRevoke(t *testing.T) {
	h, nowPtr := testHandlerAt(t, time.Now())
	h.Authorize(net.ParseIP("10.0.0.5"), time.Minute)
	if !h.IsAuthorized(net.ParseIP("10.0.0.5")) {
		t.Fatal("just authorized IP not authorized")
	}
	*nowPtr = nowPtr.Add(2 * time.Minute) // advance injected clock
	if h.IsAuthorized(net.ParseIP("10.0.0.5")) {
		t.Fatal("expired authorization still valid")
	}
	h.Authorize(net.ParseIP("10.0.0.6"), 0)
	h.Revoke(net.ParseIP("10.0.0.6"))
	if h.IsAuthorized(net.ParseIP("10.0.0.6")) {
		t.Fatal("revoked authorization still valid")
	}
}

func TestReturnSignature(t *testing.T) {
	h, _ := testHandler(t)
	sig := h.SignReturn("/original?q=1")
	if !h.VerifyReturn("/original?q=1", sig) {
		t.Fatal("valid signature rejected")
	}
	if h.VerifyReturn("/original?q=2", sig) {
		t.Fatal("tampered return accepted")
	}
	if h.VerifyReturn("/original?q=1", "deadbeef") {
		t.Fatal("invalid hex accepted")
	}
}

func TestLoginPageRendersAndRejectsOpenRedirect(t *testing.T) {
	h, _ := testHandler(t)
	validSig := h.SignReturn("/home")
	req := httptest.NewRequest(http.MethodGet, "/portal/login?return=%2Fhome&sig="+validSig, nil)
	req.RemoteAddr = "192.168.1.20:4444"
	rec := httptest.NewRecorder()
	h.LoginPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "const returnURL = \"/home\"") {
		t.Fatal("return URL not embedded")
	}
	if !strings.Contains(body, "/api/v1/portal/challenge") {
		t.Fatal("login JS missing challenge call")
	}
	if !strings.Contains(body, "auth_ticket") {
		t.Fatal("login JS missing ticket forwarding")
	}

	// Unsigned absolute URL must be replaced with "/".
	req = httptest.NewRequest(http.MethodGet, "/portal/login?return=http%3A%2F%2Fevil.example&sig=x", nil)
	rec = httptest.NewRecorder()
	h.LoginPage(rec, req)
	if strings.Contains(rec.Body.String(), "evil.example") {
		t.Fatal("open redirect not blocked")
	}
}

func TestWhitelistAndAllowedHosts(t *testing.T) {
	_, net1, _ := net.ParseCIDR("10.0.0.0/8")
	h, _ := NewHandler(Config{
		Enabled: true, Secret: "s",
		Whitelist:    []*net.IPNet{net1},
		AllowedHosts: []string{"health.internal"},
	})
	if !h.isClientWhitelisted(net.ParseIP("10.5.5.5")) || h.isClientWhitelisted(net.ParseIP("192.168.1.1")) {
		t.Fatal("whitelist classifier wrong")
	}
	if !h.isHostAllowed("health.internal:8080") || h.isHostAllowed("example.com") {
		t.Fatal("allowed hosts classifier wrong")
	}
}

func TestBuildPortalURL(t *testing.T) {
	got := BuildPortalURL("http://portal/login", "1.2.3.4", "/home")
	if !strings.Contains(got, "ip=1.2.3.4") || !strings.Contains(got, "return=") {
		t.Fatalf("portal URL = %s", got)
	}
}
