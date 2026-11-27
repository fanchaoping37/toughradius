package payment

// Gateway helpers shared by payment callbacks.
//
// The payment package deliberately does not import config: application
// configuration is converted to payment.Config by the caller (see
// adminapi.PaymentNotify). This keeps credentials and channel verification
// independent of the rest of the application's configuration layout.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrNoChannel is returned when a callback carries no channel hint and the
// application has no default channel configured. Callers must fail closed.
var ErrNoChannel = errors.New("payment: no channel in request and no default channel configured")

// NormalizeChannel maps the channel aliases used by clients and configuration
// to the canonical names understood by Registry. Unknown channels return
// ErrUnsupported rather than silently falling through to another provider.
func NormalizeChannel(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "wechat", "weixin", "wx":
		return "wechat", nil
	case "alipay", "zhifubao":
		return "alipay", nil
	case "card", "yikatong", "campus":
		return "card", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupported, raw)
	}
}

// ChannelFromRequest selects the verification channel for a callback.
//
// Resolution order (first match wins):
//  1. `channel` query parameter or X-Payment-Channel / Channel header
//  2. Wechatpay-Signature header (WeChat v3 callbacks) => wechat
//  3. channel/app_id/characteristic fields in the form or JSON body
//  4. the configured default channel
//
// Only the channel is selected here; the provider still verifies the signature
// and (for Alipay) the app_id, so a misleading hint cannot bypass verification.
// When nothing identifies a channel and no default is configured, ErrNoChannel
// is returned.
func ChannelFromRequest(r *http.Request, body []byte, defaultChannel string) (string, error) {
	if r != nil {
		if ch := r.URL.Query().Get("channel"); ch != "" {
			return NormalizeChannel(ch)
		}
		for _, header := range []string{"X-Payment-Channel", "Channel"} {
			if ch := r.Header.Get(header); ch != "" {
				return NormalizeChannel(ch)
			}
		}
		// WeChat v3 always signs with the platform certificate and sends this
		// header, so it is an unambiguous channel hint.
		if r.Header.Get("Wechatpay-Signature") != "" {
			return "wechat", nil
		}
	}

	if ch := channelFromBody(body); ch != "" {
		return NormalizeChannel(ch)
	}
	if strings.TrimSpace(defaultChannel) != "" {
		return NormalizeChannel(defaultChannel)
	}
	return "", ErrNoChannel
}

// channelFromBody inspects the already-read body without consuming r.Body, so
// the provider can still call ParseForm() on a restored request body.
func channelFromBody(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var m map[string]interface{}
	if json.Unmarshal(body, &m) == nil {
		if ch := stringField(m, "channel"); ch != "" {
			return ch
		}
		// WeChat v3 wraps the payload in a resource envelope.
		if _, ok := m["resource"]; ok {
			return "wechat"
		}
		if _, ok := m["card_no"]; ok {
			return "card"
		}
		if stringField(m, "sign_type") == "RSA2" || stringField(m, "trade_no") != "" {
			return "alipay"
		}
		if id := firstStringField(m, "appid", "app_id", "mch_id"); id != "" {
			return channelFromAppID(id)
		}
		return ""
	}

	// Form-encoded bodies (Alipay, some WeChat v2 integrations).
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return ""
	}
	if ch := values.Get("channel"); ch != "" {
		return ch
	}
	if strings.EqualFold(values.Get("sign_type"), "RSA2") || values.Get("trade_no") != "" {
		return "alipay"
	}
	if values.Get("card_no") != "" {
		return "card"
	}
	if id := values.Get("appid"); id != "" {
		return channelFromAppID(id)
	}
	if id := values.Get("app_id"); id != "" {
		return channelFromAppID(id)
	}
	return ""
}

// channelFromAppID uses the conventional app id shapes only as a hint: Alipay
// ids are numeric, WeChat ids start with "wx". The provider re-checks the
// configured app id (Alipay) or the certificate signature (WeChat).
func channelFromAppID(appID string) string {
	id := strings.TrimSpace(appID)
	switch {
	case strings.HasPrefix(strings.ToLower(id), "wx"):
		return "wechat"
	case id != "" && isNumeric(id):
		return "alipay"
	default:
		return ""
	}
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value := stringField(m, key); value != "" {
			return value
		}
	}
	return ""
}
