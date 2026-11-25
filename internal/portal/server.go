package portal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Errors returned by the Portal server.
var (
	ErrNoSession      = errors.New("portal: no active session for user")
	ErrNoChallenge    = errors.New("portal: session has no valid challenge")
	ErrNASNotFound    = errors.New("portal: NAS not configured")
	ErrNASRejected    = errors.New("portal: NAS rejected the request")
	ErrRequestTimeout = errors.New("portal: no response from NAS")
)

// NAS is a Portal-capable access device.
type NAS struct {
	Addr   string // "host:port"; port defaults to 2000 when omitted
	Secret string // Portal shared secret
}

// ServerConfig controls the Portal UDP server.
type ServerConfig struct {
	BindHost    string                           // listener host (default 0.0.0.0)
	BindPort    int                              // listener port (default 50100)
	NASPort     int                              // default NAS Portal port (default 2000)
	Timeout     time.Duration                    // response wait per attempt (default 5s)
	Retries     int                              // retransmissions after the first try (default 2)
	SessionTTL  time.Duration                    // session lifetime (default 10m)
	SecretFor   func(host string) (string, bool) // resolves shared secret by NAS host
	OnNtfLogout func(userIP, username string)    // called on NAS-initiated forced logout
}

func (c *ServerConfig) normalize() {
	if c.BindHost == "" {
		c.BindHost = "0.0.0.0"
	}
	if c.BindPort <= 0 {
		c.BindPort = 50100
	}
	if c.NASPort <= 0 {
		c.NASPort = 2000
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Second
	}
	if c.Retries < 0 {
		c.Retries = 0
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = 10 * time.Minute
	}
}

// Session tracks one Portal authentication attempt (SerialNo is stable
// across Challenge → Auth → Logout for the same user/IP).
type Session struct {
	SerialNo  uint16
	UserIP    net.IP
	Username  string
	ReqID     uint16
	Challenge []byte
	UpdatedAt time.Time
}

type pendingRequest struct {
	auth   [16]byte
	result chan *Packet
}

// Server implements the Portal V2 protocol side. It sends requests to NAS
// devices and processes their responses/notifications on a single UDP socket.
type Server struct {
	cfg       ServerConfig
	conn      net.PacketConn
	serial    atomic.Uint32
	sessions  map[uint16]*Session
	byUserIP  map[string]uint16
	pending   map[uint16]*pendingRequest
	nasByHost map[string]*NAS
	mu        sync.RWMutex
	closeOnce sync.Once
	done      chan struct{}
}

func NewServer(cfg ServerConfig) *Server {
	cfg.normalize()
	s := &Server{
		cfg:       cfg,
		sessions:  make(map[uint16]*Session),
		byUserIP:  make(map[string]uint16),
		pending:   make(map[uint16]*pendingRequest),
		nasByHost: make(map[string]*NAS),
		done:      make(chan struct{}),
	}
	s.serial.Store(uint32(time.Now().UnixNano() & 0xffff))
	return s
}

// Listen binds the UDP socket and starts the receive loop. It blocks until
// Close is called or a fatal socket error occurs.
func (s *Server) Listen() error {
	addr := net.JoinHostPort(s.cfg.BindHost, strconv.Itoa(s.cfg.BindPort))
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("portal: listen %s: %w", addr, err)
	}
	s.conn = conn
	zap.S().Infof("Starting Huawei/CMCC Portal V2 server on %s", addr)
	return s.serve(conn)
}

// serve runs the receive loop on an already bound socket. Tests inject a
// loopback socket through this method.
func (s *Server) serve(conn net.PacketConn) error {
	go s.cleanupLoop()
	buffer := make([]byte, MaxPacketLen+1)
	for {
		n, remote, err := conn.ReadFrom(buffer)
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
			}
			return fmt.Errorf("portal: read: %w", err)
		}
		go s.handlePacket(buffer[:n], remote)
	}
}

// Close stops the server. Safe to call multiple times.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		if s.conn != nil {
			err = s.conn.Close()
		}
	})
	return err
}

// nextSerial returns a non-zero, monotonically increasing SerialNo.
func (s *Server) nextSerial() uint16 {
	for {
		v := uint16(s.serial.Add(1))
		if v != 0 {
			return v
		}
	}
}

// RegisterNAS registers a Portal-capable NAS. The host key is the address
// without a port; the stored Addr (with optional port) is used when sending.
func (s *Server) RegisterNAS(nas *NAS) error {
	host, _, err := nasHostPort(nas, s.cfg.NASPort)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.nasByHost[host] = &NAS{Addr: nas.Addr, Secret: nas.Secret}
	s.mu.Unlock()
	return nil
}

