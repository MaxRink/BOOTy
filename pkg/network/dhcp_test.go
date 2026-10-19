//go:build linux

package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/digineo/go-dhclient"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDHCPMode_Setup(t *testing.T) {
	// Fake an empty link list, so the test never probes the host NICs and
	// does not depend on root or on a DHCP server being reachable.
	t.Cleanup(snapshotNICFns())
	linkList = func() ([]netlink.Link, error) { return nil, nil }
	sleepFunc = func(time.Duration) {}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d := &DHCPMode{}
	err := d.Setup(ctx, &Config{})
	if err == nil {
		t.Fatal("Setup() = nil, want error when no interface gets a DHCP lease")
	}
	if len(d.clients) != 0 {
		t.Fatalf("Setup() kept DHCP clients after error %v", err)
	}
}

func TestDHCPMode_SetupCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &DHCPMode{}
	err := d.Setup(ctx, &Config{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Setup() error = %v, want context.Canceled", err)
	}
	if len(d.clients) != 0 {
		t.Fatal("Setup() kept DHCP clients after cancellation")
	}
}

func TestDHCPMode_Teardown(t *testing.T) {
	d := &DHCPMode{}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForHTTP_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := WaitForHTTP(context.Background(), srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForHTTP_Timeout(t *testing.T) {
	// Use localhost with a port nothing is listening on — connection refused is instant.
	err := WaitForHTTP(context.Background(), "http://127.0.0.1:19", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitForHTTP_TimeoutBoundsInFlightRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	start := time.Now()
	err := WaitForHTTP(context.Background(), srv.URL, 150*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > time.Second {
		t.Fatalf("WaitForHTTP took %s, want request bounded by timeout", elapsed)
	}
}

func TestWaitForHTTP_EmptyTarget(t *testing.T) {
	err := WaitForHTTP(context.Background(), "", 1*time.Second)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestWaitForHTTP_NonPositiveTimeout(t *testing.T) {
	err := WaitForHTTP(context.Background(), "http://127.0.0.1:19", 0)
	if err == nil {
		t.Fatal("expected error for non-positive timeout")
	}
	if !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("error = %q, want positive-timeout validation", err.Error())
	}
}

func TestWaitForHTTP_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	err := WaitForHTTP(ctx, "http://192.0.2.1:1", 5*time.Second)
	if err == nil {
		t.Fatal("expected context cancel error")
	}
}

func TestWaitForHTTP_AuthUnauthorized(t *testing.T) {
	// A 401 response proves network connectivity — the server is reachable
	// even though the request is not authenticated.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := WaitForHTTP(context.Background(), srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("expected 401 to count as connectivity, got error: %v", err)
	}
}

func TestWaitForHTTP_RedactsTargetInLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	target := strings.Replace(srv.URL, "http://", "http://user:secret@", 1) + "/image.raw?token=abc#frag"
	err := WaitForHTTP(context.Background(), target, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := logs.String()
	for _, leaked := range []string{"user", "secret", "token=abc", "frag"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("logs leaked %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "/image.raw") {
		t.Fatalf("logs = %q, want redacted path context", got)
	}
}

func TestDHCPMode_WaitForConnectivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &DHCPMode{}
	err := d.WaitForConnectivity(context.Background(), srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDHCPSetup_ContextCancelPropagates verifies that canceling the context
// terminates all concurrent NIC probes promptly.
// In environments with no physical NICs, Setup returns immediately with an
// error — that case is also valid and proves no blocking.
func TestDHCPSetup_ContextCancelPropagates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	d := &DHCPMode{}
	_ = d.Setup(ctx, &Config{})
	elapsed := time.Since(start)

	if elapsed > 700*time.Millisecond {
		t.Fatalf("Setup took %v — context cancellation did not propagate promptly", elapsed)
	}
}

// TestNewDHCPClientSendsNetworkdOptions pins the options of networkd
// (systemd 255) as captured in kindmetal: 61 (type 255, IAID, DUID-EN),
// 55 with networkd's list, 57 (MTU less 28) and 12 when a hostname is set.
func TestNewDHCPClientSendsNetworkdOptions(t *testing.T) {
	id := dhcpClientID("eth0", "0123456789abcdef0123456789abcdef")
	if len(id) != 19 || id[0] != 255 || id[5] != 0 || id[6] != 2 || !bytes.Equal(id[7:11], []byte{0, 0, 0xab, 0x11}) {
		t.Fatalf("client ID = %x, want type 255, IAID, DUID-EN 43793", id)
	}
	for _, hostname := range []string{"e2e-test-rfm-0", ""} {
		client := newDHCPClient(&net.Interface{Name: "eth0", MTU: 1500}, hostname, id, nil, nil)
		var types []layers.DHCPOpt
		for _, opt := range client.DHCPOptions {
			types = append(types, opt.Type)
			switch opt.Type {
			case layers.DHCPOptParamsRequest:
				if want := []byte{1, 3, 6, 12, 15, 26, 33, 42, 114, 119, 120, 121}; !bytes.Equal(opt.Data, want) {
					t.Errorf("parameter request list = %v, want %v", opt.Data, want)
				}
			case layers.DHCPOptMaxMessageSize:
				if !bytes.Equal(opt.Data, []byte{0x05, 0xc0}) {
					t.Errorf("max message size = %x, want 1472", opt.Data)
				}
			case layers.DHCPOptHostname:
				if string(opt.Data) != hostname {
					t.Errorf("hostname option = %q, want %q", opt.Data, hostname)
				}
			}
		}
		want := []layers.DHCPOpt{layers.DHCPOptClientID, layers.DHCPOptParamsRequest, layers.DHCPOptMaxMessageSize}
		if hostname != "" {
			want = append(want, layers.DHCPOptHostname)
		}
		if !slices.Equal(types, want) {
			t.Errorf("hostname %q: options = %v, want %v", hostname, types, want)
		}
	}
}

func TestLeaseAddrDefaultsMissingNetmask(t *testing.T) {
	addr, err := leaseAddr(&dhclient.Lease{FixedAddress: net.ParseIP("10.0.2.15")})
	if err != nil {
		t.Fatalf("leaseAddr() error = %v", err)
	}
	if got := addr.IPNet.String(); got != "10.0.2.15/8" {
		t.Fatalf("leaseAddr() = %s, want 10.0.2.15/8", got)
	}
	if _, err := leaseAddr(&dhclient.Lease{}); err == nil {
		t.Fatal("leaseAddr() without address should fail")
	}
	if _, err := leaseAddr(&dhclient.Lease{FixedAddress: net.IPv4zero}); err == nil {
		t.Fatal("leaseAddr() with 0.0.0.0 should fail")
	}
	if _, err := leaseAddr(&dhclient.Lease{FixedAddress: net.ParseIP("10.0.2.15"), Netmask: net.IPMask{255, 0, 255, 0}}); err == nil {
		t.Fatal("leaseAddr() with a non-contiguous mask should fail")
	}
}

// stubDHCPNetlink replaces the netlink calls of onBound for one test.
func stubDHCPNetlink(t *testing.T) {
	t.Helper()
	origAddr, origAddrDel, origRoute, origRouteDel := dhcpAddrReplace, dhcpAddrDel, dhcpRouteAdd, dhcpRouteDel
	origMTU, origResolvers := dhcpSetMTU, dhcpWriteResolvers
	origMax, origV6 := dhcpLinkMaxMTU, dhcpIPv6Enabled
	t.Cleanup(func() {
		dhcpAddrReplace, dhcpAddrDel, dhcpRouteAdd, dhcpRouteDel = origAddr, origAddrDel, origRoute, origRouteDel
		dhcpSetMTU, dhcpWriteResolvers = origMTU, origResolvers
		dhcpLinkMaxMTU, dhcpIPv6Enabled = origMax, origV6
	})
	dhcpLinkMaxMTU = func(int) int { return 0 }
	dhcpIPv6Enabled = func(string) bool { return false }
	dhcpSetMTU = func(netlink.Link, int) error { return nil }
	dhcpWriteResolvers = func([]string, []string) error { t.Fatal("unexpected resolv.conf write"); return nil }
	dhcpAddrReplace = func(netlink.Link, *netlink.Addr) error { return nil }
	dhcpAddrDel = func(netlink.Link, *netlink.Addr) error { return nil }
	dhcpRouteAdd = func(*netlink.Route) error { return nil }
	dhcpRouteDel = func(*netlink.Route) error { return nil }
}

func dummyLink(name string, index int) *netlink.Dummy {
	return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name, Index: index}}
}

func lease24(ip string, router ...string) *dhclient.Lease {
	l := &dhclient.Lease{FixedAddress: net.ParseIP(ip), Netmask: net.CIDRMask(24, 32)}
	for _, r := range router {
		l.Router = append(l.Router, net.ParseIP(r))
	}
	return live(l)
}

// live gives a test lease a server identifier and a 2 min lifetime.
func live(l *dhclient.Lease) *dhclient.Lease {
	l.ServerID = net.IPv4(10, 0, 2, 1)
	l.Bound = time.Now()
	l.Renew, l.Rebind, l.Expire = l.Bound.Add(time.Minute), l.Bound.Add(105*time.Second), l.Bound.Add(2*time.Minute)
	return l
}

// A lease without a lifetime or a server identifier is not applied, like
// networkd: it would end at once and come back in a loop.
func TestOnBoundRejectsLeaseWithoutLifetimeOrServer(t *testing.T) {
	stubDHCPNetlink(t)
	added := 0
	dhcpAddrReplace = func(netlink.Link, *netlink.Addr) error { added++; return nil }
	d := NewDHCPMode()
	noLife := lease24("10.0.2.15", "10.0.2.2")
	noLife.Expire = noLife.Bound
	noServer := lease24("10.0.2.15", "10.0.2.2")
	noServer.ServerID = nil
	zeroServer := lease24("10.0.2.15", "10.0.2.2")
	zeroServer.ServerID = net.IPv4zero
	for _, l := range []*dhclient.Lease{noLife, noServer, zeroServer} {
		d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 1))(l)
	}
	if added != 0 || len(d.nics) != 0 {
		t.Fatalf("applied %d leases without a lifetime or server: %v", added, d.nics)
	}
}

