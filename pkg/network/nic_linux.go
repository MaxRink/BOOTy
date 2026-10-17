//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
)

// excludedPrefixes lists interface name prefixes to exclude from physical NIC detection.
var excludedPrefixes = []string{
	"lo", "docker", "veth", "vx", "br", "dummy", "virbr", "bond", "tun", "tap",
}

// Package-level function variables allow tests to inject fakes without build constraints.
var (
	linkList       = func() ([]netlink.Link, error) { return RetryDump(netlink.LinkList) }
	listInterfaces = net.Interfaces
	ifaceAddrs     = func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() }
	sleepFunc      = time.Sleep
)

// DetectPhysicalNICs returns the names of all physical network interfaces,
// excluding loopback, virtual, bridge, and VXLAN interfaces.
// In containerlab environments, interfaces may initially appear with temporary
// names (clab-*) before being renamed to their final names. After container
// restarts, a scan can also briefly see only the management NIC before fabric
// links are moved into the namespace. This function retries briefly for both
// settling cases.
func DetectPhysicalNICs() ([]string, error) {
	return DetectPhysicalNICsContext(context.Background())
}

// DetectPhysicalNICsContext is DetectPhysicalNICs with caller cancellation.
func DetectPhysicalNICsContext(ctx context.Context) ([]string, error) {
	for i := range physicalNICMaxRetries {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("detect physical nics canceled: %w", err)
		}
		nics, hasTemp, err := detectNICsOnce()
		if err != nil {
			return nil, err
		}
		if !hasTemp && physicalNICsSettled(nics, i) {
			return nics, nil
		}
		if i == 0 {
			logNICSettleWait(nics, hasTemp)
		}
		if err := sleepPhysicalNICRetry(ctx); err != nil {
			return nil, err
		}
	}

	// Final attempt: return whatever we have, excluding any remaining clab-* names.
	nics, _, err := detectNICsOnce()
	if err != nil {
		return nil, err
	}
	var stable []string
	for _, n := range nics {
		if !strings.HasPrefix(n, "clab") {
			stable = append(stable, n)
		}
	}
	return stable, nil
}

func sleepPhysicalNICRetry(ctx context.Context) error {
	sleep := sleepFunc
	done := make(chan struct{})
	go func() {
		sleep(physicalNICRetryInterval)
		close(done)
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for physical nics to settle: %w", ctx.Err())
	case <-done:
		return nil
	}
}

const (
	physicalNICMaxRetries             = 20
	physicalNICRetryInterval          = 500 * time.Millisecond
	physicalNICSingleNICSettleRetries = 6
)

func physicalNICsSettled(nics []string, retry int) bool {
	if len(nics) == 1 {
		return retry >= physicalNICSingleNICSettleRetries
	}
	return true
}

func logNICSettleWait(nics []string, hasTemp bool) {
	if hasTemp {
		slog.Info("waiting for interface names to stabilize (clab-* detected)")
		return
	}
	slog.Info("waiting for physical interface set to stabilize", "nics", nics)
}

// detectNICsOnce performs a single scan of network interfaces and reports
// whether any temporary containerlab names (clab-*) were found.
func detectNICsOnce() (nics []string, hasTemp bool, err error) {
	links, err := linkList()
	if err != nil {
		return nil, false, fmt.Errorf("list links: %w", err)
	}

	for _, link := range links {
		name := link.Attrs().Name
		// A VRF device has a MAC and any name (for example Vrf_underlay
		// from netplan), but it is never a NIC.
		if isExcluded(name) || link.Type() == "vrf" {
			continue
		}
		if len(link.Attrs().HardwareAddr) == 0 {
			continue
		}
		if strings.HasPrefix(name, "clab") {
			hasTemp = true
			continue
		}
		nics = append(nics, name)
	}

	return nics, hasTemp, nil
}

