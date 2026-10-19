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

	"github.com/google/gopacket/layers"
	"github.com/telekom/BOOTy/pkg/network/dhcp4"
	"github.com/telekom/BOOTy/pkg/network/lldp"
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
	// watchDone ends the watch for new NICs.
	watchDone chan struct{}
	mtus      map[int]dhcpMTU // by link index: MTU before the first lease changed it

	manageDNS bool     // false: the provisioner resolv.conf stays
	resolvers []string // configured DNS servers, written before the lease ones
	written   string   // the resolv.conf content last written
}

// nicClient is the running DHCP client of one NIC.
type nicClient struct {
	client dhcpClient
	link   netlink.Link
	// down is set while the NIC has no carrier.
	down bool
	// lost is set when the NIC lost carrier after its current lease was
	// bound: only then does the carrier's return confirm the lease.
	lost bool
}

// dhcpStart starts a client; tests replace it.
var dhcpStart = func(c dhcpClient) dhcpClient {
	c.Start()
	return c
}

// dhcpApplied is what a lease configured: address, routes, DNS and NTP
// servers.
type dhcpApplied struct {
	link   netlink.Link
	addr   *netlink.Addr
	routes []*netlink.Route
	dns    []string
	ntp    []string
	// domains are the lease domain (option 15) and search list (option
	// 119), search domains like networkd with netplan (the legacy
	// resolv.conf has them as "search").
	domains []string
}

// dhcpClient is the part of a dhcp4 client that DHCPMode uses.
type dhcpClient interface {
	Start()
	Stop()
	// Reboot confirms the lease address with an INIT-REBOOT request.
	Reboot()
}

// Seams for Setup tests.
var (
	dhcpInterfaces = physicalInterfaces
	dhcpLinkByName = netlink.LinkByName
	// dhcpCarrier tells whether the NIC has carrier now (IFF_LOWER_UP); an
	// error counts as carrier.
	dhcpCarrier = func(index int) bool {
		l, err := netlink.LinkByIndex(index)
		return err != nil || l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
	}
	dhcpLinkSetUp   = netlink.LinkSetUp
	dhcpPrepareLink = promoteSecondaries
	dhcpSubscribe   = netlink.LinkSubscribeWithOptions
	dhcpNewClient   = func(iface *net.Interface, hostname string, onBound, onExpire dhcp4.Callback) dhcpClient {
		return newDHCPClient(iface, hostname, dhcpClientID(iface.Name, lldp.MachineID()), onBound, onExpire)
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
	d.mu.Lock()
	d.stopped = false
	d.mu.Unlock()
	leased := make(chan string, 16)
	hostname, _ := os.Hostname()
	// The link watch first, so a NIC that appears while the NICs are
	// listed is not missed; a NIC that both see gets one client.
	d.watchNewNICs(hostname, leased)
	ifaces, err := dhcpInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("listing interfaces: %w", err)
	}
	if len(ifaces) == 0 {
		return fmt.Errorf("no physical interfaces found for DHCP")
	}
	d.log.Info("Starting DHCP on all interfaces", "count", len(ifaces))
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
	// A NIC without carrier at the start counts as down, so its first
	// carrier sends the INIT-REBOOT at once, which in INIT restarts the
	// DISCOVER without the backoff, like networkd starts with the carrier.
	nc := &nicClient{client: dhcpStart(d.clientFor(iface, link, hostname, leased)), link: link,
		down: link.Attrs().RawFlags&unix.IFF_LOWER_UP == 0}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		nc.client.Stop() // the stop came while this client started
		return
	}
	if d.clients == nil {
		d.clients = map[int]*nicClient{}
	}
	if d.clients[link.Attrs().Index] != nil {
		d.mu.Unlock()
		nc.client.Stop() // a restart put a client there meanwhile
		return
	}
	d.clients[link.Attrs().Index] = nc
	d.mu.Unlock()
}

// watchNewNICs follows the links while the clients run, like networkd
// follows udev and the carrier:
//   - a physical NIC that appears after Setup (a driver that brings up its
//     ports late) gets its own client;
//   - a NIC that loses carrier keeps its lease state (address, routes):
//     a connection from its address, like the image stream, resumes when
//     the carrier returns (see docs/networking.md for why BOOTy does not
//     drop it like networkd);
//   - when the carrier comes back, the client confirms its address with an
//     INIT-REBOOT DHCPREQUEST at once, like networkd, and the ACK applies
//     the lease again.
func (d *DHCPMode) watchNewNICs(hostname string, leased chan<- string) {
	done := make(chan struct{})
	d.mu.Lock()
	d.watchDone = done
	d.mu.Unlock()
	subscribe := dhcpSubscribe
	go func() {
		// Links that are no physical NIC, by index and name: not checked
		// again until they go or are renamed.
		notNIC := map[linkKey]bool{}
		for {
			updates := make(chan netlink.LinkUpdate, 16)
			opts := netlink.LinkSubscribeOptions{ErrorCallback: func(err error) {
				d.log.Warn("link watch for DHCP failed, subscribing again", "error", err)
			}}
			if err := subscribe(updates, done, opts); err != nil {
				d.log.Warn("NICs that appear later get no DHCP client", "error", err)
				return
			}
			for u := range updates {
				d.linkUpdate(u, notNIC, hostname, leased)
			}
			select {
			case <-done:
				return
			case <-time.After(time.Second): // the subscription ended on an error
			}
		}
	}()
}

