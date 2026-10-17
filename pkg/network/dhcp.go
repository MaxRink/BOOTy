//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/digineo/go-dhclient"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
)

// DHCPMode implements the Mode interface using DHCP on all physical interfaces.
type DHCPMode struct {
	client *dhclient.Client
	log    *slog.Logger

	mu      sync.Mutex
	applied []dhcpApplied // what leases configured; Teardown removes it
}

// dhcpApplied is the address and default route a lease configured.
type dhcpApplied struct {
	link  netlink.Link
	addr  *netlink.Addr
	route *netlink.Route
}

// NewDHCPMode creates a DHCPMode with a component logger.
func NewDHCPMode() *DHCPMode {
	return &DHCPMode{
		log: slog.Default().With("component", "dhcp"),
	}
}

// dhcpResult carries the outcome of a single NIC probe.
type dhcpResult struct {
	client *dhclient.Client
	iface  string
}

// Setup tries DHCP on all physical interfaces in parallel.
// Each NIC gets its own 15 s timeout; the first lease wins.
func (d *DHCPMode) Setup(ctx context.Context, _ *Config) error {
	if d.log == nil {
		d.log = slog.Default().With("component", "dhcp")
	}
	ifaces, err := physicalInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("listing interfaces: %w", err)
	}
	if len(ifaces) == 0 {
		return fmt.Errorf("no physical interfaces found for DHCP")
	}

	d.log.Info("Probing DHCP on all interfaces in parallel", "count", len(ifaces))

	probeCtx, probeCancel := context.WithCancel(ctx)
	defer probeCancel()

	results := make(chan dhcpResult, len(ifaces))
	var wg sync.WaitGroup
	var winner dhcpWinner

	for i := range ifaces {
		wg.Add(1)
		go func(iface net.Interface) {
			defer wg.Done()
			d.probeNIC(probeCtx, iface, results, &winner)
		}(ifaces[i])
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for res := range results {
		d.client = res.client
		d.log.Info("DHCP succeeded", "interface", res.iface)
		probeCancel()
		break
	}

	if d.client == nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("DHCP probing canceled: %w", err)
		}
		return fmt.Errorf("DHCP failed on all %d interfaces", len(ifaces))
	}
	return nil
}

// probeNIC attempts DHCP on a single interface with a 15 s timeout.
// On success it sends a dhcpResult to the results channel.
func (d *DHCPMode) probeNIC(ctx context.Context, iface net.Interface, results chan<- dhcpResult, winner *dhcpWinner) {
	d.log.Info("Attempting DHCP", "interface", iface.Name)

	link, err := netlink.LinkByName(iface.Name)
	if err != nil {
		d.log.Warn("Cannot find link for DHCP", "interface", iface.Name, "error", err)
		return
	}
	if err := netlink.LinkSetUp(link); err != nil {
		d.log.Warn("Cannot bring up link for DHCP", "interface", iface.Name, "error", err)
		return
	}

	leased := make(chan struct{}, 1)
	hostname, _ := os.Hostname()
	client := newDHCPClient(&iface, hostname, d.onBound(link, iface.Name, leased, winner))
	client.Start()

	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	select {
	case <-leased:
		results <- dhcpResult{client: client, iface: iface.Name}
	case <-timer.C:
		client.Stop()
		d.log.Info("DHCP timeout on interface", "interface", iface.Name)
	case <-ctx.Done():
		client.Stop()
		d.log.Info("DHCP probe canceled", "interface", iface.Name, "error", ctx.Err())
	}
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
	dhcpAddrReplace  = replaceLeaseAddr
	dhcpAddrDel      = netlink.AddrDel
	dhcpRouteReplace = netlink.RouteReplace
	dhcpRouteDel     = netlink.RouteDel
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

// onBound returns a callback that configures the interface address and default route.
// The winner ensures only the first NIC whose lease applies cleanly configures the host;
// later leases are ignored. Callbacks wait for an apply in progress, so a lease on another
// NIC is still used when the first apply fails.
func (d *DHCPMode) onBound(link netlink.Link, ifName string, leased chan<- struct{}, winner *dhcpWinner) func(*dhclient.Lease) {
	return func(lease *dhclient.Lease) {
		// Validate before claiming the winner, so an unusable lease on one
		// NIC does not block a usable lease on another NIC.
		addr, err := leaseAddr(lease)
		if err != nil {
			d.log.Warn("ignoring DHCP lease", "iface", ifName, "error", err)
			return
		}
		winner.mu.Lock()
		defer winner.mu.Unlock()
		if winner.won {
			// Renewals and rebinds of the winning client update the host;
			// leases on other NICs are ignored.
			if winner.iface == ifName {
				d.applyRenewal(link, ifName, addr, lease)
			}
			return
		}
		// Replace, so an address left by an earlier lease gets this mask.
		if err := dhcpAddrReplace(link, addr); err != nil {
			d.log.Warn("failed to assign DHCP address", "iface", ifName, "error", err)
			return
		}
		var route *netlink.Route
		if len(lease.Router) > 0 {
			// Replace, so a default route left by an earlier attempt or
			// another NIC does not stay in place.
			route = &netlink.Route{Gw: lease.Router[0], LinkIndex: link.Attrs().Index}
			if err := dhcpRouteReplace(route); err != nil {
				d.log.Warn("failed to set default route", "iface", ifName, "gw", lease.Router[0], "error", err)
				// Roll the address back so another NIC can win cleanly.
				if delErr := dhcpAddrDel(link, addr); delErr != nil {
					d.log.Warn("failed to remove DHCP address after route failure", "iface", ifName, "error", delErr)
					d.remember(dhcpApplied{link: link, addr: addr})
				}
				return
			}
		}
		winner.won, winner.iface = true, ifName
		d.remember(dhcpApplied{link: link, addr: addr, route: route})
		d.log.Info("DHCP lease obtained", "iface", ifName, "addr", addr.IPNet.String())
		select {
		case leased <- struct{}{}:
		default:
		}
	}
}

// dhcpWinner lets only the first cleanly applied lease configure the host.
type dhcpWinner struct {
	mu    sync.Mutex
	won   bool
	iface string
}

// dropUnappliedAddr removes a renewed address that was set before a later
// step failed, unless it is the address already applied on the link. The
// caller holds d.mu.
func (d *DHCPMode) dropUnappliedAddr(link netlink.Link, addr *netlink.Addr) {
	for i, a := range d.applied {
		if a.link.Attrs().Index == link.Attrs().Index && a.addr.IP.Equal(addr.IP) {
			// Same IP: put the previous mask back if the renewal changed it.
			if a.addr.IPNet.String() != addr.IPNet.String() {
				if err := dhcpAddrReplace(link, a.addr); err != nil {
					d.log.Warn("failed to restore DHCP address", "iface", link.Attrs().Name, "error", err)
					d.applied[i].addr = addr // track what the kernel has
				}
			}
			return
		}
	}
	if err := dhcpAddrDel(link, addr); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
		d.log.Warn("failed to roll back renewed DHCP address", "iface", link.Attrs().Name, "error", err)
		// Keep tracking it, so Teardown can still remove it.
		d.applied = append(d.applied, dhcpApplied{link: link, addr: addr})
	}
}

