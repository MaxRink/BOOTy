//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/digineo/go-dhclient"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// DHCPMode implements the Mode interface with DHCPv4 on every physical
// interface, like systemd-networkd with the legacy ramdisk's netplan
// ("dhcp4: true" on every en* and eth* NIC): a client runs on every NIC for
// the whole run, every lease configures its NIC (address and a default route
// with the netplan metric), and renewals update it.
type DHCPMode struct {
	log *slog.Logger

	mu      sync.Mutex
	clients []dhcpClient
	nics    map[int]dhcpApplied // by link index: what the current lease configured
	stale   []dhcpApplied       // state that could not be removed; Teardown retries
	// stopped is set before the clients stop; a lease that arrives after it
	// is not applied.
	stopped bool
}

// dhcpApplied is the address, default route and NTP servers a lease
// configured.
type dhcpApplied struct {
	link  netlink.Link
	addr  *netlink.Addr
	route *netlink.Route
	ntp   []string
}

// dhcpClient is the part of a go-dhclient client that DHCPMode uses.
type dhcpClient interface {
	Start()
	Stop()
}

// Seams for Setup tests.
var (
	dhcpInterfaces  = physicalInterfaces
	dhcpLinkByName  = netlink.LinkByName
	dhcpLinkSetUp   = netlink.LinkSetUp
	dhcpPrepareLink = promoteSecondaries
	dhcpNewClient   = func(iface *net.Interface, hostname string, onBound dhclient.Callback) dhcpClient {
		return newDHCPClient(iface, hostname, onBound)
	}
)

// promoteSecondaries sets net.ipv4.conf.<nic>.promote_secondaries=1, like
// systemd's sysctl defaults (50-default.conf) in the legacy ramdisk. A
// renewal with a new address in the same subnet adds it as a secondary
// address; without promotion, removing the old primary would remove the
// new address too.
func promoteSecondaries(ifname string) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/conf/"+ifname+"/promote_secondaries", []byte("1"), 0o644); err != nil { //nolint:gosec // a sysctl
		return fmt.Errorf("set promote_secondaries on %s: %w", ifname, err)
	}
	return nil
}

// dhcpRouteMetric is the metric of DHCP default routes: netplan writes
// RouteMetric=100 for ethernets, so the routes of all NICs are equal.
const dhcpRouteMetric = 100

// dhcpSetupTimeout bounds the wait for the first lease in Setup.
var dhcpSetupTimeout = 15 * time.Second

// NewDHCPMode creates a DHCPMode with a component logger.
func NewDHCPMode() *DHCPMode {
	return &DHCPMode{
		log: slog.Default().With("component", "dhcp"),
	}
}

// Setup starts a DHCP client on every physical interface and returns when
// the first lease is applied. The clients keep running: later leases, also
// on other NICs, configure their NIC too. Without a lease within 15 s Setup
// fails, but the clients keep sending, like networkd retries forever: a
// switch port without portfast forwards only after 30 to 50 s, and the
// connectivity wait that follows gets the lease. Teardown stops them.
func (d *DHCPMode) Setup(ctx context.Context, _ *Config) error {
	if d.log == nil {
		d.log = slog.Default().With("component", "dhcp")
	}
	ifaces, err := dhcpInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("listing interfaces: %w", err)
	}
	if len(ifaces) == 0 {
		return fmt.Errorf("no physical interfaces found for DHCP")
	}

	d.log.Info("Starting DHCP on all interfaces", "count", len(ifaces))
	d.mu.Lock()
	d.stopped = false
	d.mu.Unlock()
	leased := make(chan string, len(ifaces))
	hostname, _ := os.Hostname()
	for i := range ifaces {
		d.startClient(&ifaces[i], hostname, leased)
	}

	timer := time.NewTimer(dhcpSetupTimeout)
	defer timer.Stop()
	select {
	case name := <-leased:
		d.log.Info("DHCP succeeded", "interface", name)
		return nil
	case <-timer.C:
		d.mu.Lock()
		leasedNow := len(d.nics) > 0 // a lease applied while the timer fired
		d.mu.Unlock()
		if leasedNow {
			d.log.Info("DHCP succeeded at the timeout")
			return nil
		}
		return fmt.Errorf("no DHCP lease on any of %d interfaces after %s, the clients keep trying", len(ifaces), dhcpSetupTimeout)
	case <-ctx.Done():
		d.stopClients()
		return fmt.Errorf("DHCP probing canceled: %w", ctx.Err())
	}
}

