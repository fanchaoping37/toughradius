// Package dnsintercept implements RFC 1035 DNS redirection for unauthenticated
// subscribers: configured queries receive an A record pointing at the Portal,
// while whitelisted domains are passed through to an upstream resolver.
//
// The DNS redirect itself is a deployment practice, not an RFC-defined
// protocol. All wire-format details (header, QNAME labels, A records) are
// from RFC 1035 §3.1/§4.1.
package dnsintercept

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	headerLen   = 12
	typeA       = 1
	classIN     = 1
	maxLabelLen = 63
	maxNameLen  = 255
)

var (
	ErrShortMessage = errors.New("dns: message shorter than header")
	ErrTruncated    = errors.New("dns: truncated question")
	ErrBadLabel     = errors.New("dns: invalid QNAME label")
)

// Config configures the DNS interceptor.
type Config struct {
	Enabled      bool
	ListenAddr   string                     // ":53"
	PortalIP     net.IP                     // A record returned for redirected queries
	Upstream     string                     // e.g. "8.8.8.8:53"; empty disables passthrough
	Whitelist    []string                   // exact or *.suffix domains; never redirected
	TTL          uint32                     // answer TTL; 0 means no caching (RFC 1035 §4.1.3)
	IsAuthorized func(clientIP net.IP) bool // optional policy hook
	Timeout      time.Duration
}

// Server answers DNS queries on UDP.
type Server struct {
	cfg    Config
	conn   *net.UDPConn
	done   chan struct{}
	once   sync.Once
	client *net.UDPConn
}

// NewServer validates the config.
func NewServer(cfg Config) (*Server, error) {
	if cfg.PortalIP == nil || cfg.PortalIP.To4() == nil {
		return nil, errors.New("dns: PortalIP must be an IPv4 address")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":53"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	s := &Server{cfg: cfg, done: make(chan struct{})}
	if cfg.Upstream != "" {
		conn, err := net.DialUDP("udp", nil, mustResolveUDP(cfg.Upstream))
		if err != nil {
			return nil, fmt.Errorf("dns: upstream: %w", err)
		}
		s.client = conn
	}
	return s, nil
}

func mustResolveUDP(addr string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 53}
	}
	return a
}

// Start serves DNS until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("dns: listen %s: %w", s.cfg.ListenAddr, err)
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
		if s.client != nil {
			_ = s.client.Close()
		}
	})
	return err
}

// Serve runs the receive loop on an injected socket (used in tests).
func (s *Server) Serve(conn *net.UDPConn) error {
	buf := make([]byte, 1500)
	for {
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
			}
			return fmt.Errorf("dns: read: %w", err)
		}
		query := append([]byte(nil), buf[:n]...)
		go func(remote *net.UDPAddr, data []byte) {
			resp, err := s.HandleQuery(data, remote.IP)
			if err != nil {
				zap.L().Debug("dns: drop query", zap.Error(err))
				return
			}
			if _, err := conn.WriteToUDP(resp, remote); err != nil {
				zap.L().Warn("dns: write response", zap.Error(err))
			}
		}(remote, query)
	}
}

// Question captures the decoded first query.
type Question struct {
	Name  string
	Type  uint16
	Class uint16
	// NameBytes is the wire-encoded QNAME with its terminating zero, used
	// verbatim in the forged answer to keep compression simple.
	NameBytes []byte
}

