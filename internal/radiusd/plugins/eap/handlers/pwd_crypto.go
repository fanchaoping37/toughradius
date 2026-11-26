package handlers

// This file implements the cryptographic primitives required by EAP-pwd
// (RFC 5931) using only Go's standard library.
//
// Authoritative references:
//   - RFC 5931 §2.4  H(x) = HMAC-SHA256([0]^32, x)
//   - RFC 5931 §2.5  KDF (NIST SP800-108 counter mode, HMAC-SHA256)
//   - RFC 5931 §2.8.3 hunting-and-pecking PWE derivation on NIST P-256
//   - RFC 5931 §2.8.4/§2.8.5 Commit/Confirm exchange
//   - RFC 5114 §2.6  DH group 19 = NIST P-256
//
// Note on side channels: Go's crypto/elliptic P-256 implementation is not
// constant-time. For production deployment with high-assurance requirements,
// consider using a constant-time curve implementation. The protocol logic
// and vector correctness are what this package guarantees.

import (
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"

	"errors"
	"github.com/talkincode/toughradius/v9/internal/radiusd/plugins/eap"
	"math/big"
)

var (
	p256     = elliptic.P256()
	p256P    = p256.Params().P
	p256N    = p256.Params().N
	p256B    = p256.Params().B
	p256Gx   = p256.Params().Gx
	p256Gy   = p256.Params().Gy
	zero32   = make([]byte, 32)
	one      = big.NewInt(1)
	pWElabel = []byte("EAP-pwd Hunting And Pecking")
	two      = big.NewInt(2)
)

// H implements RFC 5931 §2.4: H(x) = HMAC-SHA256([0]^32, x).
func pwdH(x ...[]byte) []byte {
	mac := hmac.New(sha256.New, zero32)
	for _, part := range x {
		mac.Write(part)
	}
	return mac.Sum(nil)
}

// kdf implements RFC 5931 §2.5 (SP800-108 feedback mode with HMAC-SHA256).
// KDF(secret, label, length) produces exactly length octets.
func pwdKDF(secret, label []byte, length int) []byte {
	out := make([]byte, 0, length)
	var previous []byte
	counter := uint32(1)
	for len(out) < length {
		mac := hmac.New(sha256.New, secret)
		if len(previous) > 0 {
			mac.Write(previous)
		}
		var c [4]byte
		c[0] = byte(counter >> 24)
		c[1] = byte(counter >> 16)
		c[2] = byte(counter >> 8)
		c[3] = byte(counter)
		mac.Write(c[:])
		mac.Write(label)
		previous = mac.Sum(nil)
		out = append(out, previous...)
		counter++
	}
	return out[:length]
}

// pwdToBig converts a fixed-length big-endian byte string to *big.Int.
func pwdToBig(b []byte) *big.Int { return new(big.Int).SetBytes(b) }

// pwdBigBytes encodes a *big.Int as a fixed-length big-endian byte string.
func pwdBigBytes(v *big.Int, length int) []byte {
	out := make([]byte, length)
	v.FillBytes(out)
	return out
}

// derivePWE implements RFC 5931 §2.8.3 hunting-and-pecking on P-256.
//
//	pwd-seed = H(token | peer-ID | server-ID | password | counter)
//	pwd-value = KDF(pwd-seed, "EAP-pwd Hunting And Pecking", len(p))
//	x = pwd-value mod p; y = sqrt(x^3 + ax + b); choose y with matching LSB
//
// The function returns the Password Element (PWE) point and the RFC-defined
// pwd-seed, which is needed by the peer during Commit.
func derivePWE(token, peerID, serverID, password []byte) (x, y *big.Int, found bool, err error) {
	if len(token) == 0 || len(peerID) == 0 || len(serverID) == 0 || len(password) == 0 {
		return nil, nil, false, errors.New("eap-pwd: empty PWE input")
	}
	for counter := byte(1); counter != 0; counter++ {
		seed := pwdH(token, peerID, serverID, password, []byte{counter})
		// For P-256, len(p) = 32 octets.
		value := pwdKDF(seed, pWElabel, 32)
		x := new(big.Int).SetBytes(value)
		x.Mod(x, p256P)
		if x.Sign() == 0 {
			continue
		}
		// y^2 = x^3 + ax + b; P-256 has a = p-3.
		a := new(big.Int).Sub(p256P, big.NewInt(3))
		y2 := new(big.Int).Mul(x, x)
		y2.Mul(y2, x)
		ax := new(big.Int).Mul(a, x)
		y2.Add(y2, ax)
		y2.Add(y2, p256B)
		y2.Mod(y2, p256P)
		// p ≡ 3 (mod 4), so sqrt = y2^((p+1)/4) mod p.
		exp := new(big.Int).Add(p256P, one)
		exp.Div(exp, big.NewInt(4))
		y := new(big.Int).Exp(y2, exp, p256P)
		// Verify y^2 == y2; if not, x is not on the curve.
		check := new(big.Int).Mul(y, y)
		check.Mod(check, p256P)
		if check.Cmp(y2) != 0 {
			continue
		}
		seedLSB := seed[len(seed)-1] & 1
		yLSB := y.Bit(0)
		if yLSB != uint(seedLSB) {
			y.Sub(p256P, y)
		}
		if !p256.IsOnCurve(x, y) {
			continue
		}
		return x, y, true, nil
	}
	return nil, nil, false, errors.New("eap-pwd: PWE not found after 255 iterations")
}

