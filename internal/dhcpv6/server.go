package dhcpv6

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Binding is an IPv6 address -> client identity record used for source tracing.
type Binding struct {
	IPv6     net.IP
	DUID     string
	IAID     uint32
	MAC      string
	Username string // filled when Portal/RADIUS authentication is correlated
	Expires  time.Time
	LastSeen time.Time
}

// Config configures the DHCPv6 server.
type Config struct {
	Enabled    bool
	BindAddr   string // :547
	Prefix     net.IPNet
	DNS        []net.IP
	PortalURL  string
	Lease      time.Duration
	ServerDUID []byte
	OnBinding  func(*Binding)
}

// Server assigns IPv6 addresses from a /64 prefix and records DUID bindings.
type Server struct {
	cfg      Config
	conn     *net.UDPConn
	done     chan struct{}
	once     sync.Once
	mu       sync.RWMutex
	bindings map[string]*Binding // key: DUID:IAID
	byAddr   map[string]*Binding
	next     uint64
}

// NewServer validates config and starts with a fresh binding table.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Prefix.IP == nil || cfg.Prefix.Mask == nil {
		return nil, errors.New("dhcpv6: prefix required")
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 2 * time.Hour
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = fmt.Sprintf(":%d", ServerPort)
	}
	if len(cfg.ServerDUID) < 2 {
		// DUID-LLT style server identifier (RFC 3315 §9.2).
		cfg.ServerDUID = []byte{0, DUID_LLT, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	}
	return &Server{
		cfg: cfg, done: make(chan struct{}),
		bindings: make(map[string]*Binding), byAddr: make(map[string]*Binding),
	}, nil
}

// Start serves DHCPv6 until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", s.cfg.BindAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp6", addr)
	if err != nil {
		return fmt.Errorf("dhcpv6: listen %s: %w", s.cfg.BindAddr, err)
	}
	s.conn = conn
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s.Serve(conn)
}

// Close stops the server.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		if s.conn != nil {
			err = s.conn.Close()
		}
	})
	return err
}

// Serve runs the receive loop on an injected socket (used in tests).
func (s *Server) Serve(conn *net.UDPConn) error {
	buf := make([]byte, 2048)
	for {
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
			}
			return fmt.Errorf("dhcpv6: read: %w", err)
		}
		data := append([]byte(nil), buf[:n]...)
		go func(remote *net.UDPAddr, data []byte) {
			resp := s.Handle(data)
			if resp == nil {
				return
			}
			if _, err := conn.WriteToUDP(resp, remote); err != nil {
				zap.L().Warn("dhcpv6: write", zap.Error(err))
			}
		}(remote, data)
	}
}

// Handle processes one DHCPv6 message and returns a reply (or nil to drop).
func (s *Server) Handle(data []byte) []byte {
	p, err := Parse(data)
	if err != nil {
		return nil
	}
	switch p.MsgType {
	case MsgSolicit, MsgRequest, MsgRenew, MsgRebind:
		return s.reply(p)
	case MsgInformationRequest:
		return Encode(MsgReply, p.TxID, s.infoOptions())
	case MsgRelease, MsgDecline:
		s.release(p)
		return nil
	default:
		return nil
	}
}

func (s *Server) infoOptions() []Option {
	opts := []Option{{Code: OptServerID, Value: s.cfg.ServerDUID}}
	if len(s.cfg.DNS) > 0 {
		opts = append(opts, DNSOption(s.cfg.DNS...))
	}
	if s.cfg.PortalURL != "" {
		opts = append(opts, CaptiveURIOption(s.cfg.PortalURL))
	}
	return opts
}

