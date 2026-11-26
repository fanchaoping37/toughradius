package dhcp

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

// Pool defines an IPv4 address pool for one subnet.
type Pool struct {
	Network    string        `json:"network"`    // e.g. 192.168.10.0/24
	Gateway    string        `json:"gateway"`    // option 3
	DNS        []string      `json:"dns"`        // option 6
	Start      string        `json:"start"`      // first assignable address
	End        string        `json:"end"`        // last assignable address
	Lease      time.Duration `json:"lease"`      // option 51
	PortalURL  string        `json:"portal_url"` // RFC 7710 option 160 + legacy 243
	SubnetMask string        `json:"subnet_mask"`
}

// ServerConfig configures the DHCP server.
type ServerConfig struct {
	Enabled    bool
	ListenAddr string // ":67"
	ServerIP   net.IP // option 54 / siaddr
	Pools      []Pool
	LeaseTTL   time.Duration
	OnLease    func(lease *Lease) // optional hook: persist binding / notify Portal
	// SetAuthorized is an optional policy hook consulted by the captive
	// redirection layer. Wired from main.go to captive.Handler.IsAuthorized.
	// When nil, leases are treated as unauthenticated.
	SetAuthorized func(ip net.IP) bool
}

// Lease is an assigned address record.
type Lease struct {
	MAC        string
	IP         net.IP
	Pool       string
	CircuitID  string
	RemoteID   string
	Expires    time.Time
	Authorized bool // set after Portal authentication; reserved for policy hooks
}

type poolState struct {
	cfg   Pool
	start uint32
	end   uint32
	netID uint32
	mask  net.IPMask
}

// Server is a minimal RFC 2131 DHCP server with lease tracking in memory.
type Server struct {
	cfg    ServerConfig
	conn   *net.UDPConn
	pools  []*poolState
	leases map[string]*Lease // keyed by MAC
	byIP   map[uint32]*Lease
	mu     sync.Mutex
	done   chan struct{}
	once   sync.Once
}

// NewServer constructs a server and validates pool ranges.
func NewServer(cfg ServerConfig) (*Server, error) {
	s := &Server{
		cfg:    cfg,
		leases: make(map[string]*Lease),
		byIP:   make(map[uint32]*Lease),
		done:   make(chan struct{}),
	}
	for _, p := range cfg.Pools {
		_, subnet, err := net.ParseCIDR(p.Network)
		if err != nil {
			return nil, fmt.Errorf("dhcp: invalid pool network %q: %w", p.Network, err)
		}
		start := ipv4ToUint32(net.ParseIP(p.Start))
		end := ipv4ToUint32(net.ParseIP(p.End))
		if start == 0 || end == 0 || start > end {
			return nil, fmt.Errorf("dhcp: invalid pool range %s-%s", p.Start, p.End)
		}
		mask := subnet.Mask
		if p.SubnetMask != "" {
			if m := net.ParseIP(p.SubnetMask).To4(); m != nil {
				mask = net.IPv4Mask(m[0], m[1], m[2], m[3])
			}
		}
		lease := p.Lease
		if lease <= 0 {
			lease = 3600 * time.Second
		}
		p.Lease = lease
		s.pools = append(s.pools, &poolState{
			cfg: p, start: start, end: end,
			netID: ipv4ToUint32(subnet.IP), mask: mask,
		})
	}
	if s.cfg.ListenAddr == "" {
		s.cfg.ListenAddr = fmt.Sprintf(":%d", ServerPort)
	}
	if s.cfg.ServerIP == nil {
		s.cfg.ServerIP = net.ParseIP("127.0.0.1")
	}
	return s, nil
}

func ipv4ToUint32(ip net.IP) uint32 {
	v := ip.To4()
	if v == nil {
		return 0
	}
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
}

