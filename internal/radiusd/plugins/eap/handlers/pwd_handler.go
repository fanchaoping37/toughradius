package handlers

// EAP-pwd handler (RFC 5931, IANA EAP type 52).
//
// Wire format (RFC 5931 §3.1):
//
//	Code(1) | Identifier(1) | Length(2) | Type(1)=52 |
//	Flags(1): L(0x80) | M(0x40) | PWD-Exch(0x0f lower nibble) |
//	[Total-Length(2) if L set] | Data
//
// PWD-Exch values: 1=ID, 2=Commit, 3=Confirm.
// Fragmentation flags L/M are parsed but this implementation requires whole
// messages (RADIUS EAP-Message attributes are concatenated by the coordinator).
//
// Server flow:
//   ID exchange    -> send Ciphersuite(0x0b) + token + Server-ID + Prep(0x00)
//   Commit exchange-> validate peer scalar/element, compute ks
//   Confirm exchange -> verify peer confirm, send server confirm + EAP-Success
//
// Limits: only group 19 (NIST P-256), PRF HMAC-SHA256, RF HMAC-SHA256,
// Prep=none (RFC 5931 §2.10 mandatory-to-implement set).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/talkincode/toughradius/v9/internal/radiusd/plugins/eap"
	"github.com/talkincode/toughradius/v9/pkg/common"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

const (
	EAPMethodPWD = "eap-pwd"

	pwdExchID      = 0x01
	pwdExchCommit  = 0x02
	pwdExchConfirm = 0x03

	pwdFlagL = 0x80
	pwdFlagM = 0x40

	pwdGroup19  = 19 // RFC 5114 §2.6, NIST P-256
	pwdPrepNone = 0x00
	pwdRFHMAC   = 0x01
)

var (
	errPWDState    = errors.New("eap-pwd: state error")
	errPWDFormat   = errors.New("eap-pwd: malformed message")
	errPWDRejected = errors.New("eap-pwd: authentication failed")
)

// pwdState is stored in EAPState.Data under this key across rounds.
const pwdStateKey = "pwd"

type pwdRoundState struct {
	Phase        string // "id", "commit", "confirm"
	ServerID     []byte
	PeerID       []byte
	Token        []byte
	Password     string
	SRand        *big.Int
	SMask        *big.Int
	ServerElem   *pwdScalarElement
	PWE          *pwdScalarElement
	KS           []byte
	Username     string
	peerElem     *pwdScalarElement
	localConfirm []byte
	msk, emsk    []byte
}

// PWDHandler implements RFC 5931 EAP-pwd.
type PWDHandler struct{}

func NewPWDHandler() *PWDHandler { return &PWDHandler{} }

func (h *PWDHandler) Name() string   { return EAPMethodPWD }
func (h *PWDHandler) EAPType() uint8 { return eap.TypePWD }

func (h *PWDHandler) CanHandle(ctx *eap.EAPContext) bool {
	return ctx.EAPMessage != nil && ctx.EAPMessage.Type == eap.TypePWD
}

// HandleIdentity sends the EAP-pwd ID exchange: ciphersuite + token +
// server ID + prep. The token also serves as an anti-clogging value (RFC 5931 §2.8.2).
func (h *PWDHandler) HandleIdentity(ctx *eap.EAPContext) (bool, error) {
	username := rfc2865.UserName_GetString(ctx.Request.Packet)
	token, err := eap.GenerateRandomBytes(32)
	if err != nil {
		return false, err
	}
	serverID := []byte("toughradius-eap-pwd")

	state := &pwdRoundState{
		Phase:    "id",
		ServerID: serverID,
		Token:    token,
		Username: username,
	}
	stateID := common.UUID()
	eapState := &eap.EAPState{
		Username: username,
		StateID:  stateID,
		Method:   EAPMethodPWD,
		Success:  false,
		Data:     map[string]interface{}{pwdStateKey: state},
	}
	if err := ctx.StateManager.SetState(stateID, eapState); err != nil {
		return false, err
	}

	// Data: ciphersuite(6) | token(32) | serverID(len prefix) | prep(1)
	payload := make([]byte, 0, 64)
	cs := make([]byte, 6)
	binary.BigEndian.PutUint16(cs[0:2], pwdGroup19)
	cs[2] = pwdRFHMAC
	cs[3] = 0 // PRF HMAC-SHA256
	cs[4] = 0 // reserved
	cs[5] = pwdPrepNone
	payload = append(payload, cs...)
	payload = append(payload, token...)
	payload = append(payload, byte(len(serverID)))
	payload = append(payload, serverID...)
	payload = append(payload, pwdPrepNone)

	msg := buildPWDMessage(eap.CodeRequest, ctx.EAPMessage.Identifier, pwdExchID, payload)
	response := ctx.Request.Response(radius.CodeAccessChallenge)
	if err := rfc2865.State_SetString(response, stateID); err != nil {
		return false, err
	}
	eap.SetEAPMessageAndAuth(response, msg, ctx.Secret)
	return true, ctx.ResponseWriter.Write(response)
}

