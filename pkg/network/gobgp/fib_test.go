//go:build linux && linux_e2e

package gobgp

import (
	"context"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/apiutil"
	"github.com/osrg/gobgp/v3/pkg/packet/bgp"
	"github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// fibTestNS creates a network namespace with VRF vrf-t (table 1000) and a
// dummy device dum-t (10.99.0.1/24) in it, plus 10.98.0.0/24 via
// 10.99.0.3 in table 1000. It returns a netlink handle for it.
func fibTestNS(t *testing.T) *netlink.Handle {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	origin, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := netns.Set(origin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ns.Close() })
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	vrf := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf-t"}, Table: 1000}
	must(h.LinkAdd(vrf))
	dum := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dum-t", MasterIndex: 0}}
	must(h.LinkAdd(dum))
	vl, err := h.LinkByName("vrf-t")
	must(err)
	dl, err := h.LinkByName("dum-t")
	must(err)
	must(h.LinkSetMaster(dl, vl))
	must(h.LinkSetUp(vl))
	must(h.LinkSetUp(dl))
	addr, _ := netlink.ParseAddr("10.99.0.1/24")
	must(h.AddrAdd(dl, addr))
	_, sub, _ := net.ParseCIDR("10.98.0.0/24")
	must(h.RouteAdd(&netlink.Route{Dst: sub, Gw: net.ParseIP("10.99.0.3"), LinkIndex: dl.Attrs().Index, Table: 1000, Protocol: unix.RTPROT_STATIC}))
	return h
}

// fibTestRoutes returns the BGP routes of table 1000 as "prefix via gw
// metric".
func fibTestRoutes(t *testing.T, h *netlink.Handle) []string {
	t.Helper()
	routes, err := h.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: 1000, Protocol: unix.RTPROT_BGP}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		gw := r.Gw
		if gw == nil && len(r.MultiPath) > 0 {
			gw = r.MultiPath[0].Gw
		}
		out = append(out, r.Dst.String()+" via "+gw.String()+" metric "+strconv.Itoa(r.Priority))
	}
	slices.Sort(out)
	return out
}

func fibTestAnnounce(t *testing.T, s *server.BgpServer, prefix, nexthop string, withdraw bool) {
	t.Helper()
	ip, ipnet, _ := net.ParseCIDR(prefix)
	ones, _ := ipnet.Mask.Size()
	nlri := bgp.NewIPAddrPrefix(uint8(ones), ip.String()) //nolint:gosec // test prefix length
	p, err := apiutil.NewPath(nlri, withdraw, []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0), bgp.NewPathAttributeNextHop(nexthop), bgp.NewPathAttributeLocalPref(100),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if withdraw {
		err = s.DeletePath(context.Background(), &apipb.DeletePathRequest{Path: p})
	} else {
		_, err = s.AddPath(context.Background(), &apipb.AddPathRequest{Path: p})
	}
	if err != nil {
		t.Fatal(err)
	}
}

func waitFIB(t *testing.T, h *netlink.Handle, want []string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		if got = fibTestRoutes(t, h); slices.Equal(got, want) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("table 1000 = %q, want %q", got, want)
}

// The FIB writer against the kernel (root only, CI job Linux E2E): it
// installs routes with metric 20 into the session VRF table, resolves a
// recursive next hop to its connected gateway, skips an unresolvable next
// hop and one that is its own address, removes a withdrawn
// route, reinstalls a route the kernel lost and cleans up when it stops.
func TestFIBWriterKernel(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for a network namespace")
	}
	h := fibTestNS(t)
	ctx := context.Background()
	fabric := server.NewBgpServer()
	go fabric.Serve()
	t.Cleanup(fabric.Stop)
	booty := server.NewBgpServer()
	go booty.Serve()
	t.Cleanup(booty.Stop)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	for _, s := range []struct {
		srv    *server.BgpServer
		id     string
		listen int32
		addr   []string
	}{{fabric, "10.0.0.1", int32(port), []string{"127.0.0.2"}}, {booty, "10.0.0.2", -1, nil}} { //nolint:gosec // test port
		if err := s.srv.StartBgp(ctx, &apipb.StartBgpRequest{Global: &apipb.Global{Asn: 65000, RouterId: s.id, ListenPort: s.listen, ListenAddresses: s.addr}}); err != nil {
			t.Fatal(err)
		}
	}
	fams := []*apipb.AfiSafi{{Config: &apipb.AfiSafiConfig{Family: &apipb.Family{Afi: apipb.Family_AFI_IP, Safi: apipb.Family_SAFI_UNICAST}}}}
	if err := fabric.AddPeer(ctx, &apipb.AddPeerRequest{Peer: &apipb.Peer{
		Conf: &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65000}, Transport: &apipb.Transport{PassiveMode: true}, AfiSafis: fams,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := booty.AddPeer(ctx, &apipb.AddPeerRequest{Peer: &apipb.Peer{
		Conf: &apipb.PeerConf{NeighborAddress: "127.0.0.2", PeerAsn: 65000}, AfiSafis: fams,
		Transport: &apipb.Transport{RemotePort: uint32(port)}, //nolint:gosec // test port
		Timers:    &apipb.Timers{Config: &apipb.TimersConfig{ConnectRetry: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	sel := &fibSelector{
		localAS: func(string) uint32 { return 65000 },
		table:   func(string) int { return 1000 },
		peerDev: func(string) string { return "" },
	}
	w := newFIBWriter(booty, sel, map[int]string{1000: "vrf-t"})
	w.nl, w.resync = h, time.Second
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(wctx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	fibTestAnnounce(t, fabric, "198.51.100.0/24", "10.99.0.2", false)
	fibTestAnnounce(t, fabric, "203.0.113.0/24", "10.98.0.5", false)
	fibTestAnnounce(t, fabric, "192.0.2.99/32", "10.97.0.1", false)
	// A next hop that is BOOTy's own address gives no route.
	fibTestAnnounce(t, fabric, "192.0.2.98/32", "10.99.0.1", false)
	waitFIB(t, h, []string{"198.51.100.0/24 via 10.99.0.2 metric 20", "203.0.113.0/24 via 10.99.0.3 metric 20"})

	fibTestAnnounce(t, fabric, "198.51.100.0/24", "10.99.0.2", true)
	waitFIB(t, h, []string{"203.0.113.0/24 via 10.99.0.3 metric 20"})

	_, lost, _ := net.ParseCIDR("203.0.113.0/24")
	if err := h.RouteDel(&netlink.Route{Dst: lost, Table: 1000, Protocol: unix.RTPROT_BGP, Priority: fibMetric}); err != nil {
		t.Fatal(err)
	}
	waitFIB(t, h, []string{"203.0.113.0/24 via 10.99.0.3 metric 20"})

	cancel()
	<-done
	if got := fibTestRoutes(t, h); len(got) != 0 {
		t.Fatalf("table 1000 after stop = %q, want no BGP routes", got)
	}
}