func (s *Server) reply(p *Packet) []byte {
	duidRaw := p.ClientDUID()
	if len(duidRaw) == 0 {
		return nil
	}
	duid, err := ParseDUID(duidRaw)
	if err != nil {
		return nil
	}
	iaRaw := p.Options[OptIA_NA]
	if len(iaRaw) == 0 {
		// Information-only request: Reply without IA_NA.
		return Encode(MsgReply, p.TxID, s.infoOptions())
	}
	ia, err := ParseIA_NA(iaRaw)
	if err != nil {
		return nil
	}
	addr := s.assign(duid, ia)
	if addr == nil {
		return nil
	}
	iaOpt, err := IA_NAOption(ia.IAID, addr,
		uint32(s.cfg.Lease.Seconds()*2/3), uint32(s.cfg.Lease.Seconds()),
		uint32(s.cfg.Lease.Seconds()/2), uint32(s.cfg.Lease.Seconds()*5/6)) //nolint:gosec
	if err != nil {
		return nil
	}
	opts := []Option{{Code: OptClientID, Value: duidRaw}, {Code: OptServerID, Value: s.cfg.ServerDUID}, iaOpt}
	opts = append(opts, s.infoOptions()[1:]...)
	return Encode(MsgReply, p.TxID, opts)
}

func (s *Server) assign(duid *DUID, ia *IA_NA) net.IP {
	key := fmt.Sprintf("%s:%d", duid.Key(), ia.IAID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.bindings[key]; ok && time.Now().Before(b.Expires) {
		b.LastSeen = time.Now()
		return b.IPv6
	}
	// Deterministic IID derived from the prefix + next counter (test/dev safe).
	base := s.cfg.Prefix.IP.To16()
	if base == nil {
		return nil
	}
	addr := make(net.IP, 16)
	copy(addr, base)
	s.next++
	for i := 0; i < 8 && s.next>>(uint(8*i)) > 0; i++ {
		addr[8+i] = byte(s.next >> (uint(8 * i)))
	}
	if s.next>>56 > 0 {
		return nil // /64 exhausted
	}
	// Avoid collisions with an existing binding.
	for _, used := range s.byAddr {
		if used.IPv6.Equal(addr) {
			s.next++
			return s.assignLocked(duid, key, ia)
		}
	}
	b := &Binding{
		IPv6: addr, DUID: duid.Key(), IAID: ia.IAID,
		Expires: time.Now().Add(s.cfg.Lease), LastSeen: time.Now(),
	}
	s.bindings[key] = b
	s.byAddr[addr.String()] = b
	if s.cfg.OnBinding != nil {
		s.cfg.OnBinding(b)
	}
	return addr
}

// assignLocked assigns after a collision without re-locking.
func (s *Server) assignLocked(duid *DUID, key string, ia *IA_NA) net.IP {
	base := s.cfg.Prefix.IP.To16()
	addr := make(net.IP, 16)
	copy(addr, base)
	for i := 0; i < 8 && s.next>>(uint(8*i)) > 0; i++ {
		addr[8+i] = byte(s.next >> (uint(8 * i)))
	}
	if s.next>>56 > 0 || s.byAddr[addr.String()] != nil {
		return nil
	}
	b := &Binding{IPv6: addr, DUID: duid.Key(), IAID: ia.IAID,
		Expires: time.Now().Add(s.cfg.Lease), LastSeen: time.Now()}
	s.bindings[key] = b
	s.byAddr[addr.String()] = b
	return b.IPv6
}

func (s *Server) release(p *Packet) {
	duidRaw := p.ClientDUID()
	if len(duidRaw) == 0 {
		return
	}
	duid, err := ParseDUID(duidRaw)
	if err != nil {
		return
	}
	iaRaw := p.Options[OptIA_NA]
	if len(iaRaw) < 4 {
		return
	}
	iaid := binary.BigEndian.Uint32(iaRaw[0:4])
	key := fmt.Sprintf("%s:%d", duid.Key(), iaid)
	s.mu.Lock()
	if b, ok := s.bindings[key]; ok {
		delete(s.byAddr, b.IPv6.String())
		delete(s.bindings, key)
	}
	s.mu.Unlock()
}

// LookupByAddr finds a DUID binding for source tracing.
func (s *Server) LookupByAddr(addr net.IP) (*Binding, bool) {
	s.mu.RLock()
	b, ok := s.byAddr[addr.String()]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	copyB := *b
	return &copyB, true
}

// SetUsername correlates an authenticated user name with a DUID binding.
func (s *Server) SetUsername(duidKey string, username string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bindings {
		if b.DUID == duidKey {
			b.Username = username
			return true
		}
	}
	return false
}