// UnregisterNAS removes a registered NAS.
func (s *Server) UnregisterNAS(addr string) {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	s.mu.Lock()
	delete(s.nasByHost, host)
	s.mu.Unlock()
}

// ResolveNAS looks up a registered NAS by address (with or without port).
// Unregistered addresses are accepted when SecretFor resolves them, which
// keeps backward compatibility with config-file-only deployments.
func (s *Server) ResolveNAS(addr string) (*NAS, error) {
	host, _, err := nasHostPort(&NAS{Addr: addr}, s.cfg.NASPort)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	nas := s.nasByHost[host]
	s.mu.RUnlock()
	if nas != nil {
		return &NAS{Addr: nas.Addr, Secret: nas.Secret}, nil
	}
	if secret, ok := s.secretFor(host); ok {
		return &NAS{Addr: addr, Secret: string(secret)}, nil
	}
	return nil, ErrNASNotFound
}

// secretFor resolves the shared secret for a NAS host. Unknown hosts are
// rejected — answering with a wrong secret would leak protocol existence.
func (s *Server) secretFor(host string) ([]byte, bool) {
	s.mu.RLock()
	nas := s.nasByHost[host]
	s.mu.RUnlock()
	if nas != nil && nas.Secret != "" {
		return []byte(nas.Secret), true
	}
	if s.cfg.SecretFor != nil {
		secret, ok := s.cfg.SecretFor(host)
		if ok && secret != "" {
			return []byte(secret), true
		}
	}
	return nil, false
}

func nasHostPort(nas *NAS, defaultPort int) (string, string, error) {
	if nas == nil || nas.Addr == "" {
		return "", "", ErrNASNotFound
	}
	host, port, err := net.SplitHostPort(nas.Addr)
	if err != nil {
		host = nas.Addr
		port = strconv.Itoa(defaultPort)
	}
	if port == "" {
		port = strconv.Itoa(defaultPort)
	}
	if host == "" {
		return "", "", ErrNASNotFound
	}
	return host, net.JoinHostPort(host, port), nil
}

// handlePacket processes one inbound NAS packet: ACKs are routed to the
// waiting sendAndWait call; notifications are handled inline.
func (s *Server) handlePacket(data []byte, remote net.Addr) {
	packet, err := Parse(data)
	if err != nil {
		zap.L().Warn("portal packet parse failed",
			zap.String("namespace", "portal"),
			zap.String("remote", remote.String()), zap.Error(err))
		return
	}
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		host = remote.String()
	}
	secret, ok := s.secretFor(host)
	if !ok {
		zap.L().Warn("portal packet from unknown NAS dropped",
			zap.String("namespace", "portal"), zap.String("remote", host))
		return
	}

	switch packet.Type {
	case TypeAckChallenge, TypeAckAuth, TypeAckLogout, TypeAckInfo:
		s.handleAck(packet, secret)
	case TypeNtfLogout:
		if !packet.Verify(secret, nil) {
			zap.L().Warn("portal NTF_LOGOUT invalid authenticator", zap.String("remote", host))
			return
		}
		s.handleNtfLogout(packet, secret, remote)
	case TypeStatusNotify:
		if !packet.Verify(secret, nil) {
			zap.L().Warn("portal STATUS_NOTIFY invalid authenticator", zap.String("remote", host))
			return
		}
		s.ackNotification(TypeAckStatus, packet, secret, remote)
	default:
		zap.L().Warn("portal unsupported message dropped",
			zap.String("type", packet.Type.String()), zap.String("remote", host))
	}
}

func (s *Server) handleAck(packet *Packet, secret []byte) {
	s.mu.RLock()
	pend, ok := s.pending[packet.SerialNo]
	s.mu.RUnlock()
	if !ok {
		return // late or duplicate response
	}
	if !packet.Verify(secret, &pend.auth) {
		zap.L().Warn("portal ACK invalid authenticator",
			zap.Uint16("serial", packet.SerialNo), zap.String("type", packet.Type.String()))
		return
	}
	select {
	case pend.result <- packet:
	default:
	}
}

