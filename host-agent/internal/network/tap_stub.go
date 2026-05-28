//go:build !linux

package network

import (
	"fmt"
	"net"
)

// Stub implementations for non-Linux platforms (local development only).
// Real TAP/iptables setup only runs on Linux with KVM.

type TAPDevice struct {
	Name    string
	HostIP  net.IP
	GuestIP net.IP
	MAC     net.HardwareAddr
}

func Create(vmID string, index int) (*TAPDevice, error) {
	return nil, fmt.Errorf("TAP devices are only supported on Linux")
}

func Delete(name string) error {
	return nil
}