// applyRenewal applies a renewed or rebound lease of the winning NIC. A
// changed address replaces the old one, and the default route follows the
// lease. Failures are logged and the previous state stays.
func (d *DHCPMode) applyRenewal(link netlink.Link, ifName string, addr *netlink.Addr, lease *dhclient.Lease) {
	if err := dhcpAddrReplace(link, addr); err != nil {
		d.log.Warn("failed to apply renewed DHCP address", "iface", ifName, "error", err)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var route *netlink.Route
	if len(lease.Router) > 0 {
		route = &netlink.Route{Gw: lease.Router[0], LinkIndex: link.Attrs().Index}
		if err := dhcpRouteReplace(route); err != nil {
			d.log.Warn("failed to apply renewed default route", "iface", ifName, "gw", lease.Router[0], "error", err)
			d.dropUnappliedAddr(link, addr)
			return
		}
	}
	kept := make([]dhcpApplied, 0, len(d.applied)+1)
	for _, a := range d.applied {
		// RFC 2131 4.4.5: a renewal without a router option drops the old
		// default route. If that fails, keep tracking it for Teardown.
		if route == nil && a.route != nil && a.link.Attrs().Index == link.Attrs().Index {
			if err := dhcpRouteDel(a.route); err != nil && !errors.Is(err, syscall.ESRCH) {
				d.log.Warn("failed to remove the old DHCP default route", "iface", ifName, "error", err)
				route = a.route
			}
		}
		if a.link.Attrs().Index == link.Attrs().Index && !a.addr.IP.Equal(addr.IP) {
			if err := dhcpAddrDel(a.link, a.addr); err != nil {
				d.log.Warn("failed to remove superseded DHCP address", "iface", ifName, "error", err)
				kept = append(kept, a)
			}
			continue
		}
		if a.link.Attrs().Index != link.Attrs().Index {
			kept = append(kept, a)
		}
	}
	kept = append(kept, dhcpApplied{link: link, addr: addr, route: route})
	d.applied = kept
	d.log.Info("DHCP lease renewed", "iface", ifName, "addr", addr.IPNet.String())
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

// Teardown stops the DHCP client and removes the address and default route
// that its lease configured, so a network retry starts clean.
func (d *DHCPMode) Teardown(_ context.Context) error {
	if d.client != nil {
		d.client.Stop()
	}
	d.mu.Lock()
	applied := d.applied
	d.applied = nil
	d.mu.Unlock()
	var errs []error
	for _, a := range applied {
		if a.route != nil {
			if err := dhcpRouteDel(a.route); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("delete DHCP default route: %w", err))
			}
		}
		if err := dhcpAddrDel(a.link, a.addr); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
			errs = append(errs, fmt.Errorf("delete DHCP address: %w", err))
		}
	}
	return errors.Join(errs...)
}

// remember records state for Teardown to remove.
func (d *DHCPMode) remember(a dhcpApplied) {
	d.mu.Lock()
	d.applied = append(d.applied, a)
	d.mu.Unlock()
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
