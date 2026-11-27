package proxy

import (
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

func TestParseDialVendorAttrs(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantLen int
		wantErr bool
	}{
		{name: "empty", raw: "", wantLen: 0},
		{name: "json null", raw: "null", wantLen: 0},
		{name: "vsa raw bytes", raw: `[{"type":26,"value_hex":"000013098c0a0104"}]`, wantLen: 1},
		{name: "max type", raw: `[{"type":255,"value_hex":"00"}]`, wantLen: 1},
		{name: "malformed json", raw: `{"type":26}`, wantErr: true},
		{name: "type zero", raw: `[{"type":0,"value_hex":"00"}]`, wantErr: true},
		{name: "type too large", raw: `[{"type":256,"value_hex":"00"}]`, wantErr: true},
		{name: "negative type", raw: `[{"type":-1,"value_hex":"00"}]`, wantErr: true},
		{name: "odd hex", raw: `[{"type":26,"value_hex":"0"}]`, wantErr: true},
		{name: "non hex", raw: `[{"type":26,"value_hex":"zz"}]`, wantErr: true},
		{name: "value too long", raw: `[{"type":26,"value_hex":"` + strings.Repeat("00", 254) + `"}]`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs, err := ParseDialVendorAttrs(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, attrs, tt.wantLen)
		})
	}
}

func clonePacketAttributes(src *radius.Packet) *radius.Packet {
	p := radius.New(src.Code, []byte("upsecret"))
	for _, attr := range src.Attributes {
		p.Attributes.Add(attr.Type, append(radius.Attribute(nil), attr.Attribute...))
	}
	return p
}

func TestApplyDialProxyProfileFilterID(t *testing.T) {
	cases := []struct {
		name       string
		vendor     string
		configured string
		want       string
	}{
		{name: "ordinary route keeps nas value", want: "orig-filter"},
		{name: "panabit default", vendor: DialVendorPanabit, want: dialFilterIDPanabit},
		{name: "xujing default", vendor: DialVendorXujing, want: dialFilterIDXujing},
		{name: "ruijie has no default", vendor: DialVendorRuijie, want: "orig-filter"},
		{name: "explicit overrides default", vendor: DialVendorPanabit, configured: "custom-filter", want: "custom-filter"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			orig := radius.New(radius.CodeAccessRequest, []byte("nassecret"))
			require.NoError(t, rfc2865.FilterID_SetString(orig, "orig-filter"))
			packet := clonePacketAttributes(orig)

			route := &domain.RadiusRealmRoute{DialVendor: tt.vendor, DialFilterID: tt.configured}
			require.NoError(t, ApplyDialProxyProfile(packet, orig, route))
			assert.Equal(t, tt.want, rfc2865.FilterID_GetString(packet))
			// The NAS packet is never mutated.
			assert.Equal(t, "orig-filter", rfc2865.FilterID_GetString(orig))
		})
	}
}

func TestApplyDialProxyProfileDrcomRemovesAttrs(t *testing.T) {
	orig := radius.New(radius.CodeAccessRequest, []byte("nassecret"))
	require.NoError(t, rfc2865.CHAPPassword_Set(orig, []byte{0x01, 0x02, 0x03}))
	orig.Attributes.Add(dialAttrTypeDrcomPrivate, radius.Attribute{0xaa, 0xbb})
	packet := clonePacketAttributes(orig)

	require.NoError(t, ApplyDialProxyProfile(packet, orig, &domain.RadiusRealmRoute{DialVendor: DialVendorDrcom}))

	_, hasChap := packet.Attributes.Lookup(dialAttrTypeCHAPPassword)
	_, hasPrivate := packet.Attributes.Lookup(dialAttrTypeDrcomPrivate)
	assert.False(t, hasChap)
	assert.False(t, hasPrivate)
	// Only the outgoing copy is stripped.
	_, origChap := orig.Attributes.Lookup(dialAttrTypeCHAPPassword)
	_, origPrivate := orig.Attributes.Lookup(dialAttrTypeDrcomPrivate)
	assert.True(t, origChap)
	assert.True(t, origPrivate)
}