// startClient brings a NIC up and starts its DHCP client.
func (d *DHCPMode) startClient(iface *net.Interface, hostname string, leased chan<- string) {
	link, err := dhcpLinkByName(iface.Name)
	if err != nil {
		d.log.Warn("Cannot find link for DHCP", "interface", iface.Name, "error", err)
		return
	}
	if err := dhcpPrepareLink(iface.Name); err != nil {
		d.log.Warn("Cannot set promote_secondaries for DHCP", "interface", iface.Name, "error", err)
	}
	if err := dhcpLinkSetUp(link); err != nil {
		d.log.Warn("Cannot bring up link for DHCP", "interface", iface.Name, "error", err)
		return
	}
	client := dhcpNewClient(iface, hostname, d.onBound(link, iface.Name, leased))
	client.Start()
	d.mu.Lock()
	d.clients = append(d.clients, client)
	d.mu.Unlock()
}

// stopClients stops every DHCP client, all at once: go-dhclient's Stop
// waits up to its 5 s response timeout on a NIC without a server.
func (d *DHCPMode) stopClients() {
	d.mu.Lock()
	d.stopped = true
	clients := d.clients
	d.clients = nil
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Go(c.Stop)
	}
	wg.Wait()
}

// newDHCPClient builds a client that requests the default parameters
// (subnet mask, router, DNS, MTU and more) and sends the hostname.
// dhclient only adds the parameter request list when no option is set, so
// adding the hostname alone would drop it.
func newDHCPClient(iface *net.Interface, hostname string, onBound dhclient.Callback) *dhclient.Client {
	client := &dhclient.Client{Iface: iface, OnBound: onBound}
	for _, param := range dhclient.DefaultParamsRequestList {
		client.AddParamRequest(param)
	}
	if hostname != "" {
		client.AddOption(layers.DHCPOptHostname, []byte(hostname))
	}
	return client
}

// Netlink seams for onBound tests.
var (
	dhcpAddrReplace = replaceLeaseAddr
	dhcpAddrDel     = netlink.AddrDel
	dhcpRouteAdd    = netlink.RouteAppend
	dhcpRouteDel    = netlink.RouteDel
)

// replaceLeaseAddr sets addr on link. The kernel keys IPv4 addresses by
// address and prefix, so AddrReplace alone would keep the same IP with an
// older prefix next to the new one. Those entries are removed after the new
// address is in place, so a failure never leaves the link without the lease.
func replaceLeaseAddr(link netlink.Link, addr *netlink.Addr) error {
	if err := netlink.AddrReplace(link, addr); err != nil {
		return err //nolint:wrapcheck // callers add context
	}
	existing, err := RetryDump(func() ([]netlink.Addr, error) { return netlink.AddrList(link, netlink.FAMILY_V4) })
	if err != nil {
		slog.Warn("failed to list addresses for DHCP prefix cleanup", "iface", link.Attrs().Name, "error", err)
		return nil
	}
	for i := range existing {
		a := &existing[i]
		if a.IP.Equal(addr.IP) && a.IPNet.String() != addr.IPNet.String() {
			if err := netlink.AddrDel(link, a); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
				slog.Warn("failed to remove DHCP address with an old prefix", "iface", link.Attrs().Name, "addr", a.IPNet, "error", err)
			}
		}
	}
	return nil
}

