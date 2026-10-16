//go:build linux

package gobgp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const (
	// fibDebounce collects the follow-up events of a burst into one
	// reconcile; the first event of a burst reconciles at once.
	fibDebounce = 200 * time.Millisecond
	// fibResyncDefault is the period of the full reconcile, which
	// reinstalls every route, so routes that the kernel dropped (a NIC
	// down, a flushed table) come back, and re-resolves the recursive next
	// hops.
	fibResyncDefault = 30 * time.Second
	// fibMetric is the metric zebra installs BGP routes with
	// (ROUTE_INSTALLATION_METRIC).
	fibMetric = 20
	// unresolvedLogEvery limits the warning about an unresolvable next hop.
	unresolvedLogEvery = time.Minute
)

// fibWriter installs the IPv4 and IPv6 unicast best paths of the GoBGP
// RIB into the kernel routing table of their sessions' VRF, like zebra
// does for bgpd (#630 C17). The routes carry protocol "bgp", so the writer
// only replaces and deletes its own routes.
type fibWriter struct {
	bgp  *server.BgpServer
	sel  *fibSelector
	nl   *netlink.Handle
	vrfs map[int]string // table -> VRF device, for next hop resolution
	log  *slog.Logger

	// installed holds the routes this writer installed, by table and
	// prefix.
	installed  map[string]*netlink.Route
	unresolved map[netip.Addr]time.Time
	resync     time.Duration
}

func newFIBWriter(bgp *server.BgpServer, sel *fibSelector, vrfs map[int]string) *fibWriter {
	return &fibWriter{
		bgp:        bgp,
		sel:        sel,
		nl:         &netlink.Handle{},
		vrfs:       vrfs,
		log:        slog.With("tier", "underlay", "component", "fib"),
		installed:  map[string]*netlink.Route{},
		unresolved: map[netip.Addr]time.Time{},
		resync:     fibResyncDefault,
	}
}

// run reconciles the kernel tables on every best path change and every
// change of the other kernel routes (the next hops may resolve
// differently) until ctx ends, then removes the routes it installed.
func (w *fibWriter) run(ctx context.Context) {
	changed := make(chan struct{}, 1)
	notify := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	err := w.bgp.WatchEvent(ctx, &apipb.WatchEventRequest{Table: &apipb.WatchEventRequest_Table{
		// A new equal-cost path does not change the best path, so every
		// received update triggers a reconcile too.
		Filters: []*apipb.WatchEventRequest_Table_Filter{
			{Type: apipb.WatchEventRequest_Table_Filter_BEST, Init: true},
			{Type: apipb.WatchEventRequest_Table_Filter_ADJIN},
		},
	}}, func(*apipb.WatchEventResponse) { notify() })
	if err != nil {
		w.log.Error("watch best paths", "error", err)
		return
	}
	w.watchUnderlay(ctx, notify)
	defer w.removeAll()

	resync := time.NewTicker(w.resync)
	defer resync.Stop()
	var pending <-chan time.Time
	var last time.Time
	for {
		force := false
		select {
		case <-ctx.Done():
			return
		case <-changed:
			now, wait := debounce(pending != nil, time.Since(last))
			if !now {
				if pending == nil {
					pending = time.After(wait)
				}
				continue
			}
		case <-pending:
			pending = nil
		case <-resync.C:
			force = true
		}
		last = time.Now()
		if err := w.reconcile(ctx, force); err != nil {
			w.log.Warn("FIB update failed", "error", err)
		}
	}
}

// debounce decides on an event: leading edge, so the first event of a
// burst reconciles at once (now) and the rest of the burst once, wait
// later. pending is a reconcile already scheduled, sinceLast the time
// since the last reconcile.
func debounce(pending bool, sinceLast time.Duration) (now bool, wait time.Duration) {
	switch {
	case pending:
		return false, 0
	case sinceLast >= fibDebounce:
		return true, 0
	}
	return false, fibDebounce - sinceLast
}

// watchUnderlay calls notify on every kernel route change that is not
// one of the writer's own, so recursive next hops are resolved again.
func (w *fibWriter) watchUnderlay(ctx context.Context, notify func()) {
	updates := make(chan netlink.RouteUpdate, 64)
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(done)
	}()
	if err := netlink.RouteSubscribeWithOptions(updates, done, netlink.RouteSubscribeOptions{
		ErrorCallback: func(err error) {
			if ctx.Err() == nil {
				w.log.Warn("route updates", "error", err)
			}
		},
	}); err != nil {
		w.log.Warn("watch kernel routes; recursive next hops follow only the periodic resync", "error", err)
		return
	}
	go func() {
		for u := range updates {
			if u.Protocol != unix.RTPROT_BGP {
				notify()
			}
		}
	}()
}

func (w *fibWriter) reconcile(ctx context.Context, force bool) error {
	want, err := w.wanted(ctx)
	if err != nil {
		return err
	}
	w.apply(want, force)
	return nil
}

// wanted returns the kernel routes of the current RIB by table and prefix.
func (w *fibWriter) wanted(ctx context.Context) (map[string]*netlink.Route, error) {
	want := map[string]*netlink.Route{}
	for _, afi := range []apipb.Family_Afi{apipb.Family_AFI_IP, apipb.Family_AFI_IP6} {
		var dests []*apipb.Destination
		err := w.bgp.ListPath(ctx, &apipb.ListPathRequest{
			TableType: apipb.TableType_GLOBAL,
			Family:    &apipb.Family{Afi: afi, Safi: apipb.Family_SAFI_UNICAST},
		}, func(d *apipb.Destination) { dests = append(dests, d) })
		if err != nil {
			return nil, fmt.Errorf("list paths: %w", err)
		}
		routes, err := w.sel.selectRoutes(dests)
		if err != nil {
			w.log.Warn("skip paths", "error", err)
		}
		for i := range routes {
			r, err := w.kernelRoute(&routes[i])
			if err != nil {
				w.log.Warn("skip route", "prefix", routes[i].Prefix, "error", err)
				continue
			}
			if r != nil {
				want[routeKey(r.Table, routes[i].Prefix)] = r
			}
		}
	}
	return want, nil
}

