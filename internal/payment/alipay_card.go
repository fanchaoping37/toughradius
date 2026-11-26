package payment

// Alipay (RSA2) and campus card (一卡通) callback verification.
//
// Alipay: https://opendocs.alipay.com/common/02kkv7
//   - Remove sign and sign_type from the parameter map.
//   - Sort remaining keys lexicographically, build "k=v&k2=v2" (no trailing &).
//   - Verify base64 RSA-SHA256 signature with the Alipay public key.
//
// Campus card: vendor-neutral HMAC-SHA256 adapter. The signed string is
// "method\ncard_no\namount\ntimestamp\nnonce" with fields supplied by the
// integration; it is a deployment convention, not a published standard.

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AlipayProvider verifies Alipay RSA2 notifications.
type AlipayProvider struct {
	cfg Config
}

func (p *AlipayProvider) Channel() string { return "alipay" }

func (p *AlipayProvider) Verify(r *http.Request, body []byte) (*Result, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	values := make(map[string]string, len(r.Form))
	for k, vals := range r.Form {
		if len(vals) > 0 {
			values[k] = vals[0]
		}
	}
	if len(values) == 0 {
		// Support JSON body for modern Alipay gateways.
		m, err := jsonMap(body)
		if err != nil {
			return nil, err
		}
		for k := range m {
			values[k] = stringField(m, k)
		}
	}
	sign := values["sign"]
	signType := strings.ToUpper(values["sign_type"])
	if sign == "" || (signType != "" && signType != "RSA2") {
		return nil, ErrInvalidSign
	}
	pub, err := ParsePEMPublicKey(p.cfg.AlipayPublicKey)
	if err != nil {
		return nil, err
	}
	delete(values, "sign")
	delete(values, "sign_type")
	signString := buildAlipaySignString(values)
	if !verifyRSASHA256(pub, []byte(signString), sign) {
		return nil, ErrInvalidSign
	}
	// app_id must match the configured merchant to prevent cross-app replay.
	if appID := values["app_id"]; appID != "" && p.cfg.AlipayAppID != "" && appID != p.cfg.AlipayAppID {
		return nil, ErrInvalidSign
	}
	total, err := parseAmountFen(values["total_amount"])
	if err != nil {
		return nil, err
	}
	tradeStatus := values["trade_status"]
	return &Result{
		Channel:    "alipay",
		TxnID:      values["trade_no"],
		OrderNo:    values["out_trade_no"],
		AmountFen:  total,
		Paid:       tradeStatus == "TRADE_SUCCESS" || tradeStatus == "TRADE_FINISHED",
		Raw:        string(body),
		VerifiedAt: nowTime(),
	}, nil
}

// buildAlipaySignString sorts parameters and joins k=v with '&'.
func buildAlipaySignString(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}

// CardProvider verifies campus-card (一卡通) HMAC callbacks.
type CardProvider struct {
	cfg Config
}

func (p *CardProvider) Channel() string { return "card" }

func (p *CardProvider) Verify(r *http.Request, body []byte) (*Result, error) {
	m, err := jsonMap(body)
	if err != nil {
		return nil, err
	}
	method := stringField(m, "method")
	cardNo := stringField(m, "card_no")
	amount := stringField(m, "amount")
	timestamp := stringField(m, "timestamp")
	nonce := stringField(m, "nonce")
	sign := stringField(m, "sign")
	if method == "" || cardNo == "" || amount == "" || sign == "" {
		return nil, ErrMissingField
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || absInt64(nowUnix()-ts) > 300 {
		return nil, ErrInvalidSign
	}
	want := signHMACSHA256(p.cfg.CardSecret, method, cardNo, amount, timestamp, nonce)
	if !hmacEqual(want, sign) {
		return nil, ErrInvalidSign
	}
	amountFen, err := parseAmountFen(amount)
	if err != nil {
		return nil, err
	}
	return &Result{
		Channel:    "card",
		TxnID:      cardNo + ":" + nonce,
		OrderNo:    stringField(m, "order_no"),
		AmountFen:  amountFen,
		Paid:       true, // card deduction is synchronous
		Raw:        string(body),
		VerifiedAt: nowTime(),
	}, nil
}

// --- small helpers kept local to avoid leaking crypto choices to callers ---

var nowFunc = time.Now

func nowUnix() int64     { return nowFunc().Unix() }
func nowTime() time.Time { return nowFunc() }

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func base64Decode(value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	decoded, err = base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("payment: invalid base64")
	}
	return decoded, nil
}

func hmacEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// EncodeForm is used by tests to build an Alipay-style form body.
func EncodeForm(params map[string]string) string {
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	return values.Encode()
}