func TestOnBoundReplacesStateAndTeardownRemovesIt(t *testing.T) {
	stubDHCPNetlink(t)
	var addrs, delAddrs []*netlink.Addr
	var routes, delRoutes []*netlink.Route
	dhcpAddrReplace = func(_ netlink.Link, a *netlink.Addr) error { addrs = append(addrs, a); return nil }
	dhcpAddrDel = func(_ netlink.Link, a *netlink.Addr) error { delAddrs = append(delAddrs, a); return nil }
	dhcpRouteAdd = func(r *netlink.Route) error { routes = append(routes, r); return nil }
	dhcpRouteDel = func(r *netlink.Route) error { delRoutes = append(delRoutes, r); return nil }

	d := NewDHCPMode()
	leased := make(chan string, 1)
	lease := lease24("10.0.2.15", "10.0.2.2")
	lease.OtherOptions = []dhclient.Option{{Type: layers.DHCPOptNTPServers, Data: []byte{10, 0, 2, 3}}}
	d.onBound(dummyLink("eth0", 7), "eth0", leased)(lease)
	t.Cleanup(func() { setDHCPNTPServers(nil) })
	if got := DHCPNTPServers(); len(got) != 1 || got[0] != "10.0.2.3" {
		t.Fatalf("DHCP NTP servers = %v, want [10.0.2.3]", got)
	}

	if len(leased) != 1 || <-leased != "eth0" {
		t.Fatal("lease was not reported as bound")
	}
	if len(addrs) != 1 || addrs[0].IPNet.String() != "10.0.2.15/24" {
		t.Fatalf("addresses = %v, want 10.0.2.15/24", addrs)
	}
	// networkd: a host route to the gateway, then the default route.
	if len(routes) != 2 || routes[0].Dst.String() != "10.0.2.2/32" || routes[0].Scope != netlink.SCOPE_LINK {
		t.Fatalf("routes = %+v, want the host route to 10.0.2.2 first", routes)
	}
	r := routes[1]
	if r.Dst != nil || r.LinkIndex != 7 || !r.Gw.Equal(net.ParseIP("10.0.2.2")) || !r.Src.Equal(net.ParseIP("10.0.2.15")) ||
		r.Priority != dhcpRouteMetric || r.Protocol != unix.RTPROT_DHCP {
		t.Fatalf("default route = %+v, want via 10.0.2.2 src 10.0.2.15 on link 7, metric 100, proto dhcp", r)
	}

	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	if len(delAddrs) != 1 || len(delRoutes) != 2 {
		t.Fatalf("Teardown removed addrs=%v routes=%v, want the lease address and route", delAddrs, delRoutes)
	}
}

func TestOnBoundIgnoresInvalidLease(t *testing.T) {
	stubDHCPNetlink(t)
	d := NewDHCPMode()
	leased := make(chan string, 1)
	d.onBound(dummyLink("eth0", 1), "eth0", leased)(&dhclient.Lease{})
	if len(leased) != 0 || len(d.nics) != 0 {
		t.Fatalf("an invalid lease was applied: leased=%d state=%v", len(leased), d.nics)
	}
}

// TestOnBoundAppliesEveryNIC: like networkd with "dhcp4: true" on every
// NIC, each NIC keeps its own lease and default route; a NIC whose lease
// fails does not stop the others.
func TestOnBoundAppliesEveryNIC(t *testing.T) {
	stubDHCPNetlink(t)
	var routes []string
	dhcpAddrReplace = func(link netlink.Link, _ *netlink.Addr) error {
		if link.Attrs().Name == "eth2" {
			return syscall.EPERM
		}
		return nil
	}
	dhcpRouteAdd = func(r *netlink.Route) error {
		if r.Gw != nil {
			routes = append(routes, r.Gw.String())
		}
		return nil
	}

	d := NewDHCPMode()
	leased := make(chan string, 3)
	d.onBound(dummyLink("eth0", 1), "eth0", leased)(lease24("10.0.2.15", "10.0.2.2"))
	d.onBound(dummyLink("eth1", 2), "eth1", leased)(lease24("10.0.3.15", "10.0.3.2"))
	d.onBound(dummyLink("eth2", 3), "eth2", leased)(lease24("10.0.4.15", "10.0.4.2"))

	if want := []string{"10.0.2.2", "10.0.3.2"}; !slices.Equal(routes, want) {
		t.Fatalf("default routes added = %v, want %v", routes, want)
	}
	if len(d.nics) != 2 || d.nics[1].addr.IPNet.String() != "10.0.2.15/24" || d.nics[2].addr.IPNet.String() != "10.0.3.15/24" {
		t.Fatalf("applied state = %v, want eth0 and eth1", d.nics)
	}
	if len(leased) != 2 {
		t.Fatalf("%d leases reported, want 2", len(leased))
	}
}

func TestOnBoundRouteFailureRollsBackAddress(t *testing.T) {
	stubDHCPNetlink(t)
	var rolledBack []string
	dhcpAddrDel = func(link netlink.Link, _ *netlink.Addr) error {
		rolledBack = append(rolledBack, link.Attrs().Name)
		return nil
	}
	dhcpRouteAdd = func(r *netlink.Route) error {
		if r.LinkIndex == 1 {
			return syscall.ENETUNREACH
		}
		return nil
	}

	d := NewDHCPMode()
	leased := make(chan string, 2)
	d.onBound(dummyLink("eth0", 1), "eth0", leased)(lease24("10.0.2.15", "10.0.2.2"))
	d.onBound(dummyLink("eth1", 2), "eth1", leased)(lease24("10.0.3.15", "10.0.3.2"))

	if len(leased) != 1 || <-leased != "eth1" {
		t.Fatal("want only the lease on eth1 reported")
	}
	if len(rolledBack) != 1 || rolledBack[0] != "eth0" {
		t.Fatalf("rolled back addresses on %v, want eth0", rolledBack)
	}
	if _, ok := d.nics[1]; ok {
		t.Fatalf("eth0 state = %v, want none", d.nics[1])
	}
}

func TestOnBoundAppliesRenewal(t *testing.T) {
	stubDHCPNetlink(t)
	var set, deleted []string
	var routeDels int
	dhcpAddrReplace = func(link netlink.Link, a *netlink.Addr) error {
		set = append(set, link.Attrs().Name+" "+a.IPNet.String())
		return nil
	}
	dhcpAddrDel = func(link netlink.Link, a *netlink.Addr) error {
		deleted = append(deleted, link.Attrs().Name+" "+a.IPNet.String())
		return nil
	}
	dhcpRouteAdd = func(*netlink.Route) error { return syscall.EEXIST } // a renewal adds the same route again
	dhcpRouteDel = func(*netlink.Route) error { routeDels++; return nil }

	d := NewDHCPMode()
	bound0 := d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 4))
	bound1 := d.onBound(dummyLink("eth1", 2), "eth1", make(chan string, 4))
	bound0(lease24("10.0.2.15", "10.0.2.2"))
	bound1(lease24("10.0.3.15", "10.0.3.2"))
	bound0(lease24("10.0.2.15", "10.0.2.2"))
	if routeDels != 0 {
		t.Fatalf("a renewal with the same router removed %d routes", routeDels)
	}
	bound0(lease24("10.0.2.16", "10.0.2.2"))

	if want := []string{"eth0 10.0.2.15/24", "eth1 10.0.3.15/24", "eth0 10.0.2.15/24", "eth0 10.0.2.16/24"}; !slices.Equal(set, want) {
		t.Fatalf("addresses set = %v, want %v", set, want)
	}
	if want := []string{"eth0 10.0.2.15/24"}; !slices.Equal(deleted, want) {
		t.Fatalf("addresses deleted = %v, want %v", deleted, want)
	}
	if routeDels != 2 {
		t.Fatalf("route deletes = %d, want the gateway and default routes with the old source", routeDels)
	}
	if len(d.nics) != 2 || d.nics[1].addr.IPNet.String() != "10.0.2.16/24" || d.nics[2].addr.IPNet.String() != "10.0.3.15/24" {
		t.Fatalf("applied state = %v, want eth0 10.0.2.16/24 and eth1 10.0.3.15/24", d.nics)
	}
}