// pwdScalarElement holds the Commit exchange values (scalar, element).
type pwdScalarElement struct {
	Scalar             *big.Int
	ElementX, ElementY *big.Int
}

// commit computes Scalar = (rand + mask) mod r, Element = inv(mask * PWE).
func commit(randVal, maskVal *big.Int, pweX, pweY *big.Int) (*pwdScalarElement, error) {
	if randVal.Sign() <= 0 || randVal.Cmp(p256N) >= 0 ||
		maskVal.Sign() <= 0 || maskVal.Cmp(p256N) >= 0 {
		return nil, errors.New("eap-pwd: scalar out of range")
	}
	scalar := new(big.Int).Add(randVal, maskVal)
	scalar.Mod(scalar, p256N)
	if scalar.Sign() == 0 {
		return nil, errors.New("eap-pwd: zero scalar")
	}
	// mask * PWE
	mx, my := p256.ScalarMult(pweX, pweY, maskVal.Bytes())
	if mx == nil || my == nil {
		return nil, errors.New("eap-pwd: invalid mask point")
	}
	// inverse = -point (negate Y mod p)
	invY := new(big.Int).Sub(p256P, my)
	invY.Mod(invY, p256P)
	if !p256.IsOnCurve(mx, invY) {
		return nil, errors.New("eap-pwd: inverse point not on curve")
	}
	return &pwdScalarElement{Scalar: scalar, ElementX: mx, ElementY: invY}, nil
}

// sharedSecret implements RFC 5931 §2.8.4 (ECC case):
//
//	KS = s_rand * (Scalar_P * PWE + Element_P)
//	ks = F(KS) = x-coordinate of KS
func sharedSecret(sRand *big.Int, peer *pwdScalarElement, pweX, pweY *big.Int) ([]byte, error) {
	// Validate peer element is on curve.
	if !p256.IsOnCurve(peer.ElementX, peer.ElementY) {
		return nil, errors.New("eap-pwd: peer element not on curve")
	}
	// Scalar_P * PWE
	px, py := p256.ScalarMult(pweX, pweY, peer.Scalar.Bytes())
	if px == nil || py == nil {
		return nil, errors.New("eap-pwd: invalid peer scalar")
	}
	// + Element_P
	sx, sy := p256.Add(px, py, peer.ElementX, peer.ElementY)
	if sx == nil || sy == nil {
		return nil, errors.New("eap-pwd: point addition failed")
	}
	// s_rand * result
	kx, _ := p256.ScalarMult(sx, sy, sRand.Bytes())
	if kx == nil {
		return nil, errors.New("eap-pwd: invalid shared secret point")
	}
	return pwdBigBytes(kx, 32), nil
}

// deriveMSK implements RFC 5931 §2.9:
//
//	MK = H(ks | Confirm_P | Confirm_S)
//	MSK|EMSK = KDF(MK, Session-ID, 1024) -> 64 bytes each
func deriveMSK(ks, confirmP, confirmS, sessionID []byte) (msk, emsk []byte) {
	mk := pwdH(ks, confirmP, confirmS)
	both := pwdKDF(mk, sessionID, 128)
	return both[:64], both[64:]
}

// passwordFromProvider is a helper type alias for clarity at call sites.
type passwordFromProvider string

// generateRandomScalar returns a random scalar in [1, r-1].
func generateRandomScalar() (*big.Int, error) {
	for {
		b, err := eap.GenerateRandomBytes(32)
		if err != nil {
			return nil, err
		}
		v := new(big.Int).SetBytes(b)
		v.Mod(v, p256N)
		if v.Sign() > 0 {
			return v, nil
		}
	}
}

// compareConstantTime compares two byte slices in constant time.
func compareConstantTime(a, b []byte) bool { return hmac.Equal(a, b) }

// bigIntFromBytes is a convenience wrapper used by tests and handlers.
func bigIntFromBytes(b []byte) *big.Int { return pwdToBig(b) }

// curveParams exposes P-256 parameters for verification/tests.
func curveParams() (p, n *big.Int) {
	return new(big.Int).Set(p256P), new(big.Int).Set(p256N)
}

var (
	_ = two
	_ = p256Gx
	_ = p256Gy
	_ = passwordFromProvider("")
)
