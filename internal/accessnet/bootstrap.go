// Package accessnet wires the second-development access-network services
// (DHCPv4, DNS redirect, DHCPv6 and captive HTTP 302) into the application.
// Every service is opt-in via configuration; disabled services add zero
// behavior and zero listeners.
package accessnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/talkincode/toughradius/v9/config"
	"github.com/talkincode/toughradius/v9/internal/captive"
	"github.com/talkincode/toughradius/v9/internal/dhcp"
	"github.com/talkincode/toughradius/v9/internal/dhcpv6"
	"github.com/talkincode/toughradius/v9/internal/dnsintercept"
	"go.uber.org/zap"
)

// Services holds the live access-network service instances for the process.
type Services struct {
	DHCP       *dhcp.Server
	DNS        *dnsintercept.Server
	DHCPv6     *dhcpv6.Server
	Captive    *captive.Handler
	closeFuncs []func()
}

// Start initialises and starts all enabled services. Each goroutine is
// attached to ctx; callers use errgroup or a WaitGroup to observe exits.
func Start(ctx context.Context, cfg config.AppConfig, run func(func() error)) (*Services, error) {
	svc := &Services{}
	var errs []string

	// Captive HTTP handler is shared by DHCP/DNS policy hooks and installed
	// into the web server separately (see AdminAPIRoute).
	if cfg.Captive.Enabled {
		whitelist := make([]*net.IPNet, 0)
		for _, cidr := range cfg.Captive.WhitelistCIDR {
			_, n, err := net.ParseCIDR(strings.TrimSpace(cidr))
			if err != nil {
				errs = append(errs, fmt.Sprintf("captive whitelist %s: %v", cidr, err))
				continue
			}
			whitelist = append(whitelist, n)
		}
		handler, err := captive.NewHandler(captive.Config{
			Enabled:       true,
			LoginPath:     cfg.Captive.LoginPath,
			PortalURL:     cfg.Captive.PortalURL,
			AllowedHosts:  cfg.Captive.AllowedHosts,
			Whitelist:     whitelist,
			AuthorizedTTL: time.Duration(cfg.Captive.AuthorizedTTL) * time.Second,
			Secret:        cfg.Web.Secret,
		})
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			svc.Captive = handler
			captive.LeaseLookup = func(ip string) *struct{ MAC string } {
				if svc.DHCP == nil {
					return nil
				}
				if lease, ok := svc.DHCP.LookupLease(ip); ok {
					return &struct{ MAC string }{MAC: lease.MAC}
				}
				return nil
			}
		}
	}

	if cfg.DHCP.Enabled {
		dnsList := strings.Split(cfg.DHCP.DNS, ",")
		pool := dhcp.Pool{
			Network: cfg.DHCP.Network, Gateway: cfg.DHCP.Gateway, DNS: dnsList,
			Start: cfg.DHCP.Start, End: cfg.DHCP.End,
			Lease:     time.Duration(cfg.DHCP.Lease) * time.Second,
			PortalURL: cfg.DHCP.PortalURL,
		}
		if pool.PortalURL == "" {
			pool.PortalURL = cfg.Captive.PortalURL
		}
		var authorizedHook func(net.IP) bool
		if svc.Captive != nil {
			authorizedHook = svc.Captive.IsAuthorized
		}
		server, err := dhcp.NewServer(dhcp.ServerConfig{
			Enabled: true, ListenAddr: cfg.DHCP.BindAddr,
			ServerIP: net.ParseIP(cfg.DHCP.ServerIP), Pools: []dhcp.Pool{pool},
			SetAuthorized: authorizedHook,
		})
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			svc.DHCP = server
			run(func() error {
				zap.L().Info("accessnet: DHCPv4 started", zap.String("addr", cfg.DHCP.BindAddr))
				return server.Start(ctx)
			})
		}
	}

	if cfg.DNSRedirect.Enabled {
		server, err := dnsintercept.NewServer(dnsintercept.Config{
			Enabled: true, ListenAddr: cfg.DNSRedirect.BindAddr,
			PortalIP:  net.ParseIP(cfg.DNSRedirect.PortalIP),
			Upstream:  cfg.DNSRedirect.Upstream,
			Whitelist: cfg.DNSRedirect.Whitelist,
			TTL:       uint32(cfg.DNSRedirect.TTL), //nolint:gosec
			IsAuthorized: func(ip net.IP) bool {
				if svc.Captive == nil {
					return false
				}
				return svc.Captive.IsAuthorized(ip)
			},
		})
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			svc.DNS = server
			run(func() error {
				zap.L().Info("accessnet: DNS redirect started", zap.String("addr", cfg.DNSRedirect.BindAddr))
				return server.Start(ctx)
			})
		}
	}

	if cfg.IPv6.Enabled {
		_, prefix, err := net.ParseCIDR(cfg.IPv6.Prefix)
		if err != nil {
			errs = append(errs, fmt.Sprintf("ipv6 prefix: %v", err))
		} else {
			var dnsIPs []net.IP
			for _, d := range strings.Split(cfg.IPv6.DNS, ",") {
				if ip := net.ParseIP(strings.TrimSpace(d)); ip != nil {
					dnsIPs = append(dnsIPs, ip)
				}
			}
			portalURL := cfg.IPv6.PortalURL
			if portalURL == "" {
				portalURL = cfg.Captive.PortalURL
			}
			server, err := dhcpv6.NewServer(dhcpv6.Config{
				Enabled: true, BindAddr: cfg.IPv6.BindAddr,
				Prefix: *prefix, DNS: dnsIPs, PortalURL: portalURL,
				Lease: time.Duration(cfg.IPv6.Lease) * time.Second,
			})
			if err != nil {
				errs = append(errs, err.Error())
			} else {
				svc.DHCPv6 = server
				run(func() error {
					zap.L().Info("accessnet: DHCPv6 started", zap.String("addr", cfg.IPv6.BindAddr))
					return server.Start(ctx)
				})
			}
		}
	}

	if len(errs) > 0 {
		return svc, errors.New(strings.Join(errs, "; "))
	}
	return svc, nil
}

// Stop releases all listening sockets.
func (s *Services) Stop() {
	if s == nil {
		return
	}
	if s.DHCP != nil {
		_ = s.DHCP.Close()
	}
	if s.DNS != nil {
		_ = s.DNS.Close()
	}
	if s.DHCPv6 != nil {
		_ = s.DHCPv6.Close()
	}
	for _, f := range s.closeFuncs {
		f()
	}
}