func TestOnBoundRenewalRouteFailureKeepsPreviousLease(t *testing.T) {
	stubDHCPNetlink(t)
	var deleted []string
	dhcpAddrDel = func(_ netlink.Link, a *netlink.Addr) error {
		deleted = append(deleted, a.IPNet.String())
		return nil
	}
	failRoute := false
	dhcpRouteAdd = func(*netlink.Route) error {
		if failRoute {
			return syscall.ENETUNREACH
		}
		return nil
	}

	d := NewDHCPMode()
	bound := d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 2))
	bound(lease24("10.0.2.15", "10.0.2.2"))
	failRoute = true
	bound(lease24("10.0.2.16", "10.0.2.2"))

	if want := []string{"10.0.2.16/24"}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted = %v, want only the new address rolled back", deleted)
	}
	if d.nics[1].addr.IPNet.String() != "10.0.2.15/24" || len(d.nics[1].routes) == 0 {
		t.Fatalf("applied state = %v, want the previous lease 10.0.2.15/24 with its route", d.nics)
	}
}

func TestOnBoundRenewalDropsRouteAndRestoresMask(t *testing.T) {
	stubDHCPNetlink(t)
	var routeDels int
	dhcpRouteDel = func(*netlink.Route) error { routeDels++; return nil }
	var set []string
	dhcpAddrReplace = func(_ netlink.Link, a *netlink.Addr) error {
		set = append(set, a.IPNet.String())
		return nil
	}
	failRoute := false
	dhcpRouteAdd = func(*netlink.Route) error {
		if failRoute {
			return syscall.ENETUNREACH
		}
		return nil
	}

	d := NewDHCPMode()
	bound := d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 3))
	bound(lease24("10.0.2.15", "10.0.2.2"))
	// Renewal without a router option removes the old default route.
	bound(lease24("10.0.2.15"))
	if routeDels != 2 || len(d.nics[1].routes) != 0 {
		t.Fatalf("route deletes = %d, applied state = %v, want the old default route removed", routeDels, d.nics)
	}

	// Renewal with a new mask whose route fails restores the old mask.
	failRoute = true
	bound(live(&dhclient.Lease{FixedAddress: net.ParseIP("10.0.2.15"), Netmask: net.CIDRMask(16, 32), Router: []net.IP{net.ParseIP("10.0.2.2")}}))
	if want := []string{"10.0.2.15/24", "10.0.2.15/24", "10.0.2.15/16", "10.0.2.15/24"}; !slices.Equal(set, want) {
		t.Fatalf("addresses set = %v, want %v", set, want)
	}
}

func TestOnBoundRenewalTracksAddressWhenRollbackFails(t *testing.T) {
	stubDHCPNetlink(t)
	dhcpAddrDel = func(netlink.Link, *netlink.Addr) error { return syscall.EBUSY }
	failRoute := false
	dhcpRouteAdd = func(*netlink.Route) error {
		if failRoute {
			return syscall.ENETUNREACH
		}
		return nil
	}

	d := NewDHCPMode()
	bound := d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 2))
	bound(lease24("10.0.2.15", "10.0.2.2"))
	failRoute = true
	bound(lease24("10.0.2.16", "10.0.2.2"))

	if len(d.stale) != 1 || d.stale[0].addr.IPNet.String() != "10.0.2.16/24" {
		t.Fatalf("stale state = %v, want the address whose rollback failed tracked for Teardown", d.stale)
	}
}

// fakeDHCPClient records Start and Stop; Stop takes a while, like
// go-dhclient waiting out its response timeout.
type fakeDHCPClient struct {
	mu               sync.Mutex
	started, stopped int
	onStart          func()
}

func (c *fakeDHCPClient) Start() {
	c.mu.Lock()
	c.started++
	c.mu.Unlock()
	if c.onStart != nil {
		c.onStart()
	}
}

func (c *fakeDHCPClient) Stop() {
	time.Sleep(200 * time.Millisecond)
	c.mu.Lock()
	c.stopped++
	c.mu.Unlock()
}

// stubDHCPSetup gives Setup two fake NICs and fake clients.
func stubDHCPSetup(t *testing.T, onBound func(name string, cb dhclient.Callback)) map[string]*fakeDHCPClient {
	t.Helper()
	stubDHCPNetlink(t)
	origIfaces, origByName, origUp, origPrep, origNew, origTimeout := dhcpInterfaces, dhcpLinkByName, dhcpLinkSetUp, dhcpPrepareLink, dhcpNewClient, dhcpSetupTimeout
	t.Cleanup(func() {
		dhcpInterfaces, dhcpLinkByName, dhcpLinkSetUp, dhcpPrepareLink, dhcpNewClient, dhcpSetupTimeout = origIfaces, origByName, origUp, origPrep, origNew, origTimeout
	})
	origSub := dhcpSubscribe
	t.Cleanup(func() { dhcpSubscribe = origSub })
	dhcpSubscribe = func(chan<- netlink.LinkUpdate, <-chan struct{}, netlink.LinkSubscribeOptions) error { return nil }
	dhcpInterfaces = func(context.Context) ([]net.Interface, error) {
		return []net.Interface{{Name: "eth0", Index: 1}, {Name: "eth1", Index: 2}}, nil
	}
	dhcpLinkByName = func(name string) (netlink.Link, error) {
		return dummyLink(name, int(name[3]-'0')+1), nil
	}
	dhcpLinkSetUp = func(netlink.Link) error { return nil }
	var prepared []string
	dhcpPrepareLink = func(name string) error { prepared = append(prepared, name); return nil }
	t.Cleanup(func() {
		if len(prepared) < 2 {
			t.Errorf("promote_secondaries set on %v, want every NIC", prepared)
		}
	})
	clients := map[string]*fakeDHCPClient{}
	var mu sync.Mutex
	dhcpNewClient = func(iface *net.Interface, _ string, cb, _ dhclient.Callback) dhcpClient {
		c := &fakeDHCPClient{}
		if onBound != nil {
			name := iface.Name
			c.onStart = func() { onBound(name, cb) }
		}
		mu.Lock()
		clients[iface.Name] = c
		mu.Unlock()
		return c
	}
	return clients
}

func stoppedAll(t *testing.T, clients map[string]*fakeDHCPClient, want int) {
	t.Helper()
	for name, c := range clients {
		c.mu.Lock()
		if c.started != 1 || c.stopped != want {
			t.Errorf("%s: started %d stopped %d, want 1 and %d", name, c.started, c.stopped, want)
		}
		c.mu.Unlock()
	}
}

// Without a lease at the timeout Setup fails, but the clients keep
// sending until Teardown, which stops them in parallel.
func TestSetupKeepsClientsOnTimeout(t *testing.T) {
	clients := stubDHCPSetup(t, nil)
	dhcpSetupTimeout = 50 * time.Millisecond
	d := NewDHCPMode()
	if err := d.Setup(context.Background(), &Config{}); err == nil {
		t.Fatal("Setup() = nil without a lease")
	}
	stoppedAll(t, clients, 0)
	start := time.Now()
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 350*time.Millisecond {
		t.Fatalf("Teardown took %s: the clients were not stopped in parallel", took)
	}
	stoppedAll(t, clients, 1)
}