// apply installs the wanted routes that changed (all with force) and
// removes the installed routes that are no longer wanted.
func (w *fibWriter) apply(want map[string]*netlink.Route, force bool) {
	for key, r := range want {
		if old, ok := w.installed[key]; ok && !force && sameRoute(old, r) {
			continue
		}
		if err := w.nl.RouteReplace(r); err != nil {
			w.log.Warn("install route", "route", key, "error", err)
			continue
		}
		if old, ok := w.installed[key]; !ok || !sameRoute(old, r) {
			w.log.Info("installed route", "route", key, "nexthops", len(r.MultiPath))
		}
		w.installed[key] = r
	}
	for key, r := range w.installed {
		if _, ok := want[key]; ok {
			continue
		}
		if err := w.nl.RouteDel(r); err != nil {
			w.log.Warn("remove route", "route", key, "error", err)
		}
		delete(w.installed, key)
		w.log.Info("removed route", "route", key)
	}
}

func routeKey(table int, p netip.Prefix) string { return fmt.Sprintf("%d/%s", table, p) }

func (w *fibWriter) removeAll() {
	for key, r := range w.installed {
		if err := w.nl.RouteDel(r); err != nil {
			w.log.Debug("remove route", "route", key, "error", err)
		}
	}
	w.installed = map[string]*netlink.Route{}
}

// kernelRoute builds the netlink route with the zebra metric. IPv4
// prefixes with an IPv6 next hop (RFC 5549, unnumbered peers) use RTA_VIA.
// A next hop that is not on a connected network is resolved to its
// connected gateway, like zebra next hop tracking. It returns nil when no
// next hop resolves.
func (w *fibWriter) kernelRoute(r *fibRoute) (*netlink.Route, error) {
	dst := &net.IPNet{IP: r.Prefix.Addr().AsSlice(), Mask: net.CIDRMask(r.Prefix.Bits(), r.Prefix.Addr().BitLen())}
	route := &netlink.Route{Dst: dst, Table: r.Table, Protocol: unix.RTPROT_BGP, Priority: fibMetric}
	for _, nh := range r.Nexthops {
		info, err := w.nexthop(r, nh)
		if err != nil {
			return nil, err
		}
		if info != nil {
			route.MultiPath = append(route.MultiPath, info)
		}
	}
	if len(route.MultiPath) == 0 {
		return nil, nil //nolint:nilnil // no resolvable next hop: nothing to install
	}
	return route, nil
}

func (w *fibWriter) nexthop(r *fibRoute, nh fibNexthop) (*netlink.NexthopInfo, error) {
	info := &netlink.NexthopInfo{}
	if nh.Dev != "" {
		link, err := w.nl.LinkByName(nh.Dev)
		if err != nil {
			return nil, fmt.Errorf("next hop device %s: %w", nh.Dev, err)
		}
		info.LinkIndex = link.Attrs().Index
	}
	gw := nh.Addr
	if nh.Dev == "" {
		dev, via, ok := w.resolve(r.Table, nh.Addr)
		if !ok {
			return nil, nil //nolint:nilnil // unresolvable next hop: skipped and logged
		}
		info.LinkIndex, gw = dev, via
	}
	ip := net.IP(gw.AsSlice())
	if r.Prefix.Addr().Is4() && gw.Is6() {
		info.Via = &netlink.Via{AddrFamily: nl.FAMILY_V6, Addr: ip}
	} else {
		info.Gw = ip
	}
	return info, nil
}

// resolve returns the device and the connected gateway of a next hop in
// the VRF of table: the next hop itself when it is on a connected network,
// else the gateway the kernel uses to reach it (zebra NHT).
func (w *fibWriter) resolve(table int, nh netip.Addr) (int, netip.Addr, bool) {
	routes, err := w.nl.RouteGetWithOptions(net.IP(nh.AsSlice()), &netlink.RouteGetOptions{VrfName: w.vrfs[table]})
	if err == nil && len(routes) > 0 && routes[0].LinkIndex > 0 && routes[0].Type != unix.RTN_LOCAL {
		via := nh
		if gw, ok := netip.AddrFromSlice(routes[0].Gw); ok && routes[0].Gw != nil {
			via = gw.Unmap()
		}
		delete(w.unresolved, nh)
		return routes[0].LinkIndex, via, true
	}
	if last, ok := w.unresolved[nh]; !ok || time.Since(last) >= unresolvedLogEvery {
		w.unresolved[nh] = time.Now()
		w.log.Warn("next hop not reachable; its routes are not installed", "nexthop", nh, "table", table, "error", err)
	}
	return 0, netip.Addr{}, false
}

func sameRoute(a, b *netlink.Route) bool {
	if a.Priority != b.Priority || len(a.MultiPath) != len(b.MultiPath) {
		return false
	}
	for i := range a.MultiPath {
		x, y := a.MultiPath[i], b.MultiPath[i]
		if x.LinkIndex != y.LinkIndex || !x.Gw.Equal(y.Gw) || (x.Via == nil) != (y.Via == nil) {
			return false
		}
		if x.Via != nil && x.Via.String() != y.Via.String() {
			return false
		}
	}
	return true
}