// HandleResponse drives the Commit and Confirm exchanges.
func (h *PWDHandler) HandleResponse(ctx *eap.EAPContext) (bool, error) {
	stateID := rfc2865.State_GetString(ctx.Request.Packet)
	if stateID == "" {
		return false, errPWDState
	}
	eapState, err := ctx.StateManager.GetState(stateID)
	if err != nil {
		return false, err
	}
	state, ok := eapState.Data[pwdStateKey].(*pwdRoundState)
	if !ok || state == nil {
		return false, errPWDState
	}

	exch, payload, err := parsePWDMessage(ctx.EAPMessage.Data)
	if err != nil {
		return false, err
	}

	switch state.Phase {
	case "id":
		return h.handlePeerID(ctx, stateID, eapState, state, payload, exch)
	case "commit":
		return h.handlePeerCommit(ctx, stateID, eapState, state, payload, exch)
	case "confirm":
		return h.handlePeerConfirm(ctx, stateID, eapState, state, payload, exch)
	default:
		return false, errPWDState
	}
}

// handlePeerID receives the peer's echoed ciphersuite/token + Peer-ID, derives
// the password element and sends the server Commit.
func (h *PWDHandler) handlePeerID(ctx *eap.EAPContext, stateID string, eapState *eap.EAPState,
	state *pwdRoundState, payload []byte, exch byte) (bool, error) {
	if exch != pwdExchID || len(payload) < 6+32+1 {
		return false, errPWDFormat
	}
	// Verify the peer echoed our ciphersuite/token (anti-clogging, §2.8.2).
	if !hmac.Equal(payload[:6], []byte{0, pwdGroup19, pwdRFHMAC, 0, 0, pwdPrepNone}) {
		return false, errPWDRejected
	}
	if !hmac.Equal(payload[6:38], state.Token) {
		return false, errPWDRejected
	}
	peerIDLen := int(payload[38])
	if peerIDLen <= 0 || 39+peerIDLen > len(payload) {
		return false, errPWDFormat
	}
	state.PeerID = append([]byte(nil), payload[39:39+peerIDLen]...)
	username := state.Username
	if username == "" {
		username = string(state.PeerID)
	}
	password, err := ctx.PwdProvider.GetPassword(ctx.User, ctx.IsMacAuth)
	if err != nil {
		return false, err
	}
	state.Password = password

	pweX, pweY, ok, err := derivePWE(state.Token, state.PeerID, state.ServerID, []byte(password))
	if err != nil || !ok {
		return false, fmt.Errorf("eap-pwd: derive PWE: %w", err)
	}
	pwe := &pwdScalarElement{Scalar: nil, ElementX: pweX, ElementY: pweY}

	sRand, err := generateRandomScalar()
	if err != nil {
		return false, err
	}
	sMask, err := generateRandomScalar()
	if err != nil {
		return false, err
	}
	serverCommit, err := commit(sRand, sMask, pweX, pweY)
	if err != nil {
		return false, err
	}
	state.SRand = sRand
	state.SMask = sMask
	state.ServerElem = serverCommit
	state.PWE = pwe
	state.Phase = "commit"
	eapState.Data[pwdStateKey] = state
	if err := ctx.StateManager.SetState(stateID, eapState); err != nil {
		return false, err
	}

	// Commit Data: Scalar_S(32) | Element_S x(32) | Element_S y(32)
	data := make([]byte, 0, 96)
	data = append(data, pwdBigBytes(serverCommit.Scalar, 32)...)
	data = append(data, pwdBigBytes(serverCommit.ElementX, 32)...)
	data = append(data, pwdBigBytes(serverCommit.ElementY, 32)...)
	msg := buildPWDMessage(eap.CodeRequest, ctx.EAPMessage.Identifier, pwdExchCommit, data)
	response := ctx.Request.Response(radius.CodeAccessChallenge)
	if err := rfc2865.State_SetString(response, stateID); err != nil {
		return false, err
	}
	eap.SetEAPMessageAndAuth(response, msg, ctx.Secret)
	return true, ctx.ResponseWriter.Write(response)
}