func TestSetupStopsClientsOnCancel(t *testing.T) {
	clients := stubDHCPSetup(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewDHCPMode().Setup(ctx, &Config{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Setup() = %v, want context.Canceled", err)
	}
	stoppedAll(t, clients, 1)
}

func TestSetupKeepsClientsUntilTeardown(t *testing.T) {
	clients := stubDHCPSetup(t, func(name string, cb dhclient.Callback) {
		if name == "eth0" {
			cb(lease24("10.0.2.15", "10.0.2.2"))
		}
	})
	d := NewDHCPMode()
	if err := d.Setup(context.Background(), &Config{}); err != nil {
		t.Fatalf("Setup() = %v", err)
	}
	stoppedAll(t, clients, 0)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	stoppedAll(t, clients, 1)
}

// A lease applied while the setup timer fires counts as success.
// At the timeout Setup checks the applied leases, not only the channel: a
// lease that was applied but not reported (as when it lands while the
// timer fires) makes Setup succeed.
func TestSetupTimeoutRace(t *testing.T) {
	clients := stubDHCPSetup(t, nil)
	dhcpSetupTimeout = 50 * time.Millisecond
	d := NewDHCPMode()
	d.onBound(dummyLink("eth0", 1), "eth0", make(chan string))(lease24("10.0.2.15", "10.0.2.2")) // nobody reads the channel
	if err := d.Setup(context.Background(), &Config{}); err != nil {
		t.Fatalf("Setup() = %v with a lease applied", err)
	}
	stoppedAll(t, clients, 0)
}

// Like networkd hands the servers of every link to timesyncd: a NIC
// without option 42 does not drop the servers of another NIC; Teardown
// removes them.
func TestOnBoundKeepsNTPServersOfEveryNIC(t *testing.T) {
	stubDHCPNetlink(t)
	t.Cleanup(func() { setDHCPNTPList(nil) })
	d := NewDHCPMode()
	l0 := lease24("10.0.2.15", "10.0.2.2")
	l0.OtherOptions = []dhclient.Option{{Type: layers.DHCPOptNTPServers, Data: []byte{10, 0, 2, 3}}}
	l1 := lease24("10.0.3.15", "10.0.3.2")
	l1.OtherOptions = []dhclient.Option{{Type: layers.DHCPOptNTPServers, Data: []byte{10, 0, 3, 3, 10, 0, 2, 3}}}
	d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 4))(l0)
	d.onBound(dummyLink("eth1", 2), "eth1", make(chan string, 4))(l1)
	d.onBound(dummyLink("eth2", 3), "eth2", make(chan string, 4))(lease24("10.0.4.15", "10.0.4.2"))
	if got := DHCPNTPServers(); !slices.Equal(got, []string{"10.0.2.3", "10.0.3.3"}) {
		t.Fatalf("NTP servers = %v, want 10.0.2.3 and 10.0.3.3", got)
	}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := DHCPNTPServers(); len(got) != 0 {
		t.Fatalf("NTP servers after Teardown = %v", got)
	}
}

// A renewal with another router removes the old default route; when that
// fails, the route stays tracked and Teardown removes it, without an
// address delete for the route-only entry.
func TestOnBoundGatewayChangeAndStaleTeardown(t *testing.T) {
	stubDHCPNetlink(t)
	var deleted []string
	failDel := true
	dhcpRouteDel = func(r *netlink.Route) error {
		deleted = append(deleted, r.Gw.String())
		if failDel {
			return syscall.EBUSY
		}
		return nil
	}
	var addrDels []*netlink.Addr
	dhcpAddrDel = func(_ netlink.Link, a *netlink.Addr) error {
		if a == nil {
			t.Fatal("AddrDel called with a nil address")
		}
		addrDels = append(addrDels, a)
		return nil
	}
	d := NewDHCPMode()
	bound := d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 2))
	bound(lease24("10.0.2.15", "10.0.2.2"))
	bound(lease24("10.0.2.15", "10.0.2.3"))
	// The host route to the old gateway and the old default route.
	if !slices.Equal(deleted, []string{"<nil>", "10.0.2.2"}) {
		t.Fatalf("route deletes = %v, want the old gateway and default routes", deleted)
	}
	if len(d.stale) != 2 || d.stale[0].addr != nil || d.stale[1].addr != nil || !d.stale[1].routes[0].Gw.Equal(net.ParseIP("10.0.2.2")) {
		t.Fatalf("stale = %+v, want the old routes tracked", d.stale)
	}
	failDel = false
	deleted = nil
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{"10.0.2.2", "10.0.2.3", "<nil>", "<nil>"}) || len(addrDels) != 1 {
		t.Fatalf("Teardown deleted routes %v and %d addresses, want all four routes and the lease address", deleted, len(addrDels))
	}
}

// After the stop (Teardown) a lease that a client still delivers is not
// applied.
func TestOnBoundAfterStopIgnored(t *testing.T) {
	stubDHCPNetlink(t)
	added := 0
	dhcpAddrReplace = func(netlink.Link, *netlink.Addr) error { added++; return nil }
	d := NewDHCPMode()
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 1))(lease24("10.0.2.15", "10.0.2.2"))
	if added != 0 || len(d.nics) != 0 {
		t.Fatalf("a lease after the stop was applied: %d addresses, %v", added, d.nics)
	}
}

// TestOnExpireRemovesLeaseState checks RFC 2131 sections 4.4.5 and 3.2: when
// the lease of a NIC ends (expired, or DHCPNAK) the client stops using the
// address, so the address and default route of that NIC go and the other
// NIC keeps its lease; the next lease of the NIC is applied again.
func TestOnExpireRemovesLeaseState(t *testing.T) {
	stubDHCPNetlink(t)
	var addrs, delAddrs []*netlink.Addr
	var delRoutes []*netlink.Route
	dhcpAddrReplace = func(_ netlink.Link, a *netlink.Addr) error { addrs = append(addrs, a); return nil }
	dhcpAddrDel = func(_ netlink.Link, a *netlink.Addr) error { delAddrs = append(delAddrs, a); return nil }
	dhcpRouteAdd = func(*netlink.Route) error { return nil }
	dhcpRouteDel = func(r *netlink.Route) error { delRoutes = append(delRoutes, r); return nil }
	t.Cleanup(func() { setDHCPNTPServers(nil) })

	d := NewDHCPMode()
	eth0, eth1 := dummyLink("eth0", 7), dummyLink("eth1", 8)
	bound0 := d.onBound(eth0, "eth0", make(chan string, 2))
	bound0(lease24("10.0.2.15", "10.0.2.2"))
	d.onBound(eth1, "eth1", make(chan string, 2))(lease24("10.0.3.15", "10.0.3.2"))

	d.onExpire(eth0, "eth0")(nil)
	if len(delAddrs) != 1 || delAddrs[0].IPNet.String() != "10.0.2.15/24" || len(delRoutes) != 2 || delRoutes[0].LinkIndex != 7 || delRoutes[1].LinkIndex != 7 {
		t.Fatalf("ended lease removed addrs=%v routes=%v, want 10.0.2.15/24, its gateway route and its default route", delAddrs, delRoutes)
	}
	if _, ok := d.nics[7]; ok {
		t.Fatal("eth0 still has lease state")
	}
	if _, ok := d.nics[8]; !ok {
		t.Fatal("eth1 lost its lease")
	}
	d.onExpire(eth0, "eth0")(nil) // a NAK after the expiry changes nothing
	if len(delAddrs) != 1 {
		t.Fatalf("second end removed again: %v", delAddrs)
	}

	bound0(lease24("10.0.2.15", "10.0.2.2"))
	if _, ok := d.nics[7]; !ok || len(addrs) != 3 {
		t.Fatalf("new lease: %d address sets, state %v, want the address again", len(addrs), d.nics)
	}
}

func TestNewDHCPClientSetsExpireCallback(t *testing.T) {
	called := false
	client := newDHCPClient(&net.Interface{Name: "eth0"}, "", nil, nil, func(*dhclient.Lease) { called = true })
	if client.OnExpire == nil {
		t.Fatal("client has no OnExpire callback")
	}
	client.OnExpire(nil)
	if !called {
		t.Fatal("OnExpire is not the given callback")
	}
}

// fakeAfterFunc records the expiry timers instead of starting them.
type fakeAfterFunc struct {
	funcs  []func()
	timers []*time.Timer
}

func (f *fakeAfterFunc) install(t *testing.T) {
	t.Helper()
	orig := dhcpAfterFunc
	t.Cleanup(func() { dhcpAfterFunc = orig })
	dhcpAfterFunc = func(_ time.Duration, fn func()) *time.Timer {
		f.funcs = append(f.funcs, fn)
		tm := time.NewTimer(time.Hour) // never fires in the test
		f.timers = append(f.timers, tm)
		return tm
	}
}

