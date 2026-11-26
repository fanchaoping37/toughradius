package handlers

import (
	"bytes"
	"crypto/elliptic"
	"math/big"
	"testing"
)

func TestPWDHandshakeVector(t *testing.T) {
	// RFC 5931 cryptographic self-consistency: peer and server derive the
	// same PWE from the same credentials, and the Commit/Confirm exchanges
	// produce an identical shared secret and MSK/EMSK.
	token := []byte("0123456789abcdef0123456789abcdef")
	peerID := []byte("student@campus.example")
	serverID := []byte("toughradius-eap-pwd")
	password := []byte("correct horse battery staple")

	pweX, pweY, ok, err := derivePWE(token, peerID, serverID, password)
	if err != nil || !ok {
		t.Fatalf("derive PWE: ok=%v err=%v", ok, err)
	}
	if !p256.IsOnCurve(pweX, pweY) {
		t.Fatal("PWE not on P-256")
	}
	// Same inputs must give the same PWE (hunting-and-pecking is deterministic).
	x2, y2, ok2, _ := derivePWE(token, peerID, serverID, password)
	if !ok2 || x2.Cmp(pweX) != 0 || y2.Cmp(pweY) != 0 {
		t.Fatal("PWE derivation not deterministic")
	}
	// Different password must give a different PWE.
	x3, y3, _, _ := derivePWE(token, peerID, serverID, []byte("wrong password"))
	if x3.Cmp(pweX) == 0 && y3.Cmp(pweY) == 0 {
		t.Fatal("different password produced same PWE")
	}

	// Pick deterministic-looking randoms that are in range (test-only).
	sRand := big.NewInt(123456789)
	sMask := big.NewInt(987654321)
	pRand := big.NewInt(555555555)
	pMask := big.NewInt(444444444)

	serverCommit, err := commit(sRand, sMask, pweX, pweY)
	if err != nil {
		t.Fatal(err)
	}
	peerCommit, err := commit(pRand, pMask, pweX, pweY)
	if err != nil {
		t.Fatal(err)
	}
	ksServer, err := sharedSecret(sRand, peerCommit, pweX, pweY)
	if err != nil {
		t.Fatal(err)
	}
	ksPeer, err := sharedSecret(pRand, serverCommit, pweX, pweY)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ksServer, ksPeer) {
		t.Fatalf("shared secrets differ:\nserver=%x\npeer=%x", ksServer, ksPeer)
	}
	if len(ksServer) != 32 {
		t.Fatalf("ks length = %d", len(ksServer))
	}

	ciphersuite := []byte{0, 19, 1, 0, 0, 0}
	confirmS := pwdH(ksServer,
		pwdBigBytes(serverCommit.ElementX, 32), pwdBigBytes(serverCommit.ElementY, 32),
		pwdBigBytes(serverCommit.Scalar, 32),
		pwdBigBytes(peerCommit.ElementX, 32), pwdBigBytes(peerCommit.ElementY, 32),
		pwdBigBytes(peerCommit.Scalar, 32), ciphersuite)
	confirmP := pwdH(ksPeer,
		pwdBigBytes(peerCommit.ElementX, 32), pwdBigBytes(peerCommit.ElementY, 32),
		pwdBigBytes(peerCommit.Scalar, 32),
		pwdBigBytes(serverCommit.ElementX, 32), pwdBigBytes(serverCommit.ElementY, 32),
		pwdBigBytes(serverCommit.Scalar, 32), ciphersuite)

	mskS, emskS := deriveMSK(ksServer, confirmP, confirmS, []byte("session-1"))
	mskP, emskP := deriveMSK(ksPeer, confirmP, confirmS, []byte("session-1"))
	if !bytes.Equal(mskS, mskP) || !bytes.Equal(emskS, emskP) {
		t.Fatal("MSK/EMSK mismatch between peer and server")
	}
	if len(mskS) != 64 || len(emskS) != 64 {
		t.Fatalf("MSK/EMSK lengths: %d/%d", len(mskS), len(emskS))
	}
}

func TestPWDHAndKDF(t *testing.T) {
	// RFC 5931 §2.4: H(x) = HMAC-SHA256([0]^32, x); deterministic, 32 bytes.
	a := pwdH([]byte("abc"))
	b := pwdH([]byte("abc"))
	c := pwdH([]byte("abd"))
	if len(a) != 32 || !bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Fatal("H is not a deterministic 32-byte PRF")
	}
	// KDF outputs requested length and is deterministic.
	k1 := pwdKDF([]byte("secret"), []byte("label"), 100)
	k2 := pwdKDF([]byte("secret"), []byte("label"), 100)
	k3 := pwdKDF([]byte("secret"), []byte("other"), 100)
	if len(k1) != 100 || !bytes.Equal(k1, k2) || bytes.Equal(k1, k3) {
		t.Fatal("KDF failed length/determinism/label separation checks")
	}
}

func TestPWDRejectsInvalidInputs(t *testing.T) {
	if _, _, ok, err := derivePWE(nil, []byte("p"), []byte("s"), []byte("pw")); err == nil || ok {
		t.Fatal("empty token accepted")
	}
	bigN := new(big.Int).Set(elliptic.P256().Params().N)
	if _, err := commit(bigN, big.NewInt(2), big.NewInt(1), big.NewInt(2)); err == nil {
		t.Fatal("out-of-range scalar accepted")
	}
	badPeer := &pwdScalarElement{Scalar: big.NewInt(2), ElementX: big.NewInt(0), ElementY: big.NewInt(0)}
	if _, err := sharedSecret(big.NewInt(2), badPeer, big.NewInt(1), big.NewInt(2)); err == nil {
		t.Fatal("off-curve peer element accepted")
	}
}

func TestBuildAndParsePWDMessage(t *testing.T) {
	msg := buildPWDMessage(1, 7, pwdExchCommit, make([]byte, 96))
	if len(msg) != 102 || msg[0] != 1 || msg[1] != 7 || msg[4] != 52 || msg[5] != pwdExchCommit {
		t.Fatalf("built message header wrong: %x", msg[:8])
	}
	exch, payload, err := parsePWDMessage(msg[5:])
	if err != nil || exch != pwdExchCommit || len(payload) != 96 {
		t.Fatalf("parse: exch=%d len=%d err=%v", exch, len(payload), err)
	}
	frag := append([]byte{0xc0 | pwdExchID, 0}, make([]byte, 4)...)
	if _, _, err := parsePWDMessage(frag); err == nil {
		t.Fatal("fragmented message accepted")
	}
}

func TestRandomScalarInRange(t *testing.T) {
	for i := 0; i < 10; i++ {
		v, err := generateRandomScalar()
		if err != nil {
			t.Fatal(err)
		}
		if v.Sign() <= 0 || v.Cmp(p256N) >= 0 {
			t.Fatalf("scalar out of range: %s", v)
		}
	}
}