func TestApplyDialProxyProfileVendorAttrsReplaceExisting(t *testing.T) {
	orig := radius.New(radius.CodeAccessRequest, []byte("nassecret"))
	require.NoError(t, rfc2865.NASIPAddress_Set(orig, net.ParseIP("192.168.1.1")))
	require.NoError(t, rfc2865.FilterID_SetString(orig, "orig-filter"))
	packet := clonePacketAttributes(orig)

	route := &domain.RadiusRealmRoute{
		DialVendor: DialVendorRuijie,
		DialVendorAttrsJSON: `[
			{"type":4,"value_hex":"0a000001"},
			{"type":26,"value_hex":"000013098c0a0104"},
			{"type":11,"value_hex":"636d6363"}
		]`,
	}
	require.NoError(t, ApplyDialProxyProfile(packet, orig, route))

	nasIP, err := rfc2865.NASIPAddress_Lookup(packet)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", nasIP.String())
	assert.Equal(t, "cmcc", rfc2865.FilterID_GetString(packet))
	vsa, ok := packet.Attributes.Lookup(26)
	require.True(t, ok)
	wantVSA, err := hex.DecodeString("000013098c0a0104")
	require.NoError(t, err)
	assert.Equal(t, radius.Attribute(wantVSA), radius.Attribute(vsa))
}

func TestApplyDialProxyProfileRejectsInvalidConfig(t *testing.T) {
	orig := radius.New(radius.CodeAccessRequest, []byte("nassecret"))
	cases := []struct {
		name  string
		route *domain.RadiusRealmRoute
	}{
		{name: "bad hex", route: &domain.RadiusRealmRoute{DialVendor: DialVendorRuijie, DialVendorAttrsJSON: `[{"type":26,"value_hex":"xyz"}]`}},
		{name: "bad type", route: &domain.RadiusRealmRoute{DialVendor: DialVendorPanabit, DialVendorAttrsJSON: `[{"type":0,"value_hex":"00"}]`}},
		{name: "unknown vendor", route: &domain.RadiusRealmRoute{DialVendor: "unknown"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			packet := clonePacketAttributes(orig)
			require.Error(t, ApplyDialProxyProfile(packet, orig, tt.route))
		})
	}
}

func TestApplyDialProxyProfileOrdinaryRouteHasNoSideEffects(t *testing.T) {
	orig := radius.New(radius.CodeAccessRequest, []byte("nassecret"))
	require.NoError(t, rfc2865.UserName_SetString(orig, "alice"))
	require.NoError(t, rfc2865.UserPassword_SetString(orig, "pwd"))
	require.NoError(t, rfc2865.CHAPPassword_Set(orig, []byte{0x01}))
	orig.Attributes.Add(dialAttrTypeDrcomPrivate, radius.Attribute{0xaa})

	packet := clonePacketAttributes(orig)
	require.NoError(t, ApplyDialProxyProfile(packet, orig, &domain.RadiusRealmRoute{}))
	assert.Equal(t, orig.Attributes, packet.Attributes)
}

// TestBuildUpstreamPacketDialProfile verifies the profile is applied inside
// buildUpstreamPacket and that the Message-Authenticator is signed afterwards
// over the changed attributes.
func TestBuildUpstreamPacketDialProfile(t *testing.T) {
	orig := radius.New(radius.CodeAccessRequest, []byte("nassecret"))
	require.NoError(t, rfc2865.UserName_SetString(orig, "alice@isp"))
	require.NoError(t, rfc2865.UserPassword_SetString(orig, "campuspwd"))

	up := &domain.RadiusUpstream{Secret: "upsecret", AuthPort: 1812}
	route := &domain.RadiusRealmRoute{DialVendor: DialVendorXujing}
	packet, err := buildUpstreamPacket(orig, up, route, "alice@isp")
	require.NoError(t, err)
	assert.Equal(t, dialFilterIDXujing, rfc2865.FilterID_GetString(packet))

	got, err := rfc2869.MessageAuthenticator_Lookup(packet)
	require.NoError(t, err)
	require.NoError(t, rfc2869.MessageAuthenticator_Set(packet, make([]byte, 16)))
	wire, err := packet.MarshalBinary()
	require.NoError(t, err)
	mac := hmac.New(md5.New, []byte(up.Secret))
	_, _ = mac.Write(wire)
	assert.True(t, hmac.Equal(got, mac.Sum(nil)))
}