// RFC 2131 section 4.4.5: a lease that expires without a renewal ends,
// even though go-dhclient v1.0.2 never calls OnExpire after a failed
// renewal. Each renewal moves the expiry, and a timer of an older lease
// does nothing.
func TestLeaseExpiryTimerEndsLease(t *testing.T) {
	stubDHCPNetlink(t)
	var delAddrs []*netlink.Addr
	dhcpAddrDel = func(_ netlink.Link, a *netlink.Addr) error { delAddrs = append(delAddrs, a); return nil }
	t.Cleanup(func() { setDHCPNTPServers(nil) })
	var timers fakeAfterFunc
	timers.install(t)

	d := NewDHCPMode()
	eth0 := dummyLink("eth0", 7)
	bound := d.onBound(eth0, "eth0", make(chan string, 4))
	first := lease24("10.0.2.15", "10.0.2.2")
	first.Expire = time.Now().Add(time.Hour)
	bound(first)
	renewed := lease24("10.0.2.15", "10.0.2.2")
	renewed.Expire = time.Now().Add(2 * time.Hour)
	bound(renewed)
	if len(timers.funcs) != 2 {
		t.Fatalf("armed %d expiry timers, want one per bound lease", len(timers.funcs))
	}
	timers.funcs[0]() // the timer of the first lease, after the renewal
	if len(delAddrs) != 0 {
		t.Fatalf("an old expiry timer removed the renewed lease: %v", delAddrs)
	}
	timers.funcs[1]()
	if len(delAddrs) != 1 || delAddrs[0].IPNet.String() != "10.0.2.15/24" {
		t.Fatalf("expiry removed %v, want 10.0.2.15/24", delAddrs)
	}
	if _, ok := d.nics[7]; ok {
		t.Fatal("eth0 still has lease state after the expiry")
	}
}

// Teardown stops the expiry timers.
func TestTeardownStopsLeaseExpiry(t *testing.T) {
	stubDHCPNetlink(t)
	t.Cleanup(func() { setDHCPNTPServers(nil) })
	var timers fakeAfterFunc
	timers.install(t)
	d := NewDHCPMode()
	l := lease24("10.0.2.15", "10.0.2.2")
	l.Expire = time.Now().Add(time.Hour)
	d.onBound(dummyLink("eth0", 7), "eth0", make(chan string, 1))(l)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(timers.timers) != 1 || timers.timers[0].Stop() {
		t.Fatal("Teardown did not stop the expiry timer")
	}
}

// A delete that fails keeps the state tracked for Teardown.
func TestEndLeaseKeepsFailedDeletesForTeardown(t *testing.T) {
	stubDHCPNetlink(t)
	t.Cleanup(func() { setDHCPNTPServers(nil) })
	boom := errors.New("boom")
	d := NewDHCPMode()
	eth0 := dummyLink("eth0", 7)
	d.onBound(eth0, "eth0", make(chan string, 1))(lease24("10.0.2.15", "10.0.2.2"))
	dhcpRouteDel = func(*netlink.Route) error { return boom }
	dhcpAddrDel = func(netlink.Link, *netlink.Addr) error { return boom }
	d.onExpire(eth0, "eth0")(nil)
	// The gateway host route, the default route and the address.
	if len(d.stale) != 3 || len(d.stale[0].routes) != 1 || len(d.stale[1].routes) != 1 || d.stale[2].addr == nil {
		t.Fatalf("stale state %+v, want both routes and the address", d.stale)
	}
	var routes, addrs int
	dhcpRouteDel = func(*netlink.Route) error { routes++; return nil }
	dhcpAddrDel = func(netlink.Link, *netlink.Addr) error { addrs++; return nil }
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if routes != 2 || addrs != 1 {
		t.Fatalf("Teardown removed %d routes and %d addresses, want the stale ones", routes, addrs)
	}
}

// Setup builds every NIC's client with the lease end callback.
func TestClientForWiresOnExpire(t *testing.T) {
	d := NewDHCPMode()
	c, ok := d.clientFor(&net.Interface{Name: "eth0"}, dummyLink("eth0", 7), "", make(chan string, 1)).(*dhclient.Client)
	if !ok || c.OnBound == nil || c.OnExpire == nil {
		t.Fatal("the NIC client has no OnBound or OnExpire callback")
	}
}

// fakeRestartClient counts Start and Stop; onStop runs inside Stop.
type fakeRestartClient struct {
	starts, stops int
	onStop        func()
}

func (c *fakeRestartClient) Start() { c.starts++ }
func (c *fakeRestartClient) Stop() {
	c.stops++
	if c.onStop != nil {
		c.onStop()
	}
}

// RFC 2131 section 4.4.5: after the expiry the client starts over in INIT.
// go-dhclient keeps the old lease after failed renewals, so BOOTy replaces
// the client of the NIC, like networkd. The old client stops without d.mu
// held (its OnBound may wait for d.mu), and after Teardown a restart stops
// the fresh client again.
func TestLeaseExpiryRestartsTheClient(t *testing.T) {
	stubDHCPNetlink(t)
	t.Cleanup(func() { setDHCPNTPServers(nil) })
	var timers fakeAfterFunc
	timers.install(t)

	d := NewDHCPMode()
	eth0 := dummyLink("eth0", 7)
	old := &fakeRestartClient{}
	old.onStop = func() {
		if !d.mu.TryLock() {
			t.Error("the old client stopped with d.mu held")
			return
		}
		d.mu.Unlock()
	}
	var fresh []*fakeRestartClient
	start := func() dhcpClient {
		c := &fakeRestartClient{starts: 1}
		fresh = append(fresh, c)
		return c
	}
	d.clients = map[int]*nicClient{7: {client: old, start: start}}
	l := lease24("10.0.2.15", "10.0.2.2")
	l.Expire = time.Now().Add(time.Hour)
	d.onBound(eth0, "eth0", make(chan string, 2))(l)
	timers.funcs[0]()
	if old.stops != 1 || len(fresh) != 1 || d.clients[7] == nil || d.clients[7].client != fresh[0] {
		t.Fatalf("old stops %d, fresh clients %d, NIC client %+v; want the old one stopped and a fresh one in place",
			old.stops, len(fresh), d.clients[7])
	}

	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fresh[0].stops != 1 {
		t.Fatalf("Teardown stopped the fresh client %d times, want once", fresh[0].stops)
	}
	d.restartClient(7, "eth0", &nicClient{client: &fakeRestartClient{}, start: start})
	if len(fresh) != 1 || len(d.clients) != 0 {
		t.Fatalf("a restart after Teardown started a client: %d fresh, %d clients", len(fresh), len(d.clients))
	}
	d.onBound(eth0, "eth0", make(chan string, 1))(lease24("10.0.2.16", "10.0.2.2"))
	if len(d.nics) != 0 {
		t.Fatalf("a lease after Teardown was applied: %+v", d.nics)
	}
}

// A DHCPNAK (OnExpire) also stops the expiry timer; go-dhclient starts
// over itself.
func TestOnExpireStopsLeaseExpiryTimer(t *testing.T) {
	stubDHCPNetlink(t)
	t.Cleanup(func() { setDHCPNTPServers(nil) })
	var timers fakeAfterFunc
	timers.install(t)
	d := NewDHCPMode()
	eth0 := dummyLink("eth0", 7)
	l := lease24("10.0.2.15", "10.0.2.2")
	l.Expire = time.Now().Add(time.Hour)
	d.onBound(eth0, "eth0", make(chan string, 1))(l)
	d.onExpire(eth0, "eth0")(nil)
	if len(timers.timers) != 1 || timers.timers[0].Stop() || len(d.expiry) != 0 {
		t.Fatal("the expiry timer still runs after the DHCPNAK")
	}
}

// Teardown waits for a client restart that began before it, and the
// restart starts no client after the stop.
func TestTeardownWaitsForRestart(t *testing.T) {
	stubDHCPNetlink(t)
	d := NewDHCPMode()
	stopped := make(chan struct{})
	old := &fakeRestartClient{onStop: func() { time.Sleep(300 * time.Millisecond); close(stopped) }}
	var mu sync.Mutex
	var fresh []*fakeRestartClient
	start := func() dhcpClient {
		c := &fakeRestartClient{}
		mu.Lock()
		fresh = append(fresh, c)
		mu.Unlock()
		return c
	}
	d.clients = map[int]*nicClient{7: {client: old, start: start}}
	d.mu.Lock()
	go d.takeForRestart(7, "eth0")
	time.Sleep(50 * time.Millisecond) // the old client is stopping
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Teardown returned while the restart still stopped the old client")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, c := range fresh {
		if c.stops != 1 {
			t.Fatalf("a fresh client runs after Teardown (stops %d)", c.stops)
		}
	}
	if len(d.clients) != 0 {
		t.Fatalf("clients after Teardown: %v", d.clients)
	}
}

