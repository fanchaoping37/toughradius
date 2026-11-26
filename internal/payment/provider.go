// Package payment implements channel adapters for WeChat Pay, Alipay and
// campus card (一卡通) systems. It provides signature verification and
// callback normalisation; actual network calls are deliberately kept behind
// the Provider interface so credentials never enter logs or the database.
//
// Authoritative references:
//   - WeChat Pay v3 callback signature:
//     https://pay.weixin.qq.com/wiki/doc/apiv3/apis/chapter3_5_5.shtml
//     Sign string = timestamp\nnonce\nbody\n ; RSA-SHA256 with platform cert.
//     Resource ciphertext: AES-256-GCM, key = APIv3 key,
//     nonce/associated_data from the resource object.
//   - Alipay callback signature (RSA2):
//     https://opendocs.alipay.com/common/02kkv7
//     sign = RSA-SHA256 over sorted parameters (excluding sign/sign_type).
//   - WeChat legacy v2 MD5/HMAC-SHA256 sign (for compatibility):
//     https://pay.weixin.qq.com/wiki/doc/api/native.php?chapter=4_3
package payment

import (
	"crypto"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidSign    = errors.New("payment: invalid signature")
	ErrMissingField   = errors.New("payment: missing required field")
	ErrUnsupported    = errors.New("payment: unsupported channel")
	ErrAlreadyHandled = errors.New("payment: duplicate notification")
)

// Result is the normalised outcome of a verified channel callback.
type Result struct {
	Channel    string
	TxnID      string
	OrderNo    string
	AmountFen  int64
	Paid       bool
	Raw        string
	VerifiedAt time.Time
}

// Provider verifies and normalises one payment channel.
type Provider interface {
	Channel() string
	Verify(r *http.Request, body []byte) (*Result, error)
}

// Config holds channel credentials; empty credentials disable that channel.
type Config struct {
	WeChatAppID     string
	WeChatMchID     string
	WeChatAPIv3Key  string // 32 bytes for AES-256-GCM
	WeChatCertPEM   string // WeChat platform certificate (PEM) for RSA-SHA256
	WeChatLegacyKey string // v2 MD5/HMAC key, optional
	AlipayAppID     string
	AlipayPublicKey string // PEM PKIX public key or raw base64 DER
	CardEndpoint    string // campus card HTTP endpoint
	CardSecret      string // HMAC secret for campus card callbacks
}

// Registry holds configured providers by channel name.
type Registry struct {
	config Config
}

func NewRegistry(cfg Config) *Registry { return &Registry{config: cfg} }

// Get returns a provider for a channel, or ErrUnsupported when not configured.
func (r *Registry) Get(channel string) (Provider, error) {
	switch strings.ToLower(channel) {
	case "wechat", "weixin":
		if r.config.WeChatCertPEM == "" || r.config.WeChatAPIv3Key == "" {
			return nil, ErrUnsupported
		}
		return &WeChatProvider{cfg: r.config}, nil
	case "alipay", "zhifubao":
		if r.config.AlipayPublicKey == "" {
			return nil, ErrUnsupported
		}
		return &AlipayProvider{cfg: r.config}, nil
	case "card", "yikatong", "campus":
		if r.config.CardSecret == "" {
			return nil, ErrUnsupported
		}
		return &CardProvider{cfg: r.config}, nil
	default:
		return nil, ErrUnsupported
	}
}

// ParsePEMPublicKey parses an RSA public key from PKIX PEM or base64 DER.
func ParsePEMPublicKey(pemOrBase64 string) (*rsa.PublicKey, error) {
	value := strings.TrimSpace(pemOrBase64)
	if strings.Contains(value, "BEGIN PUBLIC KEY") {
		block := value
		if idx := strings.Index(block, "-----BEGIN"); idx > 0 {
			block = block[idx:]
		}
		der, err := base64.StdEncoding.DecodeString(decodePEMBody(block))
		if err != nil {
			return nil, fmt.Errorf("payment: decode PEM body: %w", err)
		}
		pub, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return nil, fmt.Errorf("payment: parse PEM: %w", err)
		}
		rsaPub, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("payment: not an RSA public key")
		}
		return rsaPub, nil
	}
	der, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("payment: decode public key: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("payment: parse DER: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("payment: not an RSA public key")
	}
	return rsaPub, nil
}

func decodePEMBody(pem string) string {
	lines := strings.Split(pem, "\n")
	var body strings.Builder
	inBody := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "-----BEGIN") {
			inBody = true
			continue
		}
		if strings.HasPrefix(line, "-----END") {
			break
		}
		if inBody {
			body.WriteString(line)
		}
	}
	return body.String()
}

// verifyRSASHA256 verifies a base64 RSA-SHA256 signature over data.
func verifyRSASHA256(pub *rsa.PublicKey, data []byte, sigBase64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(sigBase64)
	if err != nil {
		return false
	}
	hash := sha256.Sum256(data)
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, hash[:], sig) == nil
}

// signMD5Legacy implements WeChat Pay v2 sign: MD5(sorted params + &key=KEY).
func signMD5Legacy(params map[string]string, key string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k != "sign" && params[k] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(params[k])
		b.WriteString("&")
	}
	b.WriteString("key=")
	b.WriteString(key)
	sum := md5.Sum([]byte(b.String()))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// signHMACSHA256 implements a generic HMAC-SHA256 hex signature used by
// several campus card vendors.
func signHMACSHA256(secret string, parts ...string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	for _, part := range parts {
		mac.Write([]byte(part))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// parseAmountFen converts various amount representations to fen.
func parseAmountFen(value string) (int64, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, ErrMissingField
	}
	if strings.Contains(v, ".") {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, err
		}
		return int64(f * 100), nil
	}
	return strconv.ParseInt(v, 10, 64)
}

// jsonMap decodes a JSON object body.
func jsonMap(body []byte) (map[string]interface{}, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// stringField extracts a string field from a JSON map.
func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case string:
			return x
		case float64:
			return strconv.FormatInt(int64(x), 10)
		case json.Number:
			return x.String()
		}
	}
	return ""
}