// onBound returns the callback of the client on one NIC. Every lease, the
// first and each renewal or rebind, configures that NIC. A lease that cannot
// be applied leaves the previous state of the NIC in place.
func (d *DHCPMode) onBound(link netlink.Link, ifName string, leased chan<- string) func(*dhclient.Lease) {
	return func(lease *dhclient.Lease) {
		addr, err := leaseAddr(lease)
		if err != nil {
			d.log.Warn("ignoring DHCP lease", "iface", ifName, "error", err)
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.stopped {
			return
		}
		if err := d.apply(link, addr, lease); err != nil {
			d.log.Warn("failed to apply DHCP lease", "iface", ifName, "error", err)
			return
		}
		d.publishNTP()
		d.log.Info("DHCP lease applied", "iface", ifName, "addr", addr.IPNet.String())
		select {
		case leased <- ifName:
		default:
		}
	}
}

// apply configures the address and default route of a lease on its NIC and
// removes what the previous lease of the NIC set and this one does not. The
// caller holds d.mu.
func (d *DHCPMode) apply(link netlink.Link, addr *netlink.Addr, lease *dhclient.Lease) error {
	if d.nics == nil {
		d.nics = map[int]dhcpApplied{}
	}
	idx := link.Attrs().Index
	prev, had := d.nics[idx]
	// Replace, so an address left by an earlier lease gets this mask.
	if err := dhcpAddrReplace(link, addr); err != nil {
		return fmt.Errorf("assign DHCP address: %w", err)
	}
	route, err := d.applyRoute(link, addr, lease, prev.route)
	if err != nil {
		d.dropUnappliedAddr(link, addr, prev, had)
		return err
	}
	if had && !prev.addr.IP.Equal(addr.IP) {
		if err := dhcpAddrDel(link, prev.addr); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
			d.log.Warn("failed to remove superseded DHCP address", "iface", link.Attrs().Name, "error", err)
			d.stale = append(d.stale, dhcpApplied{link: link, addr: prev.addr})
		}
	}
	d.nics[idx] = dhcpApplied{link: link, addr: addr, route: route, ntp: leaseNTPServers(lease)}
	return nil
}

// applyRoute adds the default route of a lease (proto dhcp, the lease
// address as source, metric 100, like networkd) next to the default routes
// of other NICs, then removes the previous route of this NIC. A lease
// without a router option removes it (RFC 2131 4.4.5).
func (d *DHCPMode) applyRoute(link netlink.Link, addr *netlink.Addr, lease *dhclient.Lease, prev *netlink.Route) (*netlink.Route, error) {
	var route *netlink.Route
	if len(lease.Router) > 0 {
		route = &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Gw:        lease.Router[0],
			Src:       addr.IP,
			Priority:  dhcpRouteMetric,
			Protocol:  unix.RTPROT_DHCP,
		}
		// Append: a second NIC's default route must not replace the first.
		// The same route again (a renewal) is EEXIST.
		if err := dhcpRouteAdd(route); err != nil && !errors.Is(err, syscall.EEXIST) {
			return nil, fmt.Errorf("set default route via %s: %w", lease.Router[0], err)
		}
	}
	if prev != nil && (route == nil || !prev.Gw.Equal(route.Gw) || !prev.Src.Equal(route.Src)) {
		if err := dhcpRouteDel(prev); err != nil && !errors.Is(err, syscall.ESRCH) {
			d.log.Warn("failed to remove the old DHCP default route", "iface", link.Attrs().Name, "error", err)
			d.stale = append(d.stale, dhcpApplied{link: link, route: prev})
		}
	}
	return route, nil
}

