package payment

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNormalizeChannel(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  error
	}{
		{"wechat", "wechat", nil},
		{"Weixin", "wechat", nil},
		{" wx ", "wechat", nil},
		{"alipay", "alipay", nil},
		{"ZHIFUBAO", "alipay", nil},
		{"yikatong", "card", nil},
		{"campus", "card", nil},
		{"bogus", "", ErrUnsupported},
		{"", "", ErrUnsupported},
	}
	for _, tc := range cases {
		got, err := NormalizeChannel(tc.in)
		matchErr := tc.err == nil && err == nil || tc.err != nil && errors.Is(err, tc.err)
		if got != tc.want || !matchErr {
			t.Errorf("NormalizeChannel(%q) = %q, %v; want %q, %v", tc.in, got, err, tc.want, tc.err)
		}
	}
}

func TestChannelFromRequest(t *testing.T) {
	alipayForm := url.Values{
		"app_id":       {"2021000000000001"},
		"out_trade_no": {"ORDER-1"},
		"trade_no":     {"ALI-1"},
		"total_amount": {"1.00"},
		"sign_type":    {"RSA2"},
	}.Encode()

	cases := []struct {
		name    string
		query   string
		headers map[string]string
		body    string
		def     string
		want    string
		wantErr error
	}{
		{
			name:  "query parameter wins",
			query: "?channel=alipay",
			headers: map[string]string{
				"X-Payment-Channel":   "wechat",
				"Wechatpay-Signature": "sig",
			},
			body: `{"channel":"card"}`,
			def:  "wechat",
			want: "alipay",
		},
		{
			name:    "X-Payment-Channel header",
			headers: map[string]string{"X-Payment-Channel": "weixin"},
			body:    `{"foo":"bar"}`,
			want:    "wechat",
		},
		{
			name:    "WeChat v3 signature header",
			headers: map[string]string{"Wechatpay-Signature": "base64sig"},
			body:    `{}`,
			want:    "wechat",
		},
		{
			name: "body channel field",
			body: `{"orderNo":"O-1","channel":"yikatong"}`,
			want: "card",
		},
		{
			name: "WeChat envelope",
			body: `{"id":"evt","resource":{"algorithm":"AEAD_AES_256_GCM"}}`,
			want: "wechat",
		},
		{
			name: "card body",
			body: `{"method":"recharge","card_no":"CARD-1","sign":"abc"}`,
			want: "card",
		},
		{
			name: "alipay form",
			body: alipayForm,
			want: "alipay",
		},
		{
			name: "alipay JSON app_id",
			body: `{"app_id":"2021000000000001","trade_status":"TRADE_SUCCESS"}`,
			want: "alipay",
		},
		{
			name: "wechat v2 appid",
			body: `{"appid":"wx1234567890","out_trade_no":"O-1"}`,
			want: "wechat",
		},
		{
			name: "default channel",
			body: `{"foo":"bar"}`,
			def:  "card",
			want: "card",
		},
		{
			name:    "no channel and no default",
			body:    `{"foo":"bar"}`,
			wantErr: ErrNoChannel,
		},
		{
			name:    "empty body and no default",
			wantErr: ErrNoChannel,
		},
		{
			name:    "unknown channel is an error",
			query:   "?channel=paypal",
			body:    `{}`,
			def:     "card",
			wantErr: ErrUnsupported,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/notify"+tc.query, strings.NewReader(tc.body))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			got, err := ChannelFromRequest(req, []byte(tc.body), tc.def)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("channel = %q, want %q", got, tc.want)
			}
		})
	}
}

// ChannelFromRequest must not consume the request body: providers re-parse
// form-encoded callbacks from it after the handler has read the raw bytes.
func TestChannelFromRequestDoesNotConsumeBody(t *testing.T) {
	body := `{"app_id":"2021000000000001","trade_no":"ALI-1"}`
	req := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(body))
	ch, err := ChannelFromRequest(req, []byte(body), "")
	if err != nil || ch != "alipay" {
		t.Fatalf("channel = %q, err = %v", ch, err)
	}
	remaining, err := io.ReadAll(req.Body)
	if err != nil || string(remaining) != body {
		t.Fatalf("body consumed: %q, err = %v", remaining, err)
	}
}