// handlePeerCommit validates the peer commit, computes ks and sends our Confirm.
func (h *PWDHandler) handlePeerCommit(ctx *eap.EAPContext, stateID string, eapState *eap.EAPState,
	state *pwdRoundState, payload []byte, exch byte) (bool, error) {
	if exch != pwdExchCommit || len(payload) < 96 {
		return false, errPWDFormat
	}
	peer := &pwdScalarElement{
		Scalar:   pwdToBig(payload[0:32]),
		ElementX: pwdToBig(payload[32:64]),
		ElementY: pwdToBig(payload[64:96]),
	}
	if peer.Scalar.Sign() <= 0 || peer.Scalar.Cmp(p256N) >= 0 {
		return false, errPWDRejected
	}
	ks, err := sharedSecret(state.SRand, peer, state.PWE.ElementX, state.PWE.ElementY)
	if err != nil {
		return false, err
	}
	state.KS = ks
	state.PWE.Scalar = peer.Scalar // keep peer scalar for confirm computation
	// We need the peer element for Confirm; store a copy in a dedicated field.
	state.peerElem = &pwdScalarElement{Scalar: peer.Scalar, ElementX: peer.ElementX, ElementY: peer.ElementY}
	state.Phase = "confirm"
	eapState.Data[pwdStateKey] = state
	if err := ctx.StateManager.SetState(stateID, eapState); err != nil {
		return false, err
	}

	// Confirm_S = H(ks | Element_S | Scalar_S | Element_P | Scalar_P | Ciphersuite)
	ciphersuite := []byte{0, pwdGroup19, pwdRFHMAC, 0, 0, pwdPrepNone}
	confirmS := pwdH(ks,
		pwdBigBytes(state.ServerElem.ElementX, 32), pwdBigBytes(state.ServerElem.ElementY, 32),
		pwdBigBytes(state.ServerElem.Scalar, 32),
		pwdBigBytes(peer.ElementX, 32), pwdBigBytes(peer.ElementY, 32),
		pwdBigBytes(peer.Scalar, 32), ciphersuite)

	state.localConfirm = confirmS
	msg := buildPWDMessage(eap.CodeRequest, ctx.EAPMessage.Identifier, pwdExchConfirm, confirmS)
	response := ctx.Request.Response(radius.CodeAccessChallenge)
	if err := rfc2865.State_SetString(response, stateID); err != nil {
		return false, err
	}
	eap.SetEAPMessageAndAuth(response, msg, ctx.Secret)
	return true, ctx.ResponseWriter.Write(response)
}

// handlePeerConfirm verifies Confirm_P and finishes authentication.
func (h *PWDHandler) handlePeerConfirm(ctx *eap.EAPContext, stateID string, eapState *eap.EAPState,
	state *pwdRoundState, payload []byte, exch byte) (bool, error) {
	if exch != pwdExchConfirm || len(payload) != sha256.Size {
		return false, errPWDFormat
	}
	peer := state.peerElem
	if peer == nil {
		return false, errPWDState
	}
	ciphersuite := []byte{0, pwdGroup19, pwdRFHMAC, 0, 0, pwdPrepNone}
	confirmP := pwdH(state.KS,
		pwdBigBytes(peer.ElementX, 32), pwdBigBytes(peer.ElementY, 32),
		pwdBigBytes(peer.Scalar, 32),
		pwdBigBytes(state.ServerElem.ElementX, 32), pwdBigBytes(state.ServerElem.ElementY, 32),
		pwdBigBytes(state.ServerElem.Scalar, 32), ciphersuite)
	if !hmac.Equal(payload, confirmP) {
		return false, errPWDRejected
	}
	// Derive MSK/EMSK per §2.9 for downstream keying. Stored for completeness.
	sessionID := []byte(stateID)
	msk, emsk := deriveMSK(state.KS, confirmP, state.localConfirm, sessionID)
	state.msk, state.emsk = msk, emsk
	eapState.Success = true
	state.Phase = "done"
	eapState.Data[pwdStateKey] = state
	if err := ctx.StateManager.SetState(stateID, eapState); err != nil {
		return false, err
	}
	return true, nil
}

// buildPWDMessage assembles an EAP-pwd message without fragmentation.
func buildPWDMessage(code, identifier, exch byte, data []byte) []byte {
	// Code(1) | Id(1) | Length(2) | Type(1) | Flags(1) | Data
	length := 6 + len(data)
	msg := make([]byte, 6, length+2)
	msg[0] = code
	msg[1] = identifier
	binary.BigEndian.PutUint16(msg[2:4], uint16(length)) //nolint:gosec
	msg[4] = eap.TypePWD
	msg[5] = exch // no L/M flags: complete message
	return append(msg, data...)
}

// parsePWDMessage extracts PWD-Exch and data, validating fragmentation flags.
func parsePWDMessage(data []byte) (exch byte, payload []byte, err error) {
	if len(data) < 2 {
		return 0, nil, errPWDFormat
	}
	flags := data[0]
	exch = flags & 0x0f
	off := 1
	if flags&(pwdFlagL|pwdFlagM) != 0 {
		// L/M fragmentation requires reassembly across EAP messages, which is
		// intentionally unsupported: RADIUS EAP-Message concatenation already
		// handles transport-level fragmentation for standard MTUs.
		return 0, nil, fmt.Errorf("%w: fragmented EAP-pwd not supported", errPWDFormat)
	}
	if exch < pwdExchID || exch > pwdExchConfirm {
		return 0, nil, errPWDFormat
	}
	return exch, append([]byte(nil), data[off:]...), nil
}
