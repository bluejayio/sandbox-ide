//go:build linux

package network

import (
	"fmt"
	"net"
	"os/exec"

	"github.com/vishvananda/netlink"
)

// TAPDevice holds the host-side details of a VM's network interface.
type TAPDevice struct {
	Name    string
	HostIP  net.IP
	GuestIP net.IP
	MAC     net.HardwareAddr
}

// Create makes a TAP device for the VM at the given index, assigns it an IP
// in a /30 subnet, and wires up iptables NAT for outbound traffic.
// index must be unique per host (0–65535).
func Create(vmID string, index int) (*TAPDevice, error) {
	name := fmt.Sprintf("tap%d", index)

	// Each VM gets a dedicated /30: 172.16.x.y where x=index/256, y=index%256
	// Host side .1, guest side .2.
	hostIP := net.ParseIP(fmt.Sprintf("172.16.%d.%d", (index/64)&0xff, ((index%64)*4)+1))
	guestIP := net.ParseIP(fmt.Sprintf("172.16.%d.%d", (index/64)&0xff, ((index%64)*4)+2))
	mac, _ := net.ParseMAC(fmt.Sprintf("AA:FC:00:00:%02x:%02x", (index>>8)&0xff, index&0xff))

	la := netlink.NewLinkAttrs()
	la.Name = name
	tap := &netlink.Tuntap{LinkAttrs: la, Mode: netlink.TUNTAP_MODE_TAP}

	if err := netlink.LinkAdd(tap); err != nil {
		return nil, fmt.Errorf("create tap %s: %w", name, err)
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, err
	}

	addr := &netlink.Addr{IPNet: &net.IPNet{IP: hostIP, Mask: net.CIDRMask(30, 32)}}
	if err := netlink.AddrAdd(link, addr); err != nil {
		netlink.LinkDel(link)
		return nil, fmt.Errorf("assign ip to %s: %w", name, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		netlink.LinkDel(link)
		return nil, fmt.Errorf("bring up %s: %w", name, err)
	}

	if err := setupNAT(name); err != nil {
		netlink.LinkDel(link)
		return nil, err
	}

	return &TAPDevice{Name: name, HostIP: hostIP, GuestIP: guestIP, MAC: mac}, nil
}

// Delete removes the TAP device and its iptables rules.
func Delete(name string) error {
	teardownNAT(name)
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil // already gone
	}
	return netlink.LinkDel(link)
}

// setupNAT adds iptables rules for a TAP device:
//   - MASQUERADE outbound traffic via the host's default interface (eth0)
//   - DROP lateral traffic between TAP devices (tenant isolation)
func setupNAT(tapName string) error {
	rules := [][]string{
		// Outbound NAT
		{"-t", "nat", "-A", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"},
		// Allow TAP → external
		{"-A", "FORWARD", "-i", tapName, "-o", "eth0", "-j", "ACCEPT"},
		// Allow established return traffic
		{"-A", "FORWARD", "-i", "eth0", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		// Block inter-VM traffic (tap → tap*)
		{"-A", "FORWARD", "-i", tapName, "-o", "tap+", "-j", "DROP"},
	}
	for _, rule := range rules {
		if out, err := exec.Command("iptables", rule...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %v: %s", rule, out)
		}
	}
	return nil
}

func teardownNAT(tapName string) {
	rules := [][]string{
		{"-t", "nat", "-D", "POSTROUTING", "-o", "eth0", "-j", "MASQUERADE"},
		{"-D", "FORWARD", "-i", tapName, "-o", "eth0", "-j", "ACCEPT"},
		{"-D", "FORWARD", "-i", "eth0", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{"-D", "FORWARD", "-i", tapName, "-o", "tap+", "-j", "DROP"},
	}
	for _, rule := range rules {
		exec.Command("iptables", rule...).Run()
	}
}