// ParseQuestion decodes the DNS header and first question section.
func ParseQuestion(msg []byte) (id uint16, flags uint16, qdcount uint16, q *Question, err error) {
	if len(msg) < headerLen {
		return 0, 0, 0, nil, ErrShortMessage
	}
	id = binary.BigEndian.Uint16(msg[0:2])
	flags = binary.BigEndian.Uint16(msg[2:4])
	qdcount = binary.BigEndian.Uint16(msg[4:6])
	if qdcount == 0 {
		return id, flags, qdcount, nil, nil
	}
	off := headerLen
	start := off
	var labels []string
	for {
		if off >= len(msg) {
			return id, flags, qdcount, nil, ErrTruncated
		}
		length := int(msg[off])
		if length&0xc0 != 0 {
			return id, flags, qdcount, nil, ErrBadLabel // compression pointers not decoded
		}
		off++
		if length == 0 {
			break
		}
		if length > maxLabelLen || off+length > len(msg) {
			return id, flags, qdcount, nil, ErrBadLabel
		}
		labels = append(labels, string(msg[off:off+length]))
		off += length
		if off-start > maxNameLen {
			return id, flags, qdcount, nil, ErrBadLabel
		}
	}
	nameEnd := off
	if off+4 > len(msg) {
		return id, flags, qdcount, nil, ErrTruncated
	}
	q = &Question{
		Name:      strings.Join(labels, "."),
		Type:      binary.BigEndian.Uint16(msg[off : off+2]),
		Class:     binary.BigEndian.Uint16(msg[off+2 : off+4]),
		NameBytes: append([]byte(nil), msg[start:nameEnd]...),
	}
	return id, flags, qdcount, q, nil
}

// RedirectResponse builds a DNS response with one A record = cfg.PortalIP.
// Header: QR=1, AA=1, RA=1; QDCOUNT=1, ANCOUNT=1 (RFC 1035 §4.1.1).
func RedirectResponse(query []byte, portalIP net.IP, ttl uint32) ([]byte, error) {
	id, _, _, q, err := ParseQuestion(query)
	if err != nil {
		return nil, err
	}
	if q == nil {
		return nil, ErrTruncated
	}
	resp := make([]byte, 0, 128)
	resp = append(resp, byte(id>>8), byte(id))
	resp = append(resp, 0x84, 0x80)             // QR|AA and RA|RCODE=0
	resp = append(resp, 0, 1, 0, 1, 0, 0, 0, 0) // QD=1 AN=1 NS=0 AR=0
	resp = append(resp, q.NameBytes...)
	resp = append(resp, 0, byte(typeA), 0, byte(classIN))
	// Answer NAME as a compression pointer to QNAME at offset 12 (RFC 1035 §4.1.4).
	resp = append(resp, 0xc0, 0x0c)
	resp = append(resp, 0, byte(typeA), 0, byte(classIN))
	resp = append(resp, byte(ttl>>24), byte(ttl>>16), byte(ttl>>8), byte(ttl))
	resp = append(resp, 0, 4)
	ip := portalIP.To4()
	resp = append(resp, ip[0], ip[1], ip[2], ip[3])
	return resp, nil
}

// HandleQuery decides between redirect and upstream passthrough.
func (s *Server) HandleQuery(query []byte, clientIP net.IP) ([]byte, error) {
	_, _, _, q, err := ParseQuestion(query)
	if err != nil {
		return nil, err
	}
	if q == nil {
		return nil, ErrTruncated
	}
	if s.cfg.IsAuthorized != nil && s.cfg.IsAuthorized(clientIP) {
		return s.passthrough(query)
	}
	if q.Type != typeA || q.Class != classIN {
		return s.passthrough(query)
	}
	if s.isWhitelisted(q.Name) {
		return s.passthrough(query)
	}
	return RedirectResponse(query, s.cfg.PortalIP, s.cfg.TTL)
}

func (s *Server) isWhitelisted(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, entry := range s.cfg.Whitelist {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, "*.") {
			suffix := strings.TrimPrefix(entry, "*.")
			if name == suffix || strings.HasSuffix(name, "."+suffix) {
				return true
			}
		} else if entry == "*" {
			return true
		} else if name == entry {
			return true
		}
	}
	return false
}

func (s *Server) passthrough(query []byte) ([]byte, error) {
	if s.client == nil {
		// Fail closed: hand the Portal address even for whitelisted names when
		// no upstream resolver is configured, so the captive flow still works.
		return RedirectResponse(query, s.cfg.PortalIP, s.cfg.TTL)
	}
	if _, err := s.client.Write(query); err != nil {
		return nil, err
	}
	if err := s.client.SetReadDeadline(time.Now().Add(s.cfg.Timeout)); err != nil {
		return nil, err
	}
	buf := make([]byte, 1500)
	n, err := s.client.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