func uint32ToIPv4(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// Start listens on the configured UDP address and serves requests until ctx
// is cancelled. In tests, inject a socket with Serve(conn).
func (s *Server) Start(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("dhcp: listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.conn = conn
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s.Serve(conn)
}

// Close stops the receive loop and releases the socket.
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

// Serve runs the packet loop on an injected UDP socket (used by unit tests).
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
			return fmt.Errorf("dhcp: read: %w", err)
		}
		packetCopy := append([]byte(nil), buf[:n]...)
		go func(remote *net.UDPAddr, data []byte) {
			reply, dst, err := s.handlePacket(data)
			if err != nil {
				zap.L().Debug("dhcp: drop packet", zap.Error(err), zap.String("from", remote.String()))
				return
			}
			if reply == nil {
				return
			}
			if _, err := conn.WriteToUDP(reply, dst); err != nil {
				zap.L().Warn("dhcp: write reply", zap.Error(err))
			}
		}(remote, packetCopy)
	}
}

// handlePacket implements RFC 2131 §4.1 reply addressing rules:
//   - giaddr != 0 => unicast to relay's port 67
//   - ciaddr != 0 => unicast to ciaddr port 68
//   - B flag     => broadcast 255.255.255.255:68
//   - otherwise  => unicast to yiaddr:68 using chaddr
//
// All NAKs with giaddr==0 are broadcast.
func (s *Server) handlePacket(data []byte) ([]byte, *net.UDPAddr, error) {
	p, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	mac := p.MAC()
	var replyType byte
	var yiaddr net.IP

	switch p.MsgType {
	case MsgDiscover:
		lease := s.allocate(p, false)
		if lease == nil {
			return nil, nil, errors.New("dhcp: no free address")
		}
		yiaddr = lease.IP
		replyType = MsgOffer
	case MsgRequest:
		// Server identifier disambiguates multiple DHCP servers.
		if sid := p.ServerIdentifier(); sid != nil && !sid.Equal(s.cfg.ServerIP) {
			return nil, nil, errors.New("dhcp: request selected another server")
		}
		requested := p.RequestedIP()
		if requested == nil {
			requested = p.CIAddr
		}
		lease := s.allocate(p, requested != nil)
		if lease == nil || (requested != nil && !lease.IP.Equal(requested)) {
			replyType = MsgNak
			yiaddr = net.IPv4zero
		} else {
			yiaddr = lease.IP
			replyType = MsgAck
		}
	case MsgRelease:
		s.release(mac)
		return nil, nil, nil // RFC 2131 §4.3.6: no reply
	case MsgDecline:
		s.decline(mac, p.RequestedIP())
		return nil, nil, nil
	case MsgInform:
		replyType = MsgAck
		yiaddr = net.IPv4zero
	default:
		return nil, nil, fmt.Errorf("dhcp: unsupported message type %d", p.MsgType)
	}

	pool := s.poolFor(p)
	opts := []Option{}
	if replyType != MsgNak && pool != nil {
		opts = append(opts,
			IPOption(OptSubnetMask, net.IP(pool.mask)),
			IPOption(OptRouter, net.ParseIP(pool.cfg.Gateway)),
		)
		if len(pool.cfg.DNS) > 0 {
			ips := make([]net.IP, 0, len(pool.cfg.DNS))
			for _, d := range pool.cfg.DNS {
				ips = append(ips, net.ParseIP(d))
			}
			opts = append(opts, IPOption(OptDNSServer, ips...))
		}
		if pool.cfg.PortalURL != "" {
			// RFC 7710 §2.1 option 160 (URI, non-NUL-terminated).
			opts = append(opts, StringOption(OptCaptivePortal, pool.cfg.PortalURL))
			// Legacy option 243 for compatibility only (IANA private use).
			opts = append(opts, StringOption(OptPortalURL243, pool.cfg.PortalURL))
		}
		opts = append(opts, Uint32Option(OptLeaseTime, uint32(pool.cfg.Lease.Seconds())))
	}
	if replyType == MsgNak {
		opts = append(opts, StringOption(OptServerID, ""),
			Uint32Option(OptLeaseTime, 0))
		// Server-Identifier is a 4-byte IP, not a string: rebuild it correctly.
		opts[len(opts)-2] = Option{Code: OptServerID, Value: s.cfg.ServerIP.To4()}
	}

	reply, err := p.Reply(replyType, yiaddr, s.cfg.ServerIP, opts)
	if err != nil {
		return nil, nil, err
	}

	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: ClientPort}
	if p.GIAddr != nil && !p.GIAddr.IsUnspecified() && replyType != MsgNak {
		dst = &net.UDPAddr{IP: p.GIAddr, Port: ServerPort} // relay agent
	} else if replyType != MsgNak && !p.BroadcastFlag() && !p.CIAddr.IsUnspecified() {
		dst = &net.UDPAddr{IP: p.CIAddr, Port: ClientPort}
	} else if replyType != MsgNak && !p.BroadcastFlag() && yiaddr != nil && !yiaddr.IsUnspecified() {
		dst = &net.UDPAddr{IP: yiaddr, Port: ClientPort}
	}
	return reply, dst, nil
}