// A refused lease (no server identifier) restarts the client after a
// backoff of 2 s, doubled per refusal; a lease resets it.
func TestRefusedLeaseRestartsWithBackoff(t *testing.T) {
	stubDHCPNetlink(t)
	orig := dhcpAfterFunc
	t.Cleanup(func() { dhcpAfterFunc = orig })
	var waits []time.Duration
	var fire func()
	dhcpAfterFunc = func(d time.Duration, fn func()) *time.Timer {
		if d <= 64*time.Second { // not the 2 min expiry timer of the lease
			waits, fire = append(waits, d), fn
		}
		return time.NewTimer(time.Hour)
	}
	d := NewDHCPMode()
	link := dummyLink("eth0", 1)
	started := 0
	start := func() dhcpClient { started++; return &fakeRestartClient{} }
	old := &fakeRestartClient{}
	d.clients = map[int]*nicClient{1: {client: old, start: start}}
	refused := lease24("10.0.2.15", "10.0.2.2")
	refused.ServerID = nil
	bound := d.onBound(link, "eth0", make(chan string, 4))
	bound(refused)
	bound(refused) // a retry is pending: no second one
	fire()
	bound(refused)
	fire()
	bound(lease24("10.0.2.15", "10.0.2.2"))
	bound(refused)
	if want := []time.Duration{2 * time.Second, 4 * time.Second, 2 * time.Second}; !slices.Equal(waits, want) {
		t.Fatalf("retry waits = %v, want %v", waits, want)
	}
	if old.stops != 1 || started != 2 {
		t.Fatalf("old client stops %d, fresh clients %d, want 1 and 2", old.stops, started)
	}
}

// TestOnBoundWritesLeaseDNS: like systemd-resolved with UseDNS=yes, the DNS
// servers of every lease are used after the configured ones; the lease
// domain is a search domain, as in the legacy resolv.conf.
func TestOnBoundWritesLeaseDNS(t *testing.T) {
	stubDHCPNetlink(t)
	var written []string
	dhcpWriteResolvers = func(servers, search []string) error {
		written = append(written, strings.Join(servers, ",")+" search "+strings.Join(search, " "))
		return nil
	}

	d := NewDHCPMode()
	d.manageDNS, d.resolvers = true, []string{"10.9.9.9"}
	l0 := lease24("10.0.2.15", "10.0.2.2")
	l0.DNS = []net.IP{net.ParseIP("10.0.2.3")}
	l0.DomainName = "l2.example"
	l1 := lease24("10.0.3.15", "10.0.3.2")
	l1.DNS = []net.IP{net.ParseIP("10.0.3.3"), net.ParseIP("10.0.2.3")}
	d.onBound(dummyLink("eth1", 2), "eth1", make(chan string, 4))(l1)
	d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 4))(l0)
	d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 4))(l0) // renewal: no rewrite

	want := []string{"10.9.9.9,10.0.3.3,10.0.2.3 search ", "10.9.9.9,10.0.2.3,10.0.3.3 search l2.example"}
	if !slices.Equal(written, want) {
		t.Fatalf("resolv.conf writes = %q, want %q", written, want)
	}
}

func TestOnBoundKeepsProvisionerResolvConf(t *testing.T) {
	stubDHCPNetlink(t) // fails the test on any resolv.conf write
	d := NewDHCPMode()
	l := lease24("10.0.2.15", "10.0.2.2")
	l.DNS = []net.IP{net.ParseIP("10.0.2.3")}
	d.onBound(dummyLink("eth0", 1), "eth0", make(chan string, 1))(l)
}

// TestOnBoundSetsLeaseMTU: netplan writes UseMTU=true for dhcp4, so the
// lease MTU (option 26) is set; networkd raises values below 576 to 576 (and caps them at the NIC maximum).
// Teardown restores the MTU from before.
func TestOnBoundSetsLeaseMTU(t *testing.T) {
	stubDHCPNetlink(t)
	var mtus []int
	dhcpSetMTU = func(_ netlink.Link, mtu int) error { mtus = append(mtus, mtu); return nil }

	d := NewDHCPMode()
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 1, MTU: 1500}}
	bound := d.onBound(link, "eth0", make(chan string, 4))
	l := lease24("10.0.2.15", "10.0.2.2")
	l.MTU = 9000
	bound(l)
	bound(l) // renewal: no change
	small := lease24("10.0.2.15", "10.0.2.2")
	small.MTU = 500
	bound(small) // raised to the minimum, 576
	if want := []int{9000, 576}; !slices.Equal(mtus, want) {
		t.Fatalf("MTUs set = %v, want %v", mtus, want)
	}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	if want := []int{9000, 576, 1500}; !slices.Equal(mtus, want) {
		t.Fatalf("MTUs set = %v, want %v (Teardown restores 1500)", mtus, want)
	}
}

func TestSetupWritesConfiguredResolversUnlessKept(t *testing.T) {
	stubDHCPNetlink(t)
	t.Cleanup(snapshotNICFns())
	linkList = func() ([]netlink.Link, error) { return nil, nil }
	sleepFunc = func(time.Duration) {}
	var written []string
	dhcpWriteResolvers = func(servers, _ []string) error { written = append(written, strings.Join(servers, ",")); return nil }

	_ = (&DHCPMode{}).Setup(context.Background(), &Config{DNSResolvers: "10.9.9.9, 10.9.9.8"})
	_ = (&DHCPMode{}).Setup(context.Background(), &Config{DNSResolvers: "10.9.9.9", KeepResolvConf: true})
	if want := []string{"10.9.9.9,10.9.9.8"}; !slices.Equal(written, want) {
		t.Fatalf("resolv.conf writes = %q, want %q", written, want)
	}
}

// The end of a lease also takes its DNS servers out of resolv.conf and
// restores the MTU of its NIC.
func TestEndLeaseDropsDNSAndMTU(t *testing.T) {
	stubDHCPNetlink(t)
	var written []string
	dhcpWriteResolvers = func(servers, _ []string) error { written = append(written, strings.Join(servers, ",")); return nil }
	var mtus []int
	dhcpSetMTU = func(_ netlink.Link, mtu int) error { mtus = append(mtus, mtu); return nil }
	d := NewDHCPMode()
	d.manageDNS, d.resolvers = true, []string{"10.9.9.9"}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 7, MTU: 1500}}
	l := lease24("10.0.2.15", "10.0.2.2")
	l.DNS, l.MTU = []net.IP{net.ParseIP("10.0.2.3")}, 9000
	d.onBound(link, "eth0", make(chan string, 1))(l)
	d.onExpire(link, "eth0")(nil)
	if want := []string{"10.9.9.9,10.0.2.3", "10.9.9.9"}; !slices.Equal(written, want) {
		t.Fatalf("resolv.conf writes = %q, want %q", written, want)
	}
	if want := []int{9000, 1500}; !slices.Equal(mtus, want) {
		t.Fatalf("MTUs = %v, want %v", mtus, want)
	}
}

func TestClampMTU(t *testing.T) {
	for _, tt := range []struct {
		mtu, linkMax int
		ipv6         bool
		want         int
	}{
		{500, 0, false, 576}, {500, 0, true, 1280}, {9000, 1500, false, 1500},
		{9000, 0, false, 9000}, {1400, 9216, true, 1400}, {576, 576, true, 576},
	} {
		if got := clampMTU(tt.mtu, tt.linkMax, tt.ipv6); got != tt.want {
			t.Errorf("clampMTU(%d, %d, %v) = %d, want %d", tt.mtu, tt.linkMax, tt.ipv6, got, tt.want)
		}
	}
}

// A lease without option 26, or with the MTU the NIC has, sets nothing, and
// Teardown restores nothing.
func TestLeaseMTUUnchangedRestoresNothing(t *testing.T) {
	stubDHCPNetlink(t)
	var mtus []int
	dhcpSetMTU = func(_ netlink.Link, mtu int) error { mtus = append(mtus, mtu); return nil }
	d := NewDHCPMode()
	eth0 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 1, MTU: 1500}}
	eth1 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1", Index: 2, MTU: 1500}}
	same := lease24("10.0.2.15", "10.0.2.2")
	same.MTU = 1500
	d.onBound(eth0, "eth0", make(chan string, 1))(same)
	d.onBound(eth1, "eth1", make(chan string, 1))(lease24("10.0.3.15", "10.0.3.2"))
	d.onExpire(eth0, "eth0")(nil)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mtus) != 0 {
		t.Fatalf("MTUs set = %v, want none", mtus)
	}
}

// Only IPv4 servers that are not 0.0.0.0 go to resolv.conf.
func TestLeaseDNSFilter(t *testing.T) {
	l := lease24("10.0.2.15")
	l.DNS = []net.IP{net.IPv4zero, net.ParseIP("10.0.2.3"), net.ParseIP("fd00::1")}
	if got := leaseDNS(l); !slices.Equal(got, []string{"10.0.2.3"}) {
		t.Fatalf("leaseDNS = %v", got)
	}
}

