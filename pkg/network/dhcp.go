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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/digineo/go-dhclient"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// DHCPMode implements the Mode interface with DHCPv4 on every physical
// interface, like systemd-networkd with the legacy ramdisk's netplan
// ("dhcp4: true" on every en* and eth* NIC): a client runs on every NIC for
// the whole run, every lease configures its NIC (address, a default route
// with the netplan metric, the MTU), and renewals update it. The DNS servers
// of all leases go to resolv.conf after the configured ones, like
// systemd-resolved with UseDNS=yes.
type DHCPMode struct {
	log *slog.Logger

	mu      sync.Mutex
	clients map[int]*nicClient  // by link index
	nics    map[int]dhcpApplied // by link index: what the current lease configured
	stale   []dhcpApplied       // state that could not be removed; Teardown retries
	// stopped is set before the clients stop; a lease that arrives after it
	// is not applied.
	stopped bool
	// expiry ends the lease of a NIC at its expiry time. go-dhclient v1.0.2
	// does not: after a failed renewal it renews again forever and never
	// reaches the expiry, so OnExpire only comes for a DHCPNAK.
	expiry map[int]leaseTimer
	// restarts are client restarts in progress; Teardown waits for them.
	restarts sync.WaitGroup
	// retry restarts the client of a NIC whose lease was refused, after
	// retryWait (doubled per refusal up to 64 s, reset by a lease).
	retry     map[int]*time.Timer
	retryWait map[int]time.Duration
	mtus      map[int]dhcpMTU // by link index: MTU before the first lease changed it

	manageDNS bool     // false: the provisioner resolv.conf stays
	resolvers []string // configured DNS servers, written before the lease ones
	written   string   // the resolv.conf content last written
}

// nicClient is the running DHCP client of one NIC and how to start a fresh
// one for it.
type nicClient struct {
	client dhcpClient
	start  func() dhcpClient
}

// dhcpStart starts a client; tests replace it.
var dhcpStart = func(c dhcpClient) dhcpClient {
	c.Start()
	return c
}

// leaseTimer is the expiry timer of the lease of one NIC.
type leaseTimer struct {
	timer *time.Timer
	at    time.Time // the expiry time it was armed for
}

// dhcpAfterFunc is time.AfterFunc; tests replace it.
var dhcpAfterFunc = time.AfterFunc