// linkKey is a link by index and name.
type linkKey struct {
	index int
	name  string
}

// linkUpdate starts a client for a new physical NIC.
func (d *DHCPMode) linkUpdate(u netlink.LinkUpdate, notNIC map[linkKey]bool, hostname string, leased chan<- string) {
	if u.Link == nil {
		return
	}
	key := linkKey{u.Attrs().Index, u.Attrs().Name}
	if u.Header.Type == unix.RTM_DELLINK {
		for k := range notNIC {
			if k.index == key.index {
				delete(notNIC, k) // the index can come back for another link
			}
		}
		return
	}
	if u.Header.Type != unix.RTM_NEWLINK {
		return
	}
	// The carrier of a NIC with a client first: no NIC lookup on the way.
	if d.carrierChange(u) || notNIC[key] {
		return
	}
	d.mu.Lock()
	stopped := d.stopped
	d.mu.Unlock()
	if stopped {
		return
	}
	ifaces, err := dhcpInterfaces(context.Background())
	if err != nil {
		return
	}
	i := slices.IndexFunc(ifaces, func(n net.Interface) bool { return n.Index == key.index })
	if i < 0 {
		notNIC[key] = true
		return
	}
	d.log.Info("Starting DHCP on a new NIC", "interface", ifaces[i].Name)
	d.startClient(&ifaces[i], hostname, leased)
}

// carrierChange handles the carrier of a NIC that has a client and tells
// whether the NIC has one.
func (d *DHCPMode) carrierChange(u netlink.LinkUpdate) bool {
	d.mu.Lock()
	nc, known := d.clients[u.Attrs().Index]
	if !known || d.stopped {
		d.mu.Unlock()
		return known
	}
	carrier := u.Flags&unix.IFF_LOWER_UP != 0
	switch {
	case !carrier && !nc.down:
		nc.down = true
		_, nc.lost = d.nics[u.Attrs().Index]
		d.log.Info("NIC lost carrier, keeping its DHCP lease until the carrier returns", "interface", nc.link.Attrs().Name)
	case carrier && nc.down:
		nc.down = false
		_, leased := d.nics[u.Attrs().Index]
		lost := nc.lost
		nc.lost = false
		d.mu.Unlock()
		switch {
		case lost:
			d.log.Info("NIC has carrier again, confirming its DHCP lease", "interface", nc.link.Attrs().Name)
			nc.client.Reboot()
		case !leased:
			// No lease yet (the NIC came up after its client started):
			// restart the DISCOVER without the backoff, like networkd
			// starts its client with the carrier. A lease bound
			// meanwhile drops the request (dhcp4 bind).
			d.log.Info("NIC has carrier, starting DHCP at once", "interface", nc.link.Attrs().Name)
			nc.client.Reboot()
		}
		return true
	}
	d.mu.Unlock()
	return true
}

// stopClients stops every DHCP client, all at once, and waits for them. A
// client is stopped by whoever takes it out of d.clients.
func (d *DHCPMode) stopClients() {
	d.mu.Lock()
	d.stopped = true
	clients := d.clients
	d.clients = nil
	if d.watchDone != nil {
		close(d.watchDone)
		d.watchDone = nil
	}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Go(c.client.Stop)
	}
	wg.Wait()
}

// clientFor builds the DHCP client of one NIC with its lease callbacks.
func (d *DHCPMode) clientFor(iface *net.Interface, link netlink.Link, hostname string, leased chan<- string) dhcpClient {
	return dhcpNewClient(iface, hostname, d.onBound(link, iface.Name, leased), d.onExpire(link, iface.Name))
}

// newDHCPClient builds the client of one NIC (package dhcp4), which sends
// the options of networkd in its order: client identifier (61), parameter
// request list (55), maximum message size (57, the MTU less 28) and host
// name (12) when one is set, and follows networkd's states and backoff.
func newDHCPClient(iface *net.Interface, hostname string, clientID []byte, onBound, onExpire dhcp4.Callback) *dhcp4.Client {
	return &dhcp4.Client{
		Iface:    iface,
		Hostname: hostname,
		ClientID: clientID,
		Params:   networkdParams,
		OnBound:  onBound,
		OnExpire: onExpire,
		// The legacy ramdisk releases its leases when it stops (networkd
		// SendRelease=yes, captured in kindmetal).
		SendRelease: true,
		Log:         slog.Default(),
	}
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
func (d *DHCPMode) onBound(link netlink.Link, ifName string, leased chan<- string) func(*dhcp4.Lease) {
	return func(lease *dhcp4.Lease) {
		addr, err := leaseAddr(lease)
		if err == nil {
			err = leaseUsable(lease)
		}
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
		if nc := d.clients[link.Attrs().Index]; nc != nil {
			// The carrier now decides: up clears the down state of the
			// start; down (the ACK was read after the carrier went) makes
			// its return confirm this lease.
			up := dhcpCarrier(link.Attrs().Index)
			nc.down, nc.lost = !up, !up
		}
		d.log.Info("DHCP lease applied", "iface", ifName, "addr", addr.IPNet.String())
		select {
		case leased <- ifName:
		default:
		}
	}
}

// onExpire returns the callback for the end of a lease of one NIC: it
// expired before a renewal or a rebind succeeded, or the server sent a
// DHCPNAK. RFC 2131 sections 4.4.5 and 3.2 say the client then stops using
// the address and starts over in INIT (the dhcp4 client does), so the
// address and routes of the lease go; the next lease is applied like any
// other.
func (d *DHCPMode) onExpire(link netlink.Link, ifName string) func(*dhcp4.Lease) {
	return func(*dhcp4.Lease) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.endLease(link, ifName)
	}
}

