package payment

// WeChat Pay v3 notification verification.
//
// Wire format (official docs, see provider.go header):
//
//	Headers: Wechatpay-Serial, Wechatpay-Signature, Wechatpay-Timestamp,
//	         Wechatpay-Nonce
//	Signature string: timestamp\nnonce\nbody\n
//	Algorithm: RSA-SHA256 with the WeChat platform certificate.
//	Resource: {algorithm: "AEAD_AES_256_GCM", ciphertext, associated_data, nonce}
//	AES key = 32-byte APIv3 key; AAD = associated_data.
//
// The resource plaintext contains: out_trade_no, transaction_id, amount.total
// (fen), trade_state (SUCCESS etc.).

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// WeChatProvider verifies WeChat Pay v3 callbacks.
type WeChatProvider struct {
	cfg Config
}

func (p *WeChatProvider) Channel() string { return "wechat" }

func (p *WeChatProvider) Verify(r *http.Request, body []byte) (*Result, error) {
	timestamp := r.Header.Get("Wechatpay-Timestamp")
	nonce := r.Header.Get("Wechatpay-Nonce")
	signature := r.Header.Get("Wechatpay-Signature")
	if timestamp == "" || nonce == "" || signature == "" {
		return nil, ErrMissingField
	}
	// RFC 7230/WeChat: reject stale notifications (5-minute window).
	var ts int64
	if _, err := fmt.Sscan(timestamp, &ts); err != nil {
		return nil, ErrInvalidSign
	}
	if delta := nowUnix() - ts; delta > 300 || delta < -300 {
		return nil, fmt.Errorf("%w: timestamp outside window", ErrInvalidSign)
	}

	pub, err := ParsePEMPublicKey(p.cfg.WeChatCertPEM)
	if err != nil {
		return nil, err
	}
	signString := timestamp + "\n" + nonce + "\n" + string(body) + "\n"
	if !verifyRSASHA256(pub, []byte(signString), signature) {
		return nil, ErrInvalidSign
	}

	envelope, err := jsonMap(body)
	if err != nil {
		return nil, err
	}
	resource, ok := envelope["resource"].(map[string]interface{})
	if !ok {
		return nil, ErrMissingField
	}
	if strings.ToUpper(stringField(resource, "algorithm")) != "AEAD_AES_256_GCM" {
		return nil, fmt.Errorf("%w: unsupported algorithm", ErrUnsupported)
	}
	plaintext, err := decryptAESGCM(
		[]byte(p.cfg.WeChatAPIv3Key),
		stringField(resource, "nonce"),
		stringField(resource, "associated_data"),
		stringField(resource, "ciphertext"),
	)
	if err != nil {
		return nil, err
	}
	detail, err := jsonMap(plaintext)
	if err != nil {
		return nil, err
	}
	amount, _ := detail["amount"].(map[string]interface{})
	total, err := parseAmountFen(stringField(amount, "total"))
	if err != nil {
		return nil, err
	}
	state := stringField(detail, "trade_state")
	return &Result{
		Channel:    "wechat",
		TxnID:      stringField(detail, "transaction_id"),
		OrderNo:    stringField(detail, "out_trade_no"),
		AmountFen:  total,
		Paid:       state == "SUCCESS",
		Raw:        string(body),
		VerifiedAt: nowTime(),
	}, nil
}

// decryptAESGCM implements WeChat Pay v3 resource decryption:
// ciphertext is base64(raw: 16-byte tag appended after AES-GCM output).
func decryptAESGCM(key []byte, nonce, aad, ciphertextB64 string) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("payment: WeChat APIv3 key must be 32 bytes")
	}
	raw, err := base64Decode(ciphertextB64)
	if err != nil {
		return nil, err
	}
	if len(raw) < 16 {
		return nil, errors.New("payment: ciphertext too short")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// WeChat sends nonce as ASCII string; GCM standard nonce is 12 bytes.
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("payment: invalid GCM nonce length %d", len(nonce))
	}
	return gcm.Open(nil, []byte(nonce), raw, []byte(aad))
}

// VerifyLegacyV2 verifies WeChat Pay v2 (MD5 or HMAC-SHA256) sign.
// Kept for legacy merchants that have not migrated to v3.
func VerifyLegacyV2(params map[string]string, body []byte) bool {
	sign := strings.ToUpper(params["sign"])
	if sign == "" {
		return false
	}
	key := ""
	if v, ok := params["_key"]; ok {
		key = v
	}
	return hmac.Equal([]byte(signMD5Legacy(params, key)), []byte(sign))
}