// isExcluded checks if an interface name should be excluded.
func isExcluded(name string) bool {
	for _, prefix := range excludedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// bmcPrefixes are interface name prefixes that strongly suggest a BMC/IPMI NIC.
var bmcPrefixes = []string{"ipmi", "bmc", "idrac", "ilo", "imm"}

// mgmtPrefixes are interface name prefixes that suggest a management NIC.
var mgmtPrefixes = []string{"mgmt", "management"}

// ErrNoIPMIInterface means that no NIC is an IPMI NIC. This is expected on
// hosts where the BMC NIC is not visible to the host.
var ErrNoIPMIInterface = errors.New("no IPMI interface found")

// selectIPMIInterface picks the best interface for IPMI info from the provided list.
// It applies a three-pass ranked selection:
//  1. BMC-specific name prefixes (ipmi*, bmc*, idrac*, ilo*, imm*)
//  2. Management name prefixes (mgmt*, management*)
//  3. First interface with an IPv4 address inside ipmiNet, when ipmiNet is set
//
// Any other NIC is not an IPMI interface. Then no IPMI info is found, and the
// callers use the underlay IP rules like the legacy configure-frr.py script.
func selectIPMIInterface(ifaces []net.Interface, ipmiNet *net.IPNet) (*net.Interface, error) {
	return selectIPMIInterfaceWith(ifaces, ifaceAddrs, ipmiNet)
}

func selectIPMIInterfaceWith(ifaces []net.Interface, getAddrs func(net.Interface) ([]net.Addr, error), ipmiNet *net.IPNet) (*net.Interface, error) {
	candidates := filterAddressed(ifaces, getAddrs)

	for _, prefix := range bmcPrefixes {
		if iface := firstWithPrefix(candidates, prefix); iface != nil {
			return iface, nil
		}
	}

	for _, prefix := range mgmtPrefixes {
		if iface := firstWithPrefix(candidates, prefix); iface != nil {
			return iface, nil
		}
	}

	if ipmiNet != nil {
		for i := range candidates {
			addrs, err := getAddrs(candidates[i])
			if err == nil && ipv4In(addrs, ipmiNet) != nil {
				return &candidates[i], nil
			}
		}
	}

	return nil, ErrNoIPMIInterface
}

func filterAddressed(ifaces []net.Interface, getAddrs func(net.Interface) ([]net.Addr, error)) []net.Interface {
	var out []net.Interface
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(iface.HardwareAddr) == 0 {
			continue
		}
		addrs, err := getAddrs(iface)
		if err != nil {
			slog.Debug("skipping interface: address enumeration failed",
				"interface", iface.Name, "error", err)
			continue
		}
		if ipv4In(addrs, nil) != nil {
			out = append(out, iface)
		}
	}
	return out
}

func firstWithPrefix(ifaces []net.Interface, prefix string) *net.Interface {
	for i := range ifaces {
		if strings.HasPrefix(strings.ToLower(ifaces[i].Name), prefix) {
			return &ifaces[i]
		}
	}
	return nil
}

// ipv4In returns the first usable IPv4 address in addrs. When subnet is set,
// the address must be inside it.
func ipv4In(addrs []net.Addr, subnet *net.IPNet) net.IP {
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := ipNet.IP.To4()
		if v4 == nil || v4.IsLinkLocalUnicast() || v4.IsLoopback() {
			continue
		}
		if subnet == nil || subnet.Contains(v4) {
			return v4
		}
	}
	return nil
}

// GetIPMIInfo reads the IPMI MAC and IP from the system. ipmiSubnet is the
// optional IPMI network. It is used to find an IPMI NIC without a BMC name,
// and to prefer the address inside it.
func GetIPMIInfo(ipmiSubnet string) (mac, ip string, err error) {
	var ipmiNet *net.IPNet
	if ipmiSubnet != "" {
		if _, ipmiNet, err = net.ParseCIDR(ipmiSubnet); err != nil {
			return "", "", fmt.Errorf("parse IPMI subnet: %w", err)
		}
	}

	ifaces, err := listInterfaces()
	if err != nil {
		return "", "", fmt.Errorf("list interfaces: %w", err)
	}

	iface, err := selectIPMIInterface(ifaces, ipmiNet)
	if err != nil {
		return "", "", fmt.Errorf("select IPMI interface: %w", err)
	}

	addrs, err := ifaceAddrs(*iface)
	if err != nil {
		return "", "", fmt.Errorf("get addresses for %s: %w", iface.Name, err)
	}
	v4 := ipv4In(addrs, ipmiNet)
	if v4 == nil {
		v4 = ipv4In(addrs, nil)
	}
	if v4 == nil {
		return "", "", fmt.Errorf("no valid IPv4 address on interface %s", iface.Name)
	}
	return iface.HardwareAddr.String(), v4.String(), nil
}