func (s *Server) handleNtfLogout(packet *Packet, secret []byte, remote net.Addr) {
	userIP := packet.UserIP.String()
	username := packet.GetStringAttr(AttrUserName)
	zap.S().Infow("portal forced logout notification",
		"namespace", "portal", "user_ip", userIP, "username", username)
	s.mu.Lock()
	if serial, ok := s.byUserIP[userIP]; ok {
		delete(s.byUserIP, userIP)
		delete(s.sessions, serial)
	}
	s.mu.Unlock()
	if s.cfg.OnNtfLogout != nil {
		s.cfg.OnNtfLogout(userIP, username)
	}
	s.ackNotification(TypeAckNtfLogout, packet, secret, remote)
}

func (s *Server) ackNotification(typ MessageType, request *Packet, secret []byte, remote net.Addr) {
	resp := NewPacket(typ, 0, request.SerialNo, request.ReqID, request.UserIP, ErrSuccess)
	wire, err := resp.EncodeRequest(secret)
	if err != nil {
		zap.L().Error("portal notification ACK encode failed", zap.Error(err))
		return
	}
	if s.conn != nil {
		if _, err := s.conn.WriteTo(wire, remote); err != nil {
			zap.L().Error("portal notification ACK write failed", zap.Error(err))
		}
	}
}

// sendAndWait sends a request, retransmitting the identical wire bytes on
// timeout, and returns the first valid ACK. Request Authenticator stays
// constant across retransmissions as required by the protocol.
func (s *Server) sendAndWait(nas *NAS, packet *Packet) (*Packet, error) {
	host, addr, err := nasHostPort(nas, s.cfg.NASPort)
	if err != nil {
		return nil, err
	}
	secret, ok := s.secretFor(host)
	if !ok {
		return nil, ErrNASNotFound
	}
	wire, err := packet.EncodeRequest(secret)
	if err != nil {
		return nil, err
	}
	if s.conn == nil {
		return nil, errors.New("portal: server is not listening")
	}

	pend := &pendingRequest{auth: packet.Authenticator, result: make(chan *Packet, 1)}
	s.mu.Lock()
	s.pending[packet.SerialNo] = pend
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, packet.SerialNo)
		s.mu.Unlock()
	}()

	for attempt := 0; attempt <= s.cfg.Retries; attempt++ {
		if _, err := s.conn.WriteTo(wire, mustResolveUDP(addr)); err != nil {
			return nil, fmt.Errorf("portal: write to NAS: %w", err)
		}
		timer := time.NewTimer(s.cfg.Timeout)
		select {
		case resp := <-pend.result:
			timer.Stop()
			return resp, nil
		case <-timer.C:
			if attempt == s.cfg.Retries {
				return nil, ErrRequestTimeout
			}
			zap.S().Debugw("portal request timed out, retransmitting",
				"namespace", "portal", "serial", packet.SerialNo, "type", packet.Type.String(), "attempt", attempt+1)
		case <-s.done:
			timer.Stop()
			return nil, errors.New("portal: server closed")
		}
	}
	return nil, ErrRequestTimeout
}

// mustResolveUDP converts a "host:port" string to a UDP address.
func mustResolveUDP(addr string) net.Addr {
	if a, err := net.ResolveUDPAddr("udp", addr); err == nil {
		return a
	}
	return &net.UDPAddr{} // unreachable in practice; WriteTo will fail visibly
}

// Challenge starts a CHAP authentication: REQ_CHALLENGE → ACK_CHALLENGE,
// caching the challenge and NAS-assigned ReqID in a new session.
func (s *Server) Challenge(nas *NAS, username string, userIP net.IP) (*Session, error) {
	if userIP == nil || userIP.To4() == nil {
		return nil, errors.New("portal: user IP must be IPv4")
	}
	serial := s.nextSerial()
	p := NewPacket(TypeReqChallenge, AuthChap, serial, 0, userIP, 0)
	resp, err := s.sendAndWait(nas, p)
	if err != nil {
		return nil, err
	}
	if resp.ErrCode != ErrSuccess && resp.ErrCode != ErrLinkEstablished {
		return nil, fmt.Errorf("%w: challenge errcode=0x%02x", ErrNASRejected, resp.ErrCode)
	}
	challenge, ok := resp.GetAttr(AttrChallenge)
	if !ok || len(challenge) != 16 {
		return nil, ErrNoChallenge
	}
	session := &Session{
		SerialNo:  serial,
		UserIP:    append(net.IP(nil), userIP.To4()...),
		Username:  username,
		ReqID:     resp.ReqID,
		Challenge: append([]byte(nil), challenge...),
		UpdatedAt: time.Now(),
	}
	s.mu.Lock()
	s.sessions[serial] = session
	s.byUserIP[userIP.String()] = serial
	s.mu.Unlock()
	return session, nil
}

