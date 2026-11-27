package proxy

// Dial-proxy upstream request adaptation.
//
// The dial gateway is an external NAS; this package only adapts the RADIUS
// request that is proxied to an operator AAA. No PPPoE implementation lives
// here. Configuration is strictly data-driven from RadiusRealmRoute: vendor
// identifiers select small, well-defined transformations and an optional JSON
// attribute list carries raw VSA bytes. Every configuration value is validated
// before being applied so a bad admin input fails closed instead of emitting a
// malformed RADIUS packet.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

// Supported dial vendors. An empty DialVendor means an ordinary proxy route.
const (
	DialVendorRuijie  = "ruijie"
	DialVendorDrcom   = "drcom"
	DialVendorPanabit = "panabit"
	DialVendorCTCC    = "ctcc"
	DialVendorCMCC    = "cmcc"
	DialVendorCUCC    = "cucc"
	DialVendorXujing  = "xujing"
)

const (
	dialAttrTypeCHAPPassword = 3  // CHAP-Password (RFC 2865)
	dialAttrTypeDrcomPrivate = 60 // DRCOM vendor-specific legacy attribute
	dialMaxAttrValueLen      = 253
)

const (
	dialFilterIDPanabit = "__PAIPPXY__"
	dialFilterIDXujing  = "__DIALPROXY__"
)

// ValidateDialVendor rejects unknown dial vendor identifiers before a route
// is persisted, so a typo fails at configuration time instead of dropping
// authentication packets at runtime.
func ValidateDialVendor(vendor string) error {
	switch vendor {
	case "", DialVendorRuijie, DialVendorDrcom, DialVendorPanabit,
		DialVendorCTCC, DialVendorCMCC, DialVendorCUCC, DialVendorXujing:
		return nil
	default:
		return fmt.Errorf("dial: unknown dial vendor %q", vendor)
	}
}

// DialAttribute is one extra RADIUS attribute injected into an upstream
// dial-proxy request. ValueHex is the raw attribute value in hexadecimal
// notation, which permits byte-exact VSA payloads (including vendor IDs and
// sub-attributes) without defining a Go type for every vendor.
type DialAttribute struct {
	Type     int    `json:"type"`
	ValueHex string `json:"value_hex"`
}

// ParseDialVendorAttrs validates and parses a RadiusRealmRoute.
// DialVendorAttrsJSON value. An empty (or JSON "null") configuration means
// "no extra attributes". Validation is deliberately strict: attribute types
// must be in 1..255 and the decoded value must be legal hex of at most 253
// bytes, otherwise the route is rejected instead of forwarding corrupted data.
func ParseDialVendorAttrs(raw string) ([]DialAttribute, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil, nil
	}

	var attrs []DialAttribute
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		return nil, fmt.Errorf("dial: invalid vendor attrs json: %w", err)
	}
	for i, attr := range attrs {
		if attr.Type < 1 || attr.Type > 255 {
			return nil, fmt.Errorf("dial: vendor attrs[%d]: type %d out of range 1-255", i, attr.Type)
		}
		value, err := hex.DecodeString(attr.ValueHex)
		if err != nil {
			return nil, fmt.Errorf("dial: vendor attrs[%d]: invalid hex value: %w", i, err)
		}
		if len(value) > dialMaxAttrValueLen {
			return nil, fmt.Errorf("dial: vendor attrs[%d]: value length %d exceeds %d", i, len(value), dialMaxAttrValueLen)
		}
	}
	return attrs, nil
}

// ApplyDialProxyProfile adapts an already-copied upstream request for the
// configured dial vendor. It is called after buildUpstreamPacket has copied
// the original attributes and rewritten the credentials, and before the
// upstream Request Authenticator and Message-Authenticator are recomputed;
// this function never signs the packet itself.
//
// orig is the untouched NAS request; it is kept in the signature so callers
// can reason about the source packet, but must not be mutated here.
func ApplyDialProxyProfile(packet *radius.Packet, orig *radius.Packet, route *domain.RadiusRealmRoute) error {
	if packet == nil {
		return fmt.Errorf("dial: nil upstream packet")
	}
	if route == nil {
		return nil
	}

	if err := ValidateDialVendor(route.DialVendor); err != nil {
		return err
	}

	// Filter-Id: explicit route value first, then the vendor defaults. When
	// neither exists an ordinary route keeps whatever the NAS sent, unchanged.
	filterID := route.DialFilterID
	if filterID == "" {
		switch route.DialVendor {
		case DialVendorPanabit:
			filterID = dialFilterIDPanabit
		case DialVendorXujing:
			filterID = dialFilterIDXujing
		}
	}
	if filterID != "" {
		// Set replaces any Filter-Id copied from the original packet. NewBytes
		// enforces the 253-byte attribute value limit.
		if err := rfc2865.FilterID_SetString(packet, filterID); err != nil {
			return fmt.Errorf("dial: set filter-id: %w", err)
		}
	}

	// DRCOM uses PAP upstream: CHAP-Password and the legacy type-60 attribute
	// must not leak into the operator request. Only the outgoing copy changes.
	if route.DialVendor == DialVendorDrcom {
		packet.Attributes.Del(dialAttrTypeCHAPPassword)
		packet.Attributes.Del(dialAttrTypeDrcomPrivate)
	}

	// CTCC/CMCC/CUCC use a fixed carrier NAS-IP in the Java implementation.
	// That address is deployment-private and is NOT forged here: the gateway's
	// own NAS-IP (copied verbatim from orig) is passed through unless an
	// explicit type-4 attribute is supplied in DialVendorAttrsJSON below.
	// Nothing to do here on purpose.

	attrs, err := ParseDialVendorAttrs(route.DialVendorAttrsJSON)
	if err != nil {
		return err
	}
	for _, attr := range attrs {
		value, _ := hex.DecodeString(attr.ValueHex) // already validated
		// Set replaces all occurrences of the same type, including attributes
		// copied from orig and the Filter-Id set above.
		packet.Attributes.Set(radius.Type(attr.Type), radius.Attribute(value))
	}
	return nil
}
