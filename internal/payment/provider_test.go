package payment

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testRSAKeyPair(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestParsePEMPublicKey(t *testing.T) {
	_, pemKey := testRSAKeyPair(t)
	if _, err := ParsePEMPublicKey(pemKey); err != nil {
		t.Fatal(err)
	}
	// raw base64 DER form
	body := pemKey
	body = strings.ReplaceAll(body, "-----BEGIN PUBLIC KEY-----", "")
	body = strings.ReplaceAll(body, "-----END PUBLIC KEY-----", "")
	body = strings.ReplaceAll(body, "\n", "")
	body = strings.TrimSpace(body)
	if _, err := ParsePEMPublicKey(body); err != nil {
		t.Fatalf("base64 DER: %v", err)
	}
}

func TestAlipayCallbackVerification(t *testing.T) {
	key, pemKey := testRSAKeyPair(t)
	params := map[string]string{
		"app_id":       "2021000000000001",
		"out_trade_no": "ORDER-123",
		"trade_no":     "ALI-999",
		"total_amount": "12.34",
		"trade_status": "TRADE_SUCCESS",
		"sign_type":    "RSA2",
		"charset":      "utf-8",
	}
	signString := buildAlipaySignString(map[string]string{
		"app_id": "2021000000000001", "out_trade_no": "ORDER-123",
		"trade_no": "ALI-999", "total_amount": "12.34",
		"trade_status": "TRADE_SUCCESS", "charset": "utf-8",
	})
	hash := sha256sum(signString)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, cryptoHash, hash)
	if err != nil {
		t.Fatal(err)
	}
	params["sign"] = base64.StdEncoding.EncodeToString(sig)
	body := EncodeForm(params)

	provider := &AlipayProvider{cfg: Config{
		AlipayAppID: "2021000000000001", AlipayPublicKey: pemKey,
	}}
	req := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	result, err := provider.Verify(req, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Paid || result.AmountFen != 1234 || result.OrderNo != "ORDER-123" || result.TxnID != "ALI-999" {
		t.Fatalf("result = %+v", result)
	}

	// Tamper with the amount -> signature must fail.
	body = strings.Replace(body, "12.34", "99.99", 1)
	req = httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := provider.Verify(req, []byte(body)); err == nil {
		t.Fatal("tampered callback accepted")
	}
}

func TestAlipaySignStringSorting(t *testing.T) {
	got := buildAlipaySignString(map[string]string{"b": "2", "a": "1", "c": "3"})
	if got != "a=1&b=2&c=3" {
		t.Fatalf("sign string = %q", got)
	}
}

func TestWeChatV3CallbackVerification(t *testing.T) {
	key, pemKey := testRSAKeyPair(t)
	apiV3Key := "0123456789abcdef0123456789abcdef"
	nonce := "0123456789abcdef0123456789abcdef"[:12]
	aad := "transaction"
	plaintext := `{"out_trade_no":"WX-1","transaction_id":"TX-1","trade_state":"SUCCESS","amount":{"total":500}}`

	block, err := aes.NewCipher([]byte(apiV3Key))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, []byte(nonce), []byte(plaintext), []byte(aad))
	envelope := map[string]interface{}{
		"resource": map[string]interface{}{
			"algorithm":       "AEAD_AES_256_GCM",
			"nonce":           nonce,
			"associated_data": aad,
			"ciphertext":      base64.StdEncoding.EncodeToString(sealed),
		},
	}
	bodyBytes := jsonBytes(envelope)
	timestamp := strconvI64(nowUnix())
	signString := timestamp + "\n" + nonce + "\n" + string(bodyBytes) + "\n"
	hash := sha256sum(signString)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, cryptoHash, hash)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(string(bodyBytes)))
	req.Header.Set("Wechatpay-Timestamp", timestamp)
	req.Header.Set("Wechatpay-Nonce", nonce)
	req.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))

	provider := &WeChatProvider{cfg: Config{WeChatCertPEM: pemKey, WeChatAPIv3Key: apiV3Key}}
	result, err := provider.Verify(req, bodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if result.OrderNo != "WX-1" || result.AmountFen != 500 || !result.Paid {
		t.Fatalf("result = %+v", result)
	}

	// Stale timestamp rejected.
	req.Header.Set("Wechatpay-Timestamp", "1")
	if _, err := provider.Verify(req, bodyBytes); err == nil {
		t.Fatal("stale timestamp accepted")
	}
}

func TestCardProviderHMAC(t *testing.T) {
	secret := "campus-secret"
	ts := strconvI64(nowUnix())
	fields := []string{"recharge", "CARD-123", "1000", ts, "nonce-1"}
	sign := signHMACSHA256(secret, fields...)
	body := `{"method":"recharge","card_no":"CARD-123","amount":"1000","timestamp":"` + ts + `","nonce":"nonce-1","order_no":"O-1","sign":"` + sign + `"}`
	req := httptest.NewRequest(http.MethodPost, "/card", strings.NewReader(body))
	provider := &CardProvider{cfg: Config{CardSecret: secret}}
	result, err := provider.Verify(req, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if result.OrderNo != "O-1" || result.AmountFen != 1000 || !result.Paid {
		t.Fatalf("result = %+v", result)
	}
	bad := strings.Replace(body, "1000", "2000", 1)
	if _, err := provider.Verify(req, []byte(bad)); err == nil {
		t.Fatal("tampered card callback accepted")
	}
}

func TestRegistryAndLegacy(t *testing.T) {
	r := NewRegistry(Config{})
	if _, err := r.Get("wechat"); err == nil {
		t.Fatal("unconfigured channel must be unsupported")
	}
	if _, err := r.Get("unknown"); err == nil {
		t.Fatal("unknown channel accepted")
	}
	if signMD5Legacy(map[string]string{"a": "1", "b": "2"}, "key") == "" {
		t.Fatal("legacy sign failed")
	}
	_ = time.Now()
}

// --- test helpers ---

var cryptoHash = crypto.SHA256

func sha256sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func strconvI64(v int64) string { return strconv.FormatInt(v, 10) }

func jsonBytes(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
