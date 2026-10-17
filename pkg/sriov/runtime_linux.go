//go:build linux

package sriov

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// KernelRuntime is the RuntimeOps of the running kernel: sysfs for
// sriov_numvfs, devlink for the eSwitch mode and rtnetlink for the VF
// attributes. These interfaces are the same for every SR-IOV driver. It
// finds a device by bus and PCI address, never by the interface name,
// which a switchdev change can replace when no udev renames it back.
type KernelRuntime struct {
	// SysBus is the /sys/bus directory.
	SysBus string
	// Sockets lists the TCP sockets of the ramdisk; tests replace it.
	Sockets func() ([]*netlink.Socket, error)
	// Uplinks returns the interfaces of the ramdisk network from its
	// config (BGP sessions, BGP interfaces, static interface, bond members,
	// VLAN parents) and whether BGP runs on every physical NIC; known is
	// false before the network setup recorded them.
	Uplinks func() (names []string, allPhysical, known bool)
}

// NewKernelRuntime returns the RuntimeOps of the running kernel.
func NewKernelRuntime() *KernelRuntime {
	return &KernelRuntime{SysBus: "/sys/bus", Sockets: tcpSockets}
}

func (k *KernelRuntime) deviceDir(pf *PF) string {
	return filepath.Join(k.SysBus, pf.Bus, "devices", pf.PCIAddress)
}

// SetNumVFs implements RuntimeOps.
func (k *KernelRuntime) SetNumVFs(pf *PF, n int) error {
	path := filepath.Join(k.deviceDir(pf), "sriov_numvfs")
	if err := os.WriteFile(path, []byte(strconv.Itoa(n)), 0o200); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// netdev returns the current netdev of the PF port: the netdev of the
// device with the same phys_port_name, else the same MAC, else the only
// one that is not a representor.
func (k *KernelRuntime) netdev(pf *PF) (string, error) {
	dir := filepath.Join(k.deviceDir(pf), "net")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("netdevs of %s/%s: %w", pf.Bus, pf.PCIAddress, err)
	}
	var ports []string
	for _, e := range entries {
		port := readTrim(filepath.Join(dir, e.Name(), "phys_port_name"))
		switch {
		case pf.PhysPortName != "" && port == pf.PhysPortName:
			return e.Name(), nil
		case pf.PhysPortName == "" && pf.MAC != "" && readTrim(filepath.Join(dir, e.Name(), "address")) == pf.MAC:
			return e.Name(), nil
		case !representorName.MatchString(port):
			ports = append(ports, e.Name())
		}
	}
	if len(ports) == 1 && pf.PhysPortName == "" {
		return ports[0], nil
	}
	return "", fmt.Errorf("no netdev of %s/%s for port %q", pf.Bus, pf.PCIAddress, pf.PhysPortName+pf.MAC)
}

// SetEswitch implements RuntimeOps.
func (k *KernelRuntime) SetEswitch(pf *PF, mode string) error {
	var m uint16
	switch mode {
	case EswitchLegacy:
		m = nl.DEVLINK_ESWITCH_MODE_LEGACY
	case EswitchSwitchdev:
		m = nl.DEVLINK_ESWITCH_MODE_SWITCHDEV
	default:
		return fmt.Errorf("unknown eSwitch mode %q", mode)
	}
	_, err := devlinkRequest(nl.DEVLINK_CMD_ESWITCH_SET, pf.Bus, pf.PCIAddress,
		nl.NewRtAttr(nl.DEVLINK_ATTR_ESWITCH_MODE, nl.Uint16Attr(m)))
	return err
}