// dropUnappliedAddr removes an address that was set before the route of its
// lease failed, unless it is the address of the previous lease; then the
// previous mask comes back if the lease changed it. The caller holds d.mu.
func (d *DHCPMode) dropUnappliedAddr(link netlink.Link, addr *netlink.Addr, prev dhcpApplied, had bool) {
	if had && prev.addr.IP.Equal(addr.IP) {
		if prev.addr.IPNet.String() != addr.IPNet.String() {
			if err := dhcpAddrReplace(link, prev.addr); err != nil {
				d.log.Warn("failed to restore DHCP address", "iface", link.Attrs().Name, "error", err)
				prev.addr = addr // track what the kernel has
				d.nics[link.Attrs().Index] = prev
			}
		}
		return
	}
	if err := dhcpAddrDel(link, addr); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
		d.log.Warn("failed to roll back DHCP address", "iface", link.Attrs().Name, "error", err)
		// Keep tracking it, so Teardown can still remove it.
		d.stale = append(d.stale, dhcpApplied{link: link, addr: addr})
	}
}

// leaseNTPServers returns the NTP servers (option 42) of a lease.
func leaseNTPServers(lease *dhclient.Lease) []string {
	var out []string
	for _, opt := range lease.OtherOptions {
		if opt.Type == layers.DHCPOptNTPServers {
			out = parseNTPServers(opt.Data)
		}
	}
	return out
}

// publishNTP publishes the NTP servers of all leases (by NIC index, each
// once) for the time sync, like networkd hands the servers of every link
// to timesyncd. The caller holds d.mu.
func (d *DHCPMode) publishNTP() {
	var servers []string
	for _, idx := range slices.Sorted(maps.Keys(d.nics)) {
		for _, s := range d.nics[idx].ntp {
			if !slices.Contains(servers, s) {
				servers = append(servers, s)
			}
		}
	}
	setDHCPNTPList(servers)
}

// leaseAddr builds the interface address from a lease. A lease without a
// subnet mask option gets the classful default mask, like other clients.
func leaseAddr(lease *dhclient.Lease) (*netlink.Addr, error) {
	ip := lease.FixedAddress.To4()
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("lease has no IPv4 address")
	}
	mask := lease.Netmask
	if len(mask) == 0 {
		mask = ip.DefaultMask()
	}
	if _, bits := mask.Size(); bits != 8*net.IPv4len {
		return nil, fmt.Errorf("lease has an invalid subnet mask %v", mask)
	}
	return &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: mask}}, nil
}

// WaitForConnectivity polls the target URL until reachable or timeout.
func (d *DHCPMode) WaitForConnectivity(ctx context.Context, target string, timeout time.Duration) error {
	return WaitForHTTP(ctx, target, timeout)
}

// Teardown stops the DHCP clients and removes the addresses and default
// routes that their leases configured, so a network retry starts clean.
func (d *DHCPMode) Teardown(_ context.Context) error {
	d.stopClients()
	d.mu.Lock()
	applied := d.stale
	for _, a := range d.nics {
		applied = append(applied, a)
	}
	d.nics, d.stale = nil, nil
	setDHCPNTPList(nil)
	d.mu.Unlock()
	var errs []error
	for _, a := range applied {
		if a.route != nil {
			if err := dhcpRouteDel(a.route); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("delete DHCP default route: %w", err))
			}
		}
		if a.addr == nil {
			continue
		}
		if err := dhcpAddrDel(a.link, a.addr); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
			errs = append(errs, fmt.Errorf("delete DHCP address: %w", err))
		}
	}
	return errors.Join(errs...)
}

// physicalInterfaces returns non-loopback, non-virtual interfaces.
func physicalInterfaces(ctx context.Context) ([]net.Interface, error) {
	nicNames, err := DetectPhysicalNICsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect physical nics: %w", err)
	}

	result := make([]net.Interface, 0, len(nicNames))
	for _, name := range nicNames {
		iface, ifErr := net.InterfaceByName(name)
		if ifErr != nil {
			slog.Debug("skipping NIC missing during DHCP scan", "interface", name, "error", ifErr)
			continue
		}
		result = append(result, *iface)
	}
	return result, nil
}