// When the last lease ends, its DNS servers leave resolv.conf.
func TestEndLeaseRemovesLastDNSServers(t *testing.T) {
	stubDHCPNetlink(t)
	var written []string
	dhcpWriteResolvers = func(servers, _ []string) error { written = append(written, strings.Join(servers, ",")); return nil }
	d := NewDHCPMode()
	d.manageDNS = true
	link := dummyLink("eth0", 1)
	l := lease24("10.0.2.15", "10.0.2.2")
	l.DNS = []net.IP{net.ParseIP("10.0.2.3")}
	d.onBound(link, "eth0", make(chan string, 1))(l)
	d.onExpire(link, "eth0")(nil)
	if want := []string{"10.0.2.3", ""}; !slices.Equal(written, want) {
		t.Fatalf("resolv.conf writes = %q, want %q", written, want)
	}
}

// A bad dns_resolver fails Setup, as it failed the run before.
func TestSetupRejectsBadResolver(t *testing.T) {
	stubDHCPNetlink(t)
	err := NewDHCPMode().Setup(context.Background(), &Config{DNSResolvers: "8.8.8.8\nsearch evil.example"})
	if err == nil || !strings.Contains(err.Error(), "DNS resolvers") {
		t.Fatalf("Setup() = %v, want the resolver error", err)
	}
}

// Option 15 with one trailing NUL is used without it, like networkd.
func TestLeaseDomainTrailingNul(t *testing.T) {
	if got := leaseDomain(" l2.example\x00"); got != "l2.example" {
		t.Fatalf("leaseDomain = %q", got)
	}
}

func TestValidSearchDomainLikeSystemd(t *testing.T) {
	for d, want := range map[string]bool{
		"l2.example": true, "l2.example.": true, "a-b.example": true,
		"a..b": false, "-a.example": false, "a-.example": false, "localhost": false, "foo.localhost": false,
		"corp_lan.example": false, strings.Repeat("a", 64) + ".example": false,
	} {
		if got := validSearchDomain(d); got != want {
			t.Errorf("validSearchDomain(%q) = %v, want %v", d, got, want)
		}
	}
}

// A failed MTU restore at the end of a lease stays for Teardown.
func TestEndLeaseKeepsFailedMTURestore(t *testing.T) {
	stubDHCPNetlink(t)
	var mtus []int
	fail := true
	dhcpSetMTU = func(_ netlink.Link, mtu int) error {
		mtus = append(mtus, mtu)
		if mtu == 1500 && fail {
			fail = false
			return errors.New("device busy")
		}
		return nil
	}
	d := NewDHCPMode()
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 7, MTU: 1500}}
	l := lease24("10.0.2.15", "10.0.2.2")
	l.MTU = 9000
	d.onBound(link, "eth0", make(chan string, 1))(l)
	d.onExpire(link, "eth0")(nil)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []int{9000, 1500, 1500}; !slices.Equal(mtus, want) {
		t.Fatalf("MTUs = %v, want %v (Teardown retries the restore)", mtus, want)
	}
}

// Option 121 replaces the router of option 3 (RFC 3442, networkd); an
// on-link destination gets a link-scope route.
func TestOnBoundAppliesClasslessRoutes(t *testing.T) {
	stubDHCPNetlink(t)
	var added []string
	dhcpRouteAdd = func(r *netlink.Route) error {
		added = append(added, fmt.Sprintf("%s via %v scope %d", routeDst(r), r.Gw, r.Scope))
		return nil
	}
	l := lease24("10.0.2.15", "10.0.2.99")
	// 0.0.0.0/0 via 10.0.2.1, 10.20.0.0/16 via 10.0.2.2, 10.0.9.0/24 on link.
	l.OtherOptions = []dhclient.Option{{Type: dhcpOptClasslessRoutes, Data: []byte{
		0, 10, 0, 2, 1,
		16, 10, 20, 10, 0, 2, 2,
		24, 10, 0, 9, 0, 0, 0, 0,
	}}}
	NewDHCPMode().onBound(dummyLink("eth0", 1), "eth0", make(chan string, 1))(l)
	want := []string{
		"10.0.2.1/32 via <nil> scope 253", "10.0.2.2/32 via <nil> scope 253",
		"default via 10.0.2.1 scope 0", "10.20.0.0/16 via 10.0.2.2 scope 0", "10.0.9.0/24 via <nil> scope 253",
	}
	if !slices.Equal(added, want) {
		t.Fatalf("routes = %q, want %q", added, want)
	}
	if _, ok := classlessRoutes([]byte{33, 1, 2, 3, 4, 5, 6, 7, 8}); ok {
		t.Fatal("a width over 32 must make the option invalid")
	}
	if _, ok := classlessRoutes([]byte{24, 10, 0}); ok {
		t.Fatal("a truncated option must be invalid")
	}
}

func TestLeaseSearchDomains(t *testing.T) {
	// "a.example" then "b.example" with a pointer to "example" (RFC 3397).
	data := []byte{1, 'a', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0, 1, 'b', 0xc0, 2}
	l := &dhclient.Lease{DomainName: "l2.example", OtherOptions: []dhclient.Option{{Type: dhcpOptDomainSearch, Data: data}}}
	if got, want := leaseSearchDomains(l), []string{"l2.example", "a.example", "b.example"}; !slices.Equal(got, want) {
		t.Fatalf("search domains = %q, want %q", got, want)
	}
	if got := domainSearch([]byte{0xc0, 0}); got != nil {
		t.Fatalf("a pointer loop gave %q", got)
	}
}

// leaseRoutes cases: null routers (networkd-dhcp4.c), option 33 without
// 121, masked option 121 destinations, host routes for localhost and the
// lease address.
func TestLeaseRoutesTable(t *testing.T) {
	link := dummyLink("eth0", 1)
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP("10.0.2.15").To4(), Mask: net.CIDRMask(24, 32)}}
	show := func(rs []*netlink.Route) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, fmt.Sprintf("%s via %v scope %d src %v", routeDst(r), r.Gw, r.Scope, r.Src))
		}
		return out
	}
	opt := func(typ layers.DHCPOpt, data ...byte) []dhclient.Option {
		return []dhclient.Option{{Type: typ, Data: data}}
	}
	for _, tt := range []struct {
		name    string
		routers []string
		opts    []dhclient.Option
		want    []string
	}{
		{"first router not null", []string{"0.0.0.0", "10.0.2.2"}, nil,
			[]string{"10.0.2.2/32 via <nil> scope 253 src 10.0.2.15", "default via 10.0.2.2 scope 0 src 10.0.2.15"}},
		{"all routers null", []string{"0.0.0.0"}, nil, nil},
		{"option 33 and router", []string{"10.0.2.2"}, opt(dhcpOptStaticRoutes, 10, 30, 1, 0, 10, 0, 2, 5, 0, 0, 0, 0, 10, 0, 2, 6, 224, 0, 0, 1, 10, 0, 2, 7),
			[]string{"10.0.2.5/32 via <nil> scope 253 src 10.0.2.15", "10.0.2.2/32 via <nil> scope 253 src 10.0.2.15",
				"10.0.0.0/8 via 10.0.2.5 scope 0 src 10.0.2.15", "default via 10.0.2.2 scope 0 src 10.0.2.15"}},
		{"option 121 masked, host routes", []string{"10.0.2.99"}, append(opt(dhcpOptStaticRoutes, 10, 30, 1, 0, 10, 0, 2, 5),
			opt(dhcpOptClasslessRoutes, 12, 10, 47, 10, 0, 2, 2, 32, 127, 0, 0, 1, 0, 0, 0, 0, 32, 10, 0, 2, 15, 10, 0, 2, 2)...),
			[]string{"10.0.2.2/32 via <nil> scope 253 src 10.0.2.15", "10.32.0.0/12 via 10.0.2.2 scope 0 src 10.0.2.15",
				"127.0.0.1/32 via <nil> scope 254 src <nil>", "10.0.2.15/32 via <nil> scope 254 src 10.0.2.15"}},
	} {
		l := lease24("10.0.2.15", tt.routers...)
		l.OtherOptions = tt.opts
		if got := show(leaseRoutes(link, addr, l)); !slices.Equal(got, tt.want) {
			t.Errorf("%s: routes =\n%q\nwant\n%q", tt.name, got, tt.want)
		}
	}
}

