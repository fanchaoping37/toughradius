package portal

import (
	"errors"
	"fmt"
	"net"
)

// Service is the Portal use-case layer: it validates inputs, delegates the
// UDP exchange to Server and maps protocol ErrCodes to stable result codes
// for the HTTP API. It has no dependency on the admin API or database.
type Service struct {
	Server        *Server
	Tickets       *TicketManager
	RequireTicket bool
}

func NewService(server *Server) *Service { return &Service{Server: server} }

// The active service is set at startup by main.go and consumed by the
// admin API handlers. Nil means the Portal subsystem is disabled.
var activeService *Service

func SetActiveService(service *Service) { activeService = service }
func GetActiveService() *Service        { return activeService }

// ChallengeRequest starts a CHAP authentication.
type ChallengeRequest struct {
	NASAddr  string `json:"nas_addr"`
	Username string `json:"username"`
	UserIP   string `json:"user_ip"`
	MAC      string `json:"mac"`
}

// ChallengeResult is returned to the external login page after the
// Challenge round trip. The serial is exposed for tracing only.
type ChallengeResult struct {
	Serial   uint16 `json:"serial"`
	UserIP   string `json:"user_ip"`
	Username string `json:"username"`
	ReqID    uint16 `json:"req_id"`
	Message  string `json:"message"`
	Ticket   string `json:"auth_ticket,omitempty"`
}

// AuthRequest completes authentication. AuthMode is "chap" (default) or
// "pap"; NASAddr may be omitted when the session already exists.
type AuthRequest struct {
	NASAddr  string `json:"nas_addr"`
	Username string `json:"username"`
	Password string `json:"password"`
	UserIP   string `json:"user_ip"`
	AuthMode string `json:"auth_mode"`
	MAC      string `json:"mac"`
	Ticket   string `json:"auth_ticket"`
}

// AuthResult maps the NAS ErrCode onto a stable API code.
type AuthResult struct {
	Code          string `json:"code"`
	ErrCode       byte   `json:"err_code"`
	Message       string `json:"message"`
	NASInfo       string `json:"nas_info"`
	Serial        uint16 `json:"serial"`
	Authenticated bool   `json:"authenticated"`
}

// LogoutRequest ends a Portal session.
type LogoutRequest struct {
	NASAddr string `json:"nas_addr"`
	UserIP  string `json:"user_ip"`
}

func parseUserIP(value string) (net.IP, error) {
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil {
		return nil, errors.New("portal: user_ip must be a valid IPv4 address")
	}
	return ip, nil
}

// errCodeMessage maps NAS ErrCodes (shared across ACK_CHALLENGE / ACK_AUTH)
// into human-readable messages. ErrLinkEstablished means the user is already
// online — treated as a success case.
func errCodeMessage(code byte) string {
	switch code {
	case ErrSuccess:
		return "success"
	case ErrReject:
		return "rejected"
	case ErrLinkEstablished:
		return "already online"
	case ErrAuthenticating:
		return "authentication in progress"
	case ErrFailed:
		return "authentication failed"
	case ErrOnlineLimit:
		return "online user limit exceeded"
	case ErrOtherAuth:
		return "user is authenticating by another method"
	case ErrUserNotFound:
		return "user not found on NAS"
	default:
		return fmt.Sprintf("NAS error 0x%02x", code)
	}
}

// Challenge runs a CHAP Challenge round trip against the NAS.
func (s *Service) Challenge(req ChallengeRequest) (*ChallengeResult, error) {
	if req.Username == "" {
		return nil, errors.New("portal: username is required")
	}
	ip, err := parseUserIP(req.UserIP)
	if err != nil {
		return nil, err
	}
	nas, err := s.Server.ResolveNAS(req.NASAddr)
	if err != nil {
		return nil, err
	}
	session, err := s.Server.Challenge(nas, req.Username, ip)
	if err != nil {
		return nil, err
	}
	result := &ChallengeResult{
		Serial:   session.SerialNo,
		UserIP:   req.UserIP,
		Username: req.Username,
		ReqID:    session.ReqID,
		Message:  "challenge accepted",
	}
	if s.Tickets != nil {
		ticket, err := s.Tickets.Issue(req.Username, req.UserIP, req.MAC, session.SerialNo)
		if err != nil {
			return nil, err
		}
		result.Ticket = ticket
	}
	return result, nil
}

// Authenticate sends REQ_AUTH using CHAP (default) or PAP.
func (s *Service) Authenticate(req AuthRequest) (*AuthResult, error) {
	if req.Username == "" || req.Password == "" {
		return nil, errors.New("portal: username and password are required")
	}
	ip, err := parseUserIP(req.UserIP)
	if err != nil {
		return nil, err
	}
	if s.Tickets != nil || s.RequireTicket {
		if s.Tickets == nil {
			return nil, errors.New("portal: ticket manager is not configured")
		}
		serial, err := s.Tickets.Verify(req.Ticket, req.Username, req.UserIP, req.MAC)
		if err != nil {
			return nil, err
		}
		if session, ok := s.Server.SessionByIP(req.UserIP); ok && serial != session.SerialNo {
			return nil, errors.New("portal: auth_ticket does not match the active challenge")
		}
	}
	pap := false
	switch req.AuthMode {
	case "", "chap":
	case "pap":
		pap = true
	default:
		return nil, fmt.Errorf("portal: unsupported auth_mode %q", req.AuthMode)
	}
	nas, err := s.Server.ResolveNAS(req.NASAddr)
	if err != nil {
		return nil, err
	}
	code, info, err := s.Server.Authenticate(nas, ip, req.Username, req.Password, pap)
	if err != nil {
		return nil, err
	}
	success := code == ErrSuccess || code == ErrLinkEstablished
	result := &AuthResult{
		Code:          "REJECT",
		ErrCode:       code,
		Message:       errCodeMessage(code),
		NASInfo:       info,
		Authenticated: success,
	}
	if success {
		result.Code = "ACCEPT"
	}
	if session, ok := s.Server.SessionByIP(req.UserIP); ok {
		result.Serial = session.SerialNo
	}
	return result, nil
}

// Logout ends a Portal session via REQ_LOGOUT.
func (s *Service) Logout(req LogoutRequest) error {
	ip, err := parseUserIP(req.UserIP)
	if err != nil {
		return err
	}
	nas, err := s.Server.ResolveNAS(req.NASAddr)
	if err != nil {
		return err
	}
	return s.Server.Logout(nas, ip)
}

// SessionStatus returns the current session for a user IP, if any.
func (s *Service) SessionStatus(userIP string) (*Session, bool) {
	return s.Server.SessionByIP(userIP)
}
