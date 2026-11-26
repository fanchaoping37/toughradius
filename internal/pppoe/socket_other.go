//go:build !linux

package pppoe

import (
	"context"
	"errors"
	"net"
)

// RawSocket is unavailable on non-Linux platforms (no AF_PACKET). The wire
// protocol layer remains available for simulation and testing.
type RawSocket struct{}

var errUnsupported = errors.New("pppoe: AF_PACKET raw socket requires Linux")

func OpenRawSocket(string) (*RawSocket, error) { return nil, errUnsupported }
func (s *RawSocket) Close() error              { return nil }
func (s *RawSocket) Send([]byte) error         { return errUnsupported }
func (s *RawSocket) Serve(context.Context, string, func(*Packet, net.HardwareAddr) ([]Tag, error)) error {
	return errUnsupported
}