// Authenticate sends REQ_AUTH with either CHAP (hashed with the cached
// challenge) or PAP (plaintext, no Challenge step needed), then sends
// AFF_ACK_AUTH on success. RADIUS itself is performed by the NAS.
func (s *Server) Authenticate(nas *NAS, userIP net.IP, username, password string, pap bool) (byte, string, error) {
	if userIP == nil || userIP.To4() == nil {
		return ErrFailed, "", errors.New("portal: user IP must be IPv4")
	}
	s.mu.RLock()
	serial, ok := s.byUserIP[userIP.String()]
	session := s.sessions[serial]
	s.mu.RUnlock()
	if !ok || session == nil {
		return ErrFailed, "", ErrNoSession
	}

	authType := AuthChap
	p := NewPacket(TypeReqAuth, authType, session.SerialNo, session.ReqID, userIP, 0)
	_ = p.AddStringAttr(AttrUserName, username)
	if pap {
		authType = AuthPap
		p.AuthType = AuthPap
		if err := p.AddStringAttr(AttrPassword, password); err != nil {
			return ErrFailed, "", err
		}
	} else {
		chap, err := ChapPassword(ChapID(session.ReqID), password, session.Challenge)
		if err != nil {
			return ErrFailed, "", err
		}
		if err := p.AddAttr(AttrChapPassword, chap[:]); err != nil {
			return ErrFailed, "", err
		}
	}

	resp, err := s.sendAndWait(nas, p)
	if err != nil {
		return ErrFailed, "", err
	}
	info := resp.GetStringAttr(AttrTextInfo)
	errCode := resp.ErrCode
	if errCode == ErrSuccess || errCode == ErrLinkEstablished {
		// AFF_ACK_AUTH has no response; it uses request-style authenticator.
		aff := NewPacket(TypeAffAckAuth, authType, session.SerialNo, resp.ReqID, userIP, ErrSuccess)
		if secret, ok := s.secretFor(nasHost(nas, s.cfg.NASPort)); ok {
			if wire, err := aff.EncodeRequest(secret); err == nil && s.conn != nil {
				if _, err := s.conn.WriteTo(wire, mustResolveUDP(nasAddr(nas, s.cfg.NASPort))); err != nil {
					zap.L().Warn("portal AFF_ACK_AUTH write failed", zap.Error(err))
				}
			}
		}
		s.mu.Lock()
		session.UpdatedAt = time.Now()
		s.mu.Unlock()
	}
	return errCode, info, nil
}

func nasHost(nas *NAS, defaultPort int) string {
	host, _, err := nasHostPort(nas, defaultPort)
	if err != nil {
		return ""
	}
	return host
}

func nasAddr(nas *NAS, defaultPort int) string {
	_, addr, err := nasHostPort(nas, defaultPort)
	if err != nil {
		return ""
	}
	return addr
}

// Logout sends REQ_LOGOUT and waits for ACK_LOGOUT, then removes the session.
func (s *Server) Logout(nas *NAS, userIP net.IP) error {
	if userIP == nil || userIP.To4() == nil {
		return errors.New("portal: user IP must be IPv4")
	}
	s.mu.RLock()
	serial, ok := s.byUserIP[userIP.String()]
	s.mu.RUnlock()
	if !ok {
		return ErrNoSession
	}
	p := NewPacket(TypeReqLogout, 0, serial, 0, userIP, 0)
	if _, err := s.sendAndWait(nas, p); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.byUserIP, userIP.String())
	delete(s.sessions, serial)
	s.mu.Unlock()
	return nil
}

// SessionByIP returns a copy of the active session for a user IP.
func (s *Server) SessionByIP(userIP string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	serial, ok := s.byUserIP[userIP]
	if !ok {
		return nil, false
	}
	session, ok := s.sessions[serial]
	if !ok {
		return nil, false
	}
	copy := *session
	return &copy, true
}

func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for serial, session := range s.sessions {
				if now.Sub(session.UpdatedAt) > s.cfg.SessionTTL {
					delete(s.byUserIP, session.UserIP.String())
					delete(s.sessions, serial)
				}
			}
			s.mu.Unlock()
		case <-s.done:
			return
		}
	}
}

// Context-aware wrapper used by tests and callers that need cancellation.
func (s *Server) Wait(ctx context.Context, userIP string) (*Session, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if session, ok := s.SessionByIP(userIP); ok {
			return session, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