// endLease removes the address and routes of the lease of a NIC. State that cannot be removed stays tracked for
// Teardown. The caller holds d.mu.
func (d *DHCPMode) endLease(link netlink.Link, ifName string) {
	idx := link.Attrs().Index
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
	for _, r := range a.routes {
		if err := dhcpRouteDel(r); err != nil && !errors.Is(err, syscall.ESRCH) {
			d.log.Warn("failed to remove a route of an ended DHCP lease", "iface", ifName, "route", routeDst(r), "error", err)
			d.stale = append(d.stale, dhcpApplied{link: link, routes: []*netlink.Route{r}})
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
func (d *DHCPMode) apply(link netlink.Link, addr *netlink.Addr, lease *dhcp4.Lease) error {
	if d.nics == nil {
		d.nics = map[int]dhcpApplied{}
	}
	idx := link.Attrs().Index
	prev, had := d.nics[idx]
	// Replace, so an address left by an earlier lease gets this mask.
	if err := dhcpAddrReplace(link, addr); err != nil {
		return fmt.Errorf("assign DHCP address: %w", err)
	}
	routes, err := d.applyRoutes(link, addr, lease, prev.routes)
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
	d.nics[idx] = dhcpApplied{link: link, addr: addr, routes: routes, dns: leaseDNS(lease), ntp: leaseNTPServers(lease), domains: leaseSearchDomains(lease)}
	d.applyMTU(link, lease.MTU)
	d.writeResolvers()
	return nil
}

// leaseDNS returns the DNS servers of a lease (option 6).
func leaseDNS(lease *dhcp4.Lease) []string {
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
		for _, dom := range n.domains {
			if !slices.Contains(search, dom) {
				search = append(search, dom)
			}
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

// applyRoutes adds the routes of a lease (leaseRoutes) next to the routes
// of other NICs, then removes the routes of the previous lease of this NIC
// that the lease does not have. Routes are appended, so a second NIC's
// default route does not replace the first; a route that is there already
// (a renewal) is EEXIST. When a route fails, the routes added here are
// removed again and the previous ones stay.
func (d *DHCPMode) applyRoutes(link netlink.Link, addr *netlink.Addr, lease *dhcp4.Lease, prev []*netlink.Route) ([]*netlink.Route, error) {
	want := leaseRoutes(link, addr, lease)
	var added []*netlink.Route
	for _, r := range want {
		if err := dhcpRouteAdd(r); err != nil && !errors.Is(err, syscall.EEXIST) {
			for _, a := range added {
				if delErr := dhcpRouteDel(a); delErr != nil && !errors.Is(delErr, syscall.ESRCH) {
					d.stale = append(d.stale, dhcpApplied{link: link, routes: []*netlink.Route{a}})
				}
			}
			return nil, fmt.Errorf("add DHCP route %s via %s: %w", routeDst(r), r.Gw, err)
		}
		if !slices.ContainsFunc(prev, func(p *netlink.Route) bool { return sameRoute(p, r) }) {
			added = append(added, r)
		}
	}
	for _, p := range prev {
		if slices.ContainsFunc(want, func(r *netlink.Route) bool { return sameRoute(p, r) }) {
			continue
		}
		if err := dhcpRouteDel(p); err != nil && !errors.Is(err, syscall.ESRCH) {
			d.log.Warn("failed to remove an old DHCP route", "iface", link.Attrs().Name, "route", routeDst(p), "error", err)
			d.stale = append(d.stale, dhcpApplied{link: link, routes: []*netlink.Route{p}})
		}
	}
	return want, nil
}

func routeDst(r *netlink.Route) string {
	if r.Dst == nil {
		return "default"
	}
	return r.Dst.String()
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
func leaseNTPServers(lease *dhcp4.Lease) []string {
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
func leaseUsable(lease *dhcp4.Lease) error {
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
func leaseAddr(lease *dhcp4.Lease) (*netlink.Addr, error) {
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
	mtus := d.mtus
	d.nics, d.stale, d.mtus, d.written = nil, nil, nil, ""
	setDHCPNTPList(nil)
	d.mu.Unlock()
	errs := restoreMTUs(mtus)
	for _, a := range applied {
		for _, r := range a.routes {
			if err := dhcpRouteDel(r); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("delete DHCP route %s: %w", routeDst(r), err))
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
