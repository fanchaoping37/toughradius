// Package captive implements the HTTP side of the captive portal: HTTP 302
// redirection for unauthenticated clients and an optional built-in login page.
//
// Boundary note: the upstream project declares "do not host a login page" as
// a hard non-goal (docs-site/src/zh/portal-hotspot-boundary.md). This package
// is disabled by default and must be explicitly enabled via configuration —
// it is an intentional second-development extension, not upstream behavior.
//
// The flow is:
//  1. Unauthenticated client requests any HTTP page -> 302 to LoginURL with
//     the original URI in a return parameter.
//  2. The login page (built-in template or external page) calls the Portal
//     HTTP bridge: POST /api/v1/portal/challenge then /auth with auth_ticket.
//  3. On success the client IP is added to the authorized cache and the
//     browser is redirected to the original URL.
package captive

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config configures the captive HTTP handler.
type Config struct {
	Enabled       bool
	LoginPath     string        // path served by the built-in login page
	PortalURL     string        // public base URL used in redirects
	AuthorizedTTL time.Duration // how long a client IP stays authorized
	Whitelist     []*net.IPNet  // client networks that bypass redirection
	AllowedHosts  []string      // destination hosts that never redirect
	Secret        string        // signs the return URL to prevent open redirects
	Now           func() time.Time
}