// dhcpApplied is what a lease configured: address, default route, DNS
// and NTP servers.
type dhcpApplied struct {
	link  netlink.Link
	addr  *netlink.Addr
	route *netlink.Route
	dns   []string
	ntp   []string
	// domain is the lease domain (option 15), a search domain like
	// networkd with netplan (the legacy resolv.conf has it as "search").
	domain string
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
	dhcpNewClient   = func(iface *net.Interface, hostname string, onBound, onExpire dhclient.Callback) dhcpClient {
		return newDHCPClient(iface, hostname, onBound, onExpire)
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

// dhcpMTU is the MTU a NIC had before the first lease changed it, and the
// MTU the leases set.
type dhcpMTU struct {
	link      netlink.Link
	orig, set int
}

// dhcpMinMTU is the smallest MTU that a lease sets, the minimum DHCP packet
// size (RFC 2131: 576); with IPv6 on the NIC it is 1280 (RFC 8200), like
// systemd-networkd clamps the lease MTU.
const (
	dhcpMinMTU     = 576
	dhcpMinMTUIPv6 = 1280
)

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
func (d *DHCPMode) Setup(ctx context.Context, cfg *Config) error {
	if d.log == nil {
		d.log = slog.Default().With("component", "dhcp")
	}
	if err := d.setupDNS(cfg); err != nil {
		return err
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
	start := func() dhcpClient { return dhcpStart(d.clientFor(iface, link, hostname, leased)) }
	nc := &nicClient{client: start(), start: start}
	d.mu.Lock()
	if d.clients == nil {
		d.clients = map[int]*nicClient{}
	}
	d.clients[link.Attrs().Index] = nc
	d.mu.Unlock()
}

// stopClients stops every DHCP client, all at once: go-dhclient's Stop
// waits up to its 5 s response timeout on a NIC without a server. A client
// is stopped once, by whoever takes it out of d.clients (go-dhclient panics
// on a second Stop).
func (d *DHCPMode) stopClients() {
	d.mu.Lock()
	d.stopped = true
	clients := d.clients
	d.clients = nil
	for idx, t := range d.retry {
		t.Stop()
		delete(d.retry, idx)
	}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Go(c.client.Stop)
	}
	wg.Wait()
}

// takeForRestart takes the client of a NIC out of d.clients and restarts
// it; Teardown waits for the restart. The caller holds d.mu and does not
// after the call.
func (d *DHCPMode) takeForRestart(idx int, ifName string) {
	nc := d.clients[idx]
	delete(d.clients, idx)
	if nc == nil || d.stopped {
		d.mu.Unlock()
		return
	}
	d.restarts.Add(1)
	d.mu.Unlock()
	defer d.restarts.Done()
	d.restartClient(idx, ifName, nc)
}

// retryAfterRefusal restarts the client of a NIC whose lease was refused
// (no lifetime, no server identifier): go-dhclient stays bound to it and
// would send REQUESTs without end. Like networkd, which ignores such an
// ACK and tries again, the restart comes after a backoff of 2 s, doubled
// per refusal up to 64 s.
func (d *DHCPMode) retryAfterRefusal(idx int, ifName string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.retry[idx] != nil {
		return
	}
	wait := min(max(2*d.retryWait[idx], 2*time.Second), 64*time.Second)
	if d.retryWait == nil {
		d.retryWait, d.retry = map[int]time.Duration{}, map[int]*time.Timer{}
	}
	d.retryWait[idx] = wait
	d.retry[idx] = dhcpAfterFunc(wait, func() {
		d.mu.Lock()
		delete(d.retry, idx)
		d.takeForRestart(idx, ifName)
	})
}

// restartClient replaces the client of a NIC whose lease ended with a fresh
// one, which starts in INIT with a DHCPDISCOVER (RFC 2131 section 4.4.5),
// like networkd's client_initialize and client_start. go-dhclient keeps the
// old lease after failed renewals and would only send DHCPREQUESTs for it
// to the old server. The caller took nc out of d.clients and does not hold
// d.mu: Stop waits for the client, whose OnBound may wait for d.mu.
func (d *DHCPMode) restartClient(idx int, ifName string, nc *nicClient) {
	nc.client.Stop()
	d.mu.Lock()
	stopped := d.stopped
	d.mu.Unlock()
	if stopped {
		return
	}
	fresh := nc.start()
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		fresh.Stop()
		return
	}
	if d.clients == nil {
		d.clients = map[int]*nicClient{}
	}
	d.clients[idx] = &nicClient{client: fresh, start: nc.start}
	d.mu.Unlock()
	d.log.Info("DHCP client restarted after the lease ended", "iface", ifName)
}

// clientFor builds the DHCP client of one NIC with its lease callbacks.
func (d *DHCPMode) clientFor(iface *net.Interface, link netlink.Link, hostname string, leased chan<- string) dhcpClient {
	return dhcpNewClient(iface, hostname, d.onBound(link, iface.Name, leased), d.onExpire(link, iface.Name))
}

// newDHCPClient builds a client that requests the default parameters
// (subnet mask, router, DNS, MTU and more) and sends the hostname.
// dhclient only adds the parameter request list when no option is set, so
// adding the hostname alone would drop it.
func newDHCPClient(iface *net.Interface, hostname string, onBound, onExpire dhclient.Callback) *dhclient.Client {
	client := &dhclient.Client{Iface: iface, OnBound: onBound, OnExpire: onExpire}
	for _, param := range dhclient.DefaultParamsRequestList {
		client.AddParamRequest(param)
	}
	if hostname != "" {
		client.AddOption(layers.DHCPOptHostname, []byte(hostname))
	}
	return client
}

// Netlink and resolv.conf seams for onBound tests.
var (
	dhcpAddrReplace    = replaceLeaseAddr
	dhcpAddrDel        = netlink.AddrDel
	dhcpRouteAdd       = netlink.RouteAppend
	dhcpRouteDel       = netlink.RouteDel
	dhcpSetMTU         = netlink.LinkSetMTU
	dhcpLinkMaxMTU     = linkMaxMTU
	dhcpIPv6Enabled    = ipv6Enabled
	dhcpWriteResolvers = ConfigureResolvConf
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
		if err == nil {
			err = leaseUsable(lease)
		}
		if err != nil {
			d.log.Warn("ignoring DHCP lease", "iface", ifName, "error", err)
			d.retryAfterRefusal(link.Attrs().Index, ifName)
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.stopped {
			return
		}
		delete(d.retryWait, link.Attrs().Index)
		if err := d.apply(link, addr, lease); err != nil {
			d.log.Warn("failed to apply DHCP lease", "iface", ifName, "error", err)
			return
		}
		d.armExpiry(link, ifName, lease.Expire)
		d.publishNTP()
		d.log.Info("DHCP lease applied", "iface", ifName, "addr", addr.IPNet.String())
		select {
		case leased <- ifName:
		default:
		}
	}
}

// onExpire returns the callback for the end of a lease of one NIC that
// go-dhclient reports: a DHCPNAK, or an expiry it notices. RFC 2131
// sections 4.4.5 and 3.2 say the client then stops using the address and
// starts over in INIT, so the address and default route of the lease go.
// The client keeps running; its next lease is applied like any other.
func (d *DHCPMode) onExpire(link netlink.Link, ifName string) func(*dhclient.Lease) {
	return func(*dhclient.Lease) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.endLease(link, ifName)
	}
}

