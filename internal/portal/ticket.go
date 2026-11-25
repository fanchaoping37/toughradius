package portal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TicketManager issues one-time, short-lived authentication tickets binding
// the external login page's challenge to a user IP (and optionally MAC).
// SRS §9.2 requires an auth_ticket with one-time use and short TTL so a stolen
// portal session cannot be replayed at the NAS authentication endpoint.
type TicketManager struct {
	secret []byte
	ttl    time.Duration
	mu     sync.Mutex
	used   map[string]time.Time
	lastGC time.Time
}

// NewTicketManager derives a domain-separated signing key from the server
// secret so Portal tickets cannot be confused with JWT or payment signatures.
func NewTicketManager(serverSecret string, ttl time.Duration) *TicketManager {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	mac := hmac.New(sha256.New, []byte("toughradius:portal-ticket:"+serverSecret))
	_, _ = mac.Write([]byte("v1"))
	return &TicketManager{secret: mac.Sum(nil), ttl: ttl, used: make(map[string]time.Time), lastGC: time.Now()}
}

type ticketClaims struct {
	Username string `json:"u"`
	UserIP   string `json:"i"`
	MAC      string `json:"m"`
	Serial   uint16 `json:"s"`
	Nonce    string `json:"n"`
	Expires  int64  `json:"e"`
}

// Issue signs a new ticket for a completed Challenge round trip.
func (m *TicketManager) Issue(username, userIP, mac string, serial uint16) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("portal: ticket nonce: %w", err)
	}
	claims := ticketClaims{
		Username: username, UserIP: userIP, MAC: mac, Serial: serial,
		Nonce: hex.EncodeToString(nonce), Expires: time.Now().Add(m.ttl).UnixNano(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + m.sign(body), nil
}

func (m *TicketManager) sign(body string) string {
	mac := hmac.New(sha256.New, m.secret)
	_, _ = mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks the signature, TTL, claim binding and one-time use. On success
// it consumes the ticket; any later replay is rejected.
func (m *TicketManager) Verify(token, username, userIP, mac string) (uint16, error) {
	if token == "" {
		return 0, errors.New("portal: auth_ticket is required")
	}
	parts := splitToken(token)
	if len(parts) != 2 {
		return 0, errors.New("portal: malformed auth_ticket")
	}
	expected := m.sign(parts[0])
	if !hmac.Equal([]byte(expected), []byte(parts[1])) {
		return 0, errors.New("portal: invalid auth_ticket signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, errors.New("portal: malformed auth_ticket payload")
	}
	var claims ticketClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0, errors.New("portal: malformed auth_ticket claims")
	}
	if time.Now().UnixNano() > claims.Expires {
		return 0, errors.New("portal: auth_ticket expired")
	}
	if claims.Username != username || claims.UserIP != userIP || claims.MAC != mac {
		return 0, errors.New("portal: auth_ticket does not match this request")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.lastGC.Add(time.Minute).Before(now) {
		for key, usedAt := range m.used {
			if usedAt.Add(m.ttl).Before(now) {
				delete(m.used, key)
			}
		}
		m.lastGC = now
	}
	if _, exists := m.used[token]; exists {
		return 0, errors.New("portal: auth_ticket already consumed")
	}
	m.used[token] = now
	return claims.Serial, nil
}

func splitToken(token string) []string {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			return []string{token[:i], token[i+1:]}
		}
	}
	return nil
}