// SetVF implements RuntimeOps.
func (k *KernelRuntime) SetVF(pf *PF, index int, s *VFSettings, mac string) error {
	name, err := k.netdev(pf)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("link %s: %w", name, err)
	}
	for _, step := range vfSteps(s, mac) {
		if err := step.set(link, index); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

type vfStep struct {
	name string
	set  func(link netlink.Link, vf int) error
}

// vfSteps lists the rtnetlink calls for the set attributes, in the order
// of "ip link set <pf> vf N": MAC, VLAN and QoS, rate, spoof check, link
// state, trust.
func vfSteps(s *VFSettings, mac string) []vfStep {
	var steps []vfStep
	if mac != "" {
		steps = append(steps, vfStep{"mac", func(l netlink.Link, vf int) error {
			hw, err := net.ParseMAC(mac)
			if err != nil {
				return fmt.Errorf("parse MAC: %w", err)
			}
			return wrapNL(netlink.LinkSetVfHardwareAddr(l, vf, hw))
		}})
	}
	if s.VLAN != nil || s.QoS != nil {
		vlan, qos := deref(s.VLAN), deref(s.QoS)
		steps = append(steps, vfStep{"vlan", func(l netlink.Link, vf int) error {
			return wrapNL(netlink.LinkSetVfVlanQos(l, vf, vlan, qos))
		}})
	}
	if s.MinTxRate != nil || s.MaxTxRate != nil {
		minRate, maxRate := deref(s.MinTxRate), deref(s.MaxTxRate)
		steps = append(steps, vfStep{"rate", func(l netlink.Link, vf int) error {
			return wrapNL(netlink.LinkSetVfRate(l, vf, minRate, maxRate))
		}})
	}
	if s.SpoofChk != nil {
		on := *s.SpoofChk
		steps = append(steps, vfStep{"spoofchk", func(l netlink.Link, vf int) error {
			return wrapNL(netlink.LinkSetVfSpoofchk(l, vf, on))
		}})
	}
	if s.LinkState != "" {
		state := linkStateValue(s.LinkState)
		steps = append(steps, vfStep{"state", func(l netlink.Link, vf int) error {
			return wrapNL(netlink.LinkSetVfState(l, vf, state))
		}})
	}
	if s.Trust != nil {
		on := *s.Trust
		steps = append(steps, vfStep{"trust", func(l netlink.Link, vf int) error {
			return wrapNL(netlink.LinkSetVfTrust(l, vf, on))
		}})
	}
	return steps
}

func wrapNL(err error) error {
	if err != nil {
		return fmt.Errorf("rtnetlink: %w", err)
	}
	return nil
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func linkStateValue(s string) uint32 {
	switch s {
	case "enable":
		return netlink.VF_LINK_STATE_ENABLE
	case "disable":
		return netlink.VF_LINK_STATE_DISABLE
	}
	return netlink.VF_LINK_STATE_AUTO
}

// InUse implements RuntimeOps. BOOTy decides from its own network config
// first: an interface that carries its BGP sessions (also IPv6 link-local
// unnumbered ones), its static address, bond or VLAN is in use, and so is
// every port when BGP runs on all physical NICs. The kernel state adds a
// master device, a VLAN on top, an address other than IPv6 link-local, a
// TCP socket bound to the port (sock_diag, when inet_diag and tcp_diag are
// there) and a unicast route through it. BOOTy never probes peers: a new
// connection to a BGP peer can reset its live session.
func (k *KernelRuntime) InUse(pf *PF) (inUse bool, reason string) {
	name, err := k.netdev(pf)
	if err != nil {
		return true, fmt.Sprintf("cannot find the netdev: %v", err)
	}
	if used, reason := k.configured(name, pf.Interface); used {
		return true, reason
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return true, fmt.Sprintf("cannot read the link: %v", err)
	}
	if used, reason := linkUsed(link); used {
		return true, reason
	}
	idx := link.Attrs().Index
	if used, reason := k.socketOn(idx, name); used {
		return true, reason
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return true, fmt.Sprintf("cannot read the routes: %v", err)
	}
	for i := range routes {
		if routeUses(&routes[i], idx) && forwardingRoute(&routes[i]) {
			return true, "a route goes through the port"
		}
	}
	return false, ""
}

// Master implements RuntimeOps.
func (k *KernelRuntime) Master(pf *PF) string {
	name, err := k.netdev(pf)
	if err != nil {
		return ""
	}
	link, err := netlink.LinkByName(name)
	if err != nil || link.Attrs().MasterIndex == 0 {
		return ""
	}
	master, err := netlink.LinkByIndex(link.Attrs().MasterIndex)
	if err != nil {
		return fmt.Sprintf("ifindex %d", link.Attrs().MasterIndex)
	}
	return master.Attrs().Name
}

// configured reports a port that the ramdisk network config names.
func (k *KernelRuntime) configured(names ...string) (used bool, reason string) {
	if k.Uplinks == nil {
		return false, ""
	}
	uplinks, allPhysical, known := k.Uplinks()
	if !known {
		return false, ""
	}
	if allPhysical {
		return true, "the ramdisk BGP runs on every physical NIC (no BGP interface or session is named)"
	}
	for _, n := range names {
		if n != "" && slices.Contains(uplinks, n) {
			return true, "the ramdisk network config uses " + n + " (BGP session, BGP interface, static address, bond or VLAN)"
		}
	}
	return false, ""
}

// linkUsed reports a master device, a VLAN or another link on top, or an
// address other than IPv6 link-local.
func linkUsed(link netlink.Link) (used bool, reason string) {
	if link.Attrs().MasterIndex != 0 {
		return true, "the port has a master device (bond, bridge, VRF)"
	}
	links, err := netlink.LinkList()
	if err != nil {
		return true, fmt.Sprintf("cannot list the links: %v", err)
	}
	for _, l := range links {
		if l.Attrs().ParentIndex == link.Attrs().Index && l.Attrs().Index != link.Attrs().Index {
			return true, "link " + l.Attrs().Name + " sits on the port"
		}
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return true, fmt.Sprintf("cannot read the addresses: %v", err)
	}
	for i := range addrs {
		if !addrs[i].IP.IsLinkLocalUnicast() {
			return true, "the port has address " + addrs[i].IPNet.String()
		}
	}
	return false, ""
}

// socketOn reports a TCP socket bound to the interface index, like a BGP
// session over IPv6 link-local. Without sock_diag (inet_diag and tcp_diag
// not loaded) this signal is missing; the network config still covers the
// sessions of BOOTy.
func (k *KernelRuntime) socketOn(idx int, name string) (used bool, reason string) {
	if k.Sockets == nil {
		return false, ""
	}
	socks, err := k.Sockets()
	if err != nil {
		slog.Info("sock_diag unavailable, the SR-IOV in-use check uses the network config and the routes", "interface", name, "error", err)
		return false, ""
	}
	for _, s := range socks {
		if s.ID.Interface == uint32(idx) { //nolint:gosec // interface indexes are positive
			return true, fmt.Sprintf("TCP session %s:%d to %s:%d is bound to the port (BGP over IPv6 link-local)",
				s.ID.Source, s.ID.SourcePort, s.ID.Destination, s.ID.DestinationPort)
		}
	}
	return false, ""
}

// tcpSockets lists the IPv4 and IPv6 TCP sockets through sock_diag.
func tcpSockets() ([]*netlink.Socket, error) {
	var all []*netlink.Socket
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		socks, err := netlink.SocketDiagTCP(family)
		if err != nil {
			return nil, fmt.Errorf("sock_diag TCP: %w", err)
		}
		all = append(all, socks...)
	}
	return all, nil
}

// forwardingRoute reports a unicast route to a destination other than
// IPv6 link-local; the kernel adds link-local and multicast routes to every
// port that is up.
func forwardingRoute(r *netlink.Route) bool {
	if r.Type != unix.RTN_UNICAST {
		return false
	}
	return r.Dst == nil || (!r.Dst.IP.IsLinkLocalUnicast() && !r.Dst.IP.IsMulticast())
}

func routeUses(r *netlink.Route, idx int) bool {
	if r.LinkIndex == idx {
		return true
	}
	for _, nh := range r.MultiPath {
		if nh.LinkIndex == idx {
			return true
		}
	}
	return false
}