// armExpiry (re)starts the expiry timer of the lease of a NIC; each bound
// lease, also a renewal, moves it. A zero expiry disarms it. The caller
// holds d.mu.
func (d *DHCPMode) armExpiry(link netlink.Link, ifName string, at time.Time) {
	idx := link.Attrs().Index
	if old, ok := d.expiry[idx]; ok {
		old.timer.Stop()
		delete(d.expiry, idx)
	}
	if at.IsZero() {
		return
	}
	if d.expiry == nil {
		d.expiry = map[int]leaseTimer{}
	}
	d.expiry[idx] = leaseTimer{at: at, timer: dhcpAfterFunc(time.Until(at), func() {
		d.mu.Lock()
		// A renewal may have moved the expiry while this timer fired.
		if cur, ok := d.expiry[idx]; !ok || !cur.at.Equal(at) {
			d.mu.Unlock()
			return
		}
		d.log.Warn("DHCP lease expired without renewal", "iface", ifName)
		d.endLease(link, ifName)
		d.takeForRestart(idx, ifName)
	})}
}

// endLease removes the address and default route of the lease of a NIC
// and its expiry timer. State that cannot be removed stays tracked for
// Teardown. The caller holds d.mu.
func (d *DHCPMode) endLease(link netlink.Link, ifName string) {
	idx := link.Attrs().Index
	if t, ok := d.expiry[idx]; ok {
		t.timer.Stop()
		delete(d.expiry, idx)
	}
	a, ok := d.nics[idx]
	if !ok {
		return
	}
	delete(d.nics, idx)
	// Its NTP and DNS servers and its MTU go with the lease.
	d.publishNTP()
	d.writeResolvers()
	if m, ok := d.mtus[idx]; ok {
		if m.set == m.orig {
			delete(d.mtus, idx)
		} else if err := dhcpSetMTU(link, m.orig); err != nil {
			// Kept, so Teardown tries again.
			d.log.Warn("failed to restore the MTU of an ended DHCP lease", "iface", ifName, "error", err)
		} else {
			delete(d.mtus, idx)
		}
	}
	removed := true
	if a.route != nil {
		if err := dhcpRouteDel(a.route); err != nil && !errors.Is(err, syscall.ESRCH) {
			d.log.Warn("failed to remove the default route of an ended DHCP lease", "iface", ifName, "error", err)
			d.stale = append(d.stale, dhcpApplied{link: link, route: a.route})
			removed = false
		}
	}
	if err := dhcpAddrDel(link, a.addr); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
		d.log.Warn("failed to remove the address of an ended DHCP lease", "iface", ifName, "error", err)
		d.stale = append(d.stale, dhcpApplied{link: link, addr: a.addr})
		removed = false
	}
	if removed {
		d.log.Warn("DHCP lease ended (expired or refused), address removed until a new lease", "iface", ifName,
			"addr", a.addr.IPNet.String())
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
		d.dropUnappliedAddr(link, addr, &prev, had)
		return err
	}
	if had && !prev.addr.IP.Equal(addr.IP) {
		if err := dhcpAddrDel(link, prev.addr); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
			d.log.Warn("failed to remove superseded DHCP address", "iface", link.Attrs().Name, "error", err)
			d.stale = append(d.stale, dhcpApplied{link: link, addr: prev.addr})
		}
	}
	d.nics[idx] = dhcpApplied{link: link, addr: addr, route: route, dns: leaseDNS(lease), ntp: leaseNTPServers(lease), domain: leaseDomain(lease.DomainName)}
	d.applyMTU(link, lease.MTU)
	d.writeResolvers()
	return nil
}