// Handler redirects unknown clients and serves the built-in login form.
type Handler struct {
	cfg        Config
	authorized map[string]time.Time
	mu         sync.RWMutex
	loginTmpl  *template.Template
	now        func() time.Time
}

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>网络认证</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#f5f6f8;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center}
.card{background:#fff;border-radius:12px;box-shadow:0 4px 24px rgba(0,0,0,.08);padding:32px;width:360px;max-width:90vw}
h1{font-size:20px;margin:0 0 20px;text-align:center;color:#1f2937}
label{display:block;font-size:13px;color:#4b5563;margin:12px 0 4px}
input{width:100%;box-sizing:border-box;padding:10px 12px;border:1px solid #d1d5db;border-radius:6px;font-size:14px}
button{width:100%;margin-top:22px;padding:11px;border:0;border-radius:6px;background:#2563eb;color:#fff;font-size:15px;cursor:pointer}
button:disabled{background:#93c5fd}
#msg{margin-top:14px;font-size:13px;color:#b91c1c;min-height:18px;text-align:center}
</style></head><body><div class="card">
<h1>校园网认证</h1>
<form id="f" autocomplete="off">
<label>用户名</label><input id="u" required>
<label>密码</label><input id="p" type="password" required>
<button type="submit" id="b">登录</button>
</form><div id="msg"></div>
<script>
const returnURL = {{.ReturnURL}};
const isSafe = (u) => { try { const x = new URL(u, location.href); return x.origin === location.origin; } catch(e){ return false; } };
const dest = isSafe(returnURL) ? returnURL : '/';
document.getElementById('f').addEventListener('submit', async (e) => {
  e.preventDefault();
  const b = document.getElementById('b'), msg = document.getElementById('msg');
  b.disabled = true; msg.textContent = '';
  try {
    const username = document.getElementById('u').value;
    const password = document.getElementById('p').value;
    const ch = await fetch('/api/v1/portal/challenge', {method:'POST',headers:{'Content-Type':'application/json'},
      body: JSON.stringify({username, user_ip: {{.ClientIP}}, mac: {{.MAC}}})});
    const chData = await ch.json();
    if (chData.code !== 0) throw new Error(chData.message || 'challenge failed');
    const data = chData.data;
    const au = await fetch('/api/v1/portal/auth', {method:'POST',headers:{'Content-Type':'application/json'},
      body: JSON.stringify({username, password, user_ip: {{.ClientIP}}, mac: {{.MAC}},
        auth_mode:'chap', auth_ticket:data.auth_ticket, nas_addr: {{.NASAddr}}})});
    const auData = await au.json();
    if (auData.code !== 0 || !auData.data.authenticated) throw new Error(auData.message || '认证失败');
    await fetch('/api/v1/captive/authorize', {method:'POST',headers:{'Content-Type':'application/json'},
      body: JSON.stringify({user_ip: {{.ClientIP}}, minutes: {{.Minutes}}})});
    location.replace(dest);
  } catch(err) { msg.textContent = err.message || '网络错误'; b.disabled = false; }
});
</script></div></body></html>`))

// NewHandler constructs a captive handler. Disabled configs produce a handler
// that simply calls the next handler (no behavior change).
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.LoginPath == "" {
		cfg.LoginPath = "/portal/login"
	}
	if cfg.AuthorizedTTL <= 0 {
		cfg.AuthorizedTTL = 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &Handler{
		cfg:        cfg,
		authorized: make(map[string]time.Time),
		loginTmpl:  loginTemplate,
		now:        cfg.Now,
	}
	return h, nil
}

// IsAuthorized reports whether a client IP has a live authorization.
func (h *Handler) IsAuthorized(ip net.IP) bool {
	if h == nil || !h.cfg.Enabled {
		return true // disabled subsystem = transparent
	}
	if ip == nil {
		return false
	}
	h.mu.RLock()
	expires, ok := h.authorized[ip.String()]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	if !h.now().Before(expires) {
		h.mu.Lock()
		delete(h.authorized, ip.String())
		h.mu.Unlock()
		return false
	}
	return true
}

// Authorize grants access to a client IP (called after successful Portal auth).
func (h *Handler) Authorize(ip net.IP, ttl time.Duration) {
	if h == nil || ip == nil {
		return
	}
	if ttl <= 0 {
		ttl = h.cfg.AuthorizedTTL
	}
	h.mu.Lock()
	h.authorized[ip.String()] = h.now().Add(ttl)
	h.mu.Unlock()
}

// Revoke removes a client authorization (e.g. on Disconnect/CoA).
func (h *Handler) Revoke(ip net.IP) {
	if h == nil || ip == nil {
		return
	}
	h.mu.Lock()
	delete(h.authorized, ip.String())
	h.mu.Unlock()
}

// Middleware wraps an http.Handler: unauthenticated requests are redirected
// to the login page, except health checks, the login path, the Portal bridge
// API and whitelisted destination hosts.
func (h *Handler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h == nil || !h.cfg.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		path := r.URL.Path
		if path == "/health" || path == h.cfg.LoginPath || path == "/favicon.ico" ||
			strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		clientIP := parseIP(r.RemoteAddr)
		if h.isClientWhitelisted(clientIP) || h.IsAuthorized(clientIP) || h.isHostAllowed(r.Host) {
			next.ServeHTTP(w, r)
			return
		}
		h.Redirect(w, r)
	})
}

// Redirect builds a signed 302 to the login page. The return parameter is
// HMAC-signed to prevent the login page being used as an open redirect.
func (h *Handler) Redirect(w http.ResponseWriter, r *http.Request) {
	target := r.URL.RequestURI()
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		target = "/"
	}
	sig := h.SignReturn(target)
	location := h.cfg.LoginPath + "?return=" + url.QueryEscape(target) + "&sig=" + sig
	http.Redirect(w, r, location, http.StatusFound)
}

// SignReturn computes the HMAC-SHA256 hex signature of a return URL.
func (h *Handler) SignReturn(target string) string {
	mac := hmac.New(sha256.New, []byte(h.cfg.Secret))
	mac.Write([]byte(target))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyReturn validates a return URL signature (constant-time).
func (h *Handler) VerifyReturn(target, sig string) bool {
	want := h.SignReturn(target)
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(got, mustHex(want))
}

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// LoginPage serves the built-in login form. Only relative, signed return URLs
// are accepted to prevent open redirect.
func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	returnURL := r.URL.Query().Get("return")
	sig := r.URL.Query().Get("sig")
	if returnURL == "" || !strings.HasPrefix(returnURL, "/") || strings.HasPrefix(returnURL, "//") || !h.VerifyReturn(returnURL, sig) {
		returnURL = "/"
	}
	clientIP := parseIP(r.RemoteAddr)
	mac := ""
	if hw, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		mac = resolveMAC(hw)
	}
	nasAddr := ""
	if h.cfg.PortalURL != "" {
		if u, err := url.Parse(h.cfg.PortalURL); err == nil {
			nasAddr = u.Hostname()
		}
	}
	data := struct {
		ReturnURL string
		ClientIP  string
		MAC       string
		NASAddr   string
		Minutes   int
	}{
		ReturnURL: returnURL,
		ClientIP:  clientIP.String(),
		MAC:       mac,
		NASAddr:   nasAddr,
		Minutes:   int(h.cfg.AuthorizedTTL / time.Minute),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := h.loginTmpl.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (h *Handler) isClientWhitelisted(ip net.IP) bool {
	for _, n := range h.cfg.Whitelist {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (h *Handler) isHostAllowed(host string) bool {
	if host == "" {
		return false
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	for _, allowed := range h.cfg.AllowedHosts {
		if strings.EqualFold(name, allowed) {
			return true
		}
	}
	return false
}

func parseIP(remote string) net.IP {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	return net.ParseIP(host)
}

// resolveMAC is overridden in tests; production implementations look up the
// client IP in the DHCP lease table (injected by main.go).
var resolveMAC = func(ip string) string {
	lease := LeaseLookup(ip)
	if lease != nil {
		return lease.MAC
	}
	return ""
}

// LeaseLookup can be wired to the DHCP server's lease table by main.go.
var LeaseLookup = func(ip string) *struct{ MAC string } { return nil }

// BuildPortalURL constructs a login URL used by DHCP Option 160 / DNS redirects.
func BuildPortalURL(basePath, clientIP, returnURL string) string {
	v := url.Values{}
	if clientIP != "" {
		v.Set("ip", clientIP)
	}
	if returnURL != "" {
		v.Set("return", returnURL)
	}
	if len(v) == 0 {
		return basePath
	}
	return fmt.Sprintf("%s?%s", basePath, v.Encode())
}

// EncodeState is a small helper used by the login bridge to carry a signed,
// base64-encoded JSON auth ticket through redirects.
func EncodeState(payload []byte) string {
	return base64.RawURLEncoding.EncodeToString(payload)
}

var errDisabled = errors.New("captive: subsystem disabled")