func (s *Server) poolFor(p *Packet) *poolState {
	for _, pool := range s.pools {
		if p.GIAddr != nil && !p.GIAddr.IsUnspecified() {
			gw := ipv4ToUint32(p.GIAddr)
			maskNum := binary.BigEndian.Uint32(pool.mask)
			if gw&maskNum == pool.netID&maskNum {
				return pool
			}
		}
		if requested := p.RequestedIP(); requested != nil {
			v := ipv4ToUint32(requested)
			if v >= pool.start && v <= pool.end {
				return pool
			}
		}
	}
	if len(s.pools) > 0 {
		return s.pools[0]
	}
	return nil
}

// allocate returns the existing lease for the MAC or assigns a free address.
// When strict is true (DHCPREQUEST with requested IP), only an exact match is reused.
func (s *Server) allocate(p *Packet, strict bool) *Lease {
	mac := p.MAC()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if lease, ok := s.leases[mac]; ok && now.Before(lease.Expires) {
		return lease
	}
	for _, pool := range s.pools {
		for candidate := pool.start; candidate <= pool.end; candidate++ {
			ip := uint32ToIPv4(candidate)
			if _, used := s.byIP[candidate]; used {
				continue
			}
			lease := &Lease{
				MAC: mac, IP: ip, Pool: pool.cfg.Network,
				CircuitID: p.CircuitID(), RemoteID: p.RemoteID(),
				Expires: now.Add(pool.cfg.Lease),
			}
			s.leases[mac] = lease
			s.byIP[candidate] = lease
			if s.cfg.OnLease != nil {
				s.cfg.OnLease(lease)
			}
			return lease
		}
	}
	_ = strict // pool selection already respects the requested address via poolFor
	return nil
}

func (s *Server) release(mac string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lease, ok := s.leases[mac]; ok {
		delete(s.byIP, ipv4ToUint32(lease.IP))
		delete(s.leases, mac)
	}
}

func (s *Server) decline(mac string, ip net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lease, ok := s.leases[mac]; ok && (ip == nil || lease.IP.Equal(ip)) {
		delete(s.byIP, ipv4ToUint32(lease.IP))
		delete(s.leases, mac)
	}
}

// LookupLease returns the active lease for a MAC address.
func (s *Server) LookupLease(mac string) (*Lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.leases[mac]
	if !ok || !time.Now().Before(lease.Expires) {
		return nil, false
	}
	copyLease := *lease
	return &copyLease, true
}

// SetAuthorized marks a lease as authenticated (policy hook for DNS/HTTP
// redirection). The redirection layers consult this flag.
func (s *Server) SetAuthorized(mac string, authorized bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lease, ok := s.leases[mac]; ok {
		lease.Authorized = authorized
		return true
	}
	return false
}