// leaseDNS returns the DNS servers of a lease (option 6).
func leaseDNS(lease *dhclient.Lease) []string {
	var out []string
	for _, ip := range lease.DNS {
		if ip.To4() != nil && !ip.IsUnspecified() {
			out = append(out, ip.String())
		}
	}
	return out
}

// applyMTU sets the lease MTU (option 26) on the NIC, like networkd with
// netplan's UseMTU=true. Teardown restores the MTU from before the first
// change. The caller holds d.mu.
func (d *DHCPMode) applyMTU(link netlink.Link, leaseMTU uint16) {
	if leaseMTU == 0 {
		return // no option 26
	}
	mtu := clampMTU(int(leaseMTU), dhcpLinkMaxMTU(link.Attrs().Index), dhcpIPv6Enabled(link.Attrs().Name))
	idx := link.Attrs().Index
	m, ok := d.mtus[idx]
	if !ok {
		m = dhcpMTU{link: link, orig: link.Attrs().MTU, set: link.Attrs().MTU}
	}
	if m.set == mtu {
		return
	}
	if err := dhcpSetMTU(link, mtu); err != nil {
		d.log.Warn("failed to set the DHCP MTU", "iface", link.Attrs().Name, "mtu", mtu, "error", err)
		return
	}
	m.set = mtu
	if d.mtus == nil {
		d.mtus = map[int]dhcpMTU{}
	}
	d.mtus[idx] = m
}