func FuzzClasslessRoutes(f *testing.F) {
	f.Add([]byte{0, 10, 0, 2, 1, 16, 10, 20, 10, 0, 2, 2})
	f.Add([]byte{33, 1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{24, 10, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		routes, _ := classlessRoutes(data)
		for _, r := range routes {
			ones, bits := r.dst.Mask.Size()
			if bits != 32 || ones > 32 || !r.dst.IP.Equal(r.dst.IP.Mask(r.dst.Mask)) || len(r.gw) != 4 {
				t.Fatalf("bad route %v via %v", r.dst, r.gw)
			}
		}
		_ = staticRoutes(data)
	})
}

func FuzzDomainSearch(f *testing.F) {
	f.Add([]byte{1, 'a', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0, 1, 'b', 0xc0, 2})
	f.Add([]byte{0xc0, 0})
	f.Add([]byte{3, 'c', 'o', 'm', 0, 1, 'x', 0xc0, 0, 1, 'y', 0xc0, 5})
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, d := range domainSearch(data) {
			if len(d) > 253 {
				t.Fatalf("name of %d bytes", len(d))
			}
		}
	})
}

// Malformed options 33 and 119 are ignored as a whole, like networkd.
func TestMalformedRouteAndSearchOptions(t *testing.T) {
	if got := staticRoutes([]byte{10, 30, 1, 0, 10, 0, 2, 5, 1}); got != nil {
		t.Errorf("option 33 of 9 bytes = %v, want ignored", got)
	}
	ok := []byte{1, 'a', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0}
	for name, data := range map[string][]byte{
		"forward pointer": append(append([]byte(nil), ok...), 1, 'b', 0xc0, 20),
		"label type 01":   append(append([]byte(nil), ok...), 0x41, 'b', 0),
		"label type 10":   append(append([]byte(nil), ok...), 0x81, 'b', 0),
		"truncated":       append(append([]byte(nil), ok...), 5, 'b'),
		// A forward pointer onto a valid name in the data, and reserved
		// label types with enough bytes after them for a label.
		"forward pointer onto a name": {1, 'a', 0xc0, 4, 1, 'b', 0},
		"label type 01, long":         append(append([]byte{0x41}, bytes.Repeat([]byte{'x'}, 0x41)...), 0),
		"label type 10, long":         append(append([]byte{0x81}, bytes.Repeat([]byte{'x'}, 0x81)...), 0),
	} {
		if got := domainSearch(data); got != nil {
			t.Errorf("%s: domainSearch = %q, want the option ignored", name, got)
		}
	}
	if got := domainSearch(ok); !slices.Equal(got, []string{"a.example"}) {
		t.Errorf("domainSearch = %q", got)
	}
}

// Without a machine ID the client identifier uses a random ID of the run:
// stable within the run.
func TestClientIDWithoutMachineID(t *testing.T) {
	a, b := dhcpClientID("eth0", ""), dhcpClientID("eth0", "")
	if !bytes.Equal(a, b) || !bytes.Equal(a, dhcpClientID("eth0", fallbackMachineID())) ||
		bytes.Equal(a, dhcpClientID("eth0", "x")) || fallbackMachineID() == "" {
		t.Fatalf("client IDs %x %x", a, b)
	}
}

// A NIC that appears after Setup gets a client, like networkd matches a
// link that udev announces later; after Teardown no new client starts.
func TestSetupStartsClientsOnNewNICs(t *testing.T) {
	clients := stubDHCPSetup(t, func(name string, cb dhclient.Callback) {
		if name == "eth0" {
			cb(lease24("10.0.2.15", "10.0.2.2"))
		}
	})
	var mu sync.Mutex
	lookups := 0
	nics := make([]net.Interface, 0, 3)
	nics = append(nics, net.Interface{Name: "eth0", Index: 1}, net.Interface{Name: "eth1", Index: 2})
	dhcpInterfaces = func(context.Context) ([]net.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		lookups++
		return slices.Clone(nics), nil
	}
	updates := make(chan chan<- netlink.LinkUpdate, 1)
	dhcpSubscribe = func(ch chan<- netlink.LinkUpdate, done <-chan struct{}, _ netlink.LinkSubscribeOptions) error {
		updates <- ch
		go func() { <-done; close(ch) }()
		return nil
	}
	d := NewDHCPMode()
	if err := d.Setup(context.Background(), &Config{}); err != nil {
		t.Fatal(err)
	}
	ch := <-updates
	mu.Lock()
	nics = append(nics, net.Interface{Name: "eth3", Index: 4})
	mu.Unlock()
	newLink := func(idx int, name string) netlink.LinkUpdate {
		u := netlink.LinkUpdate{Link: dummyLink(name, idx)}
		u.Header.Type = unix.RTM_NEWLINK
		return u
	}
	ch <- newLink(1, "eth0")  // known: no second client
	ch <- newLink(9, "veth9") // no NIC: looked up once
	ch <- newLink(9, "veth9")
	ch <- newLink(4, "eth3")
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		_, ok := d.clients[4]
		d.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no client started for the new NIC eth3")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(clients) != 3 {
		t.Fatalf("clients = %d, want eth0, eth1 and eth3", len(clients))
	}
	stoppedAll(t, clients, 1)
	mu.Lock()
	defer mu.Unlock()
	if lookups != 1+1+1 { // Setup, veth9 once, eth3
		t.Fatalf("NIC lookups = %d, want 3 (veth9 only once)", lookups)
	}
}

// A client that the link watch starts while a restart runs (the MTU
// restore sends RTM_NEWLINK) and the restarted one: only one stays, the
// other is stopped, so none leaks past Teardown.
func TestRestartAndLinkWatchKeepOneClient(t *testing.T) {
	stubDHCPNetlink(t)
	d := NewDHCPMode()
	watch := &fakeRestartClient{}
	var fresh *fakeRestartClient
	start := func() dhcpClient {
		// The watch puts its client in the slot while the restart starts.
		d.mu.Lock()
		d.clients = map[int]*nicClient{7: {client: watch}}
		d.mu.Unlock()
		fresh = &fakeRestartClient{}
		return fresh
	}
	d.restartClient(7, "eth0", &nicClient{client: &fakeRestartClient{}, start: start})
	if fresh.stops != 1 || d.clients[7].client != watch {
		t.Fatalf("fresh stops %d, slot %+v: want the fresh client stopped and the watch client kept", fresh.stops, d.clients[7])
	}
}

// startClient leaves a client that is already in the slot and stops its own.
func TestStartClientKeepsTakenSlot(t *testing.T) {
	clients := stubDHCPSetup(t, nil)
	d := NewDHCPMode()
	kept := &fakeRestartClient{}
	d.clients = map[int]*nicClient{1: {client: kept}}
	d.startClient(&net.Interface{Name: "eth0", Index: 1}, "", make(chan string, 1))
	d.startClient(&net.Interface{Name: "eth1", Index: 2}, "", make(chan string, 1))
	if d.clients[1].client != kept || clients["eth0"].stopped != 1 {
		t.Fatalf("slot %+v, own client stops %d: want the slot kept and the own client stopped", d.clients[1], clients["eth0"].stopped)
	}
}

// A link that is no NIC is looked up once; after RTM_DELLINK, or under a
// new name, it is looked up again.
func TestLinkUpdateNotNICCache(t *testing.T) {
	stubDHCPSetup(t, nil)
	lookups := 0
	dhcpInterfaces = func(context.Context) ([]net.Interface, error) {
		lookups++
		return []net.Interface{{Name: "eth0", Index: 1}, {Name: "eth1", Index: 2}}, nil
	}
	d := NewDHCPMode()
	d.startClient(&net.Interface{Name: "eth0", Index: 1}, "", make(chan string, 1))
	d.startClient(&net.Interface{Name: "eth1", Index: 2}, "", make(chan string, 1))
	update := func(typ uint16, idx int, name string) netlink.LinkUpdate {
		u := netlink.LinkUpdate{Link: dummyLink(name, idx)}
		u.Header.Type = typ
		return u
	}
	notNIC := map[linkKey]bool{}
	for _, u := range []netlink.LinkUpdate{
		update(unix.RTM_NEWLINK, 9, "veth9"), update(unix.RTM_NEWLINK, 9, "veth9"), // one lookup
		update(unix.RTM_NEWLINK, 9, "veth9x"),                                        // renamed: again
		update(unix.RTM_DELLINK, 9, "veth9x"), update(unix.RTM_NEWLINK, 9, "veth9x"), // gone and back: again
	} {
		d.linkUpdate(u, notNIC, "", make(chan string, 1))
	}
	if lookups != 3 {
		t.Fatalf("NIC lookups = %d, want 3", lookups)
	}
}

// startClient after the stop keeps no client.
func TestStartClientAfterStop(t *testing.T) {
	clients := stubDHCPSetup(t, nil)
	d := NewDHCPMode()
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	d.startClient(&net.Interface{Name: "eth0", Index: 1}, "", make(chan string, 1))
	d.startClient(&net.Interface{Name: "eth1", Index: 2}, "", make(chan string, 1))
	if len(d.clients) != 0 || clients["eth0"].stopped != 1 {
		t.Fatalf("clients %v, own client stops %d: want none kept and it stopped", d.clients, clients["eth0"].stopped)
	}
}