// writeResolvers writes the configured DNS servers and then those of the
// leases (by NIC index, without duplicates), and the lease domains as search
// domains, to resolv.conf when the content changed. The caller holds d.mu.
func (d *DHCPMode) writeResolvers() {
	if !d.manageDNS {
		return
	}
	servers := slices.Clone(d.resolvers)
	var search []string
	for _, idx := range slices.Sorted(maps.Keys(d.nics)) {
		n := d.nics[idx]
		for _, s := range n.dns {
			if !slices.Contains(servers, s) {
				servers = append(servers, s)
			}
		}
		if n.domain != "" && !slices.Contains(search, n.domain) {
			search = append(search, n.domain)
		}
	}
	content := strings.Join(servers, ",") + " search " + strings.Join(search, " ")
	// Nothing to write before the first write without servers (the
	// provisioner resolv.conf stays); after it, no servers left means the
	// last lease ended, and its servers must go.
	if content == d.written || d.written == "" && len(servers) == 0 {
		return
	}
	if err := dhcpWriteResolvers(servers, search); err != nil {
		d.log.Warn("failed to write the DHCP DNS servers", "servers", servers, "search", search, "error", err)
		return
	}
	d.written = content
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
func (d *DHCPMode) dropUnappliedAddr(link netlink.Link, addr *netlink.Addr, prev *dhcpApplied, had bool) {
	if had && prev.addr.IP.Equal(addr.IP) {
		if prev.addr.IPNet.String() != addr.IPNet.String() {
			if err := dhcpAddrReplace(link, prev.addr); err != nil {
				d.log.Warn("failed to restore DHCP address", "iface", link.Attrs().Name, "error", err)
				prev.addr = addr // track what the kernel has
				d.nics[link.Attrs().Index] = *prev
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

// leaseUsable rejects a lease without a lifetime or a server identifier,
// like networkd (sd-dhcp-client.c client_parse_message): applying it would
// end it at once, and the client would apply it again in a loop.
func leaseUsable(lease *dhclient.Lease) error {
	if !lease.Expire.After(lease.Bound) {
		return fmt.Errorf("lease has no lifetime")
	}
	if ip := lease.ServerID.To4(); ip == nil || ip.IsUnspecified() {
		return fmt.Errorf("lease has no server identifier")
	}
	return nil
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
	d.restarts.Wait() // a restart that began before the stop
	d.mu.Lock()
	for _, t := range d.expiry {
		t.timer.Stop()
	}
	d.expiry = nil
	applied := d.stale
	for _, a := range d.nics {
		applied = append(applied, a)
	}
	mtus := d.mtus
	d.nics, d.stale, d.mtus, d.written = nil, nil, nil, ""
	setDHCPNTPList(nil)
	d.mu.Unlock()
	errs := restoreMTUs(mtus)
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

// clampMTU keeps a lease MTU between the minimum (576, 1280 with IPv6) and
// the largest MTU of the NIC (0: unknown), like networkd.
func clampMTU(mtu, linkMax int, ipv6 bool) int {
	mtu = max(mtu, dhcpMinMTU)
	if ipv6 {
		mtu = max(mtu, dhcpMinMTUIPv6)
	}
	if linkMax > 0 {
		mtu = min(mtu, linkMax)
	}
	return mtu
}

// linkMaxMTU returns the largest MTU of a link (IFLA_MAX_MTU), 0 when the
// kernel does not say.
func linkMaxMTU(index int) int {
	req := nl.NewNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(index) //nolint:gosec // an interface index
	req.AddData(msg)
	msgs, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	if err != nil || len(msgs) == 0 || len(msgs[0]) < unix.SizeofIfInfomsg {
		return 0
	}
	attrs, err := nl.ParseRouteAttr(msgs[0][unix.SizeofIfInfomsg:])
	if err != nil {
		return 0
	}
	for _, a := range attrs {
		if a.Attr.Type == unix.IFLA_MAX_MTU && len(a.Value) >= 4 {
			return int(nl.NativeEndian().Uint32(a.Value))
		}
	}
	return 0
}

// ipv6Enabled tells whether IPv6 is on for a NIC.
func ipv6Enabled(name string) bool {
	b, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + name + "/disable_ipv6")
	return err == nil && strings.TrimSpace(string(b)) == "0"
}

// setupDNS takes the configured resolvers; resolv.conf is DHCP mode's
// unless the provisioner keeps its own.
func (d *DHCPMode) setupDNS(cfg *Config) error {
	if cfg != nil && !cfg.KeepResolvConf {
		// A bad dns_resolver fails the setup as before; it must not block
		// writing the lease DNS servers later.
		if _, err := ResolverLines(cfg.DNSResolvers); err != nil {
			return fmt.Errorf("DNS resolvers: %w", err)
		}
	}
	d.mu.Lock()
	d.manageDNS = cfg != nil && !cfg.KeepResolvConf
	d.resolvers = nil
	if d.manageDNS {
		for _, r := range strings.Split(cfg.DNSResolvers, ",") {
			if r = strings.TrimSpace(r); r != "" {
				d.resolvers = append(d.resolvers, r)
			}
		}
	}
	d.writeResolvers()
	d.mu.Unlock()
	return nil
}

// leaseDomain returns option 15 without spaces and one trailing NUL, like
// networkd (MAKE_CSTRING_ALLOW_TRAILING_NUL).
func leaseDomain(s string) string {
	return strings.TrimSpace(strings.TrimSuffix(s, "\x00"))
}

// ManagesDNS tells whether DHCP mode writes resolv.conf itself (the
// configured servers and those of the leases).
func (d *DHCPMode) ManagesDNS() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.manageDNS
}

// restoreMTUs sets the MTU from before the first lease again.
func restoreMTUs(mtus map[int]dhcpMTU) []error {
	var errs []error
	for _, m := range mtus {
		if m.set == m.orig {
			continue
		}
		if err := dhcpSetMTU(m.link, m.orig); err != nil {
			errs = append(errs, fmt.Errorf("restore MTU %d on %s: %w", m.orig, m.link.Attrs().Name, err))
		}
	}
	return errs
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
