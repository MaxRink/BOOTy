//go:build linux

package network

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/digineo/go-dhclient"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
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
	if d.client != nil {
		t.Fatalf("Setup() kept a DHCP client after error %v", err)
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
	if d.client != nil {
		t.Fatal("Setup() kept a DHCP client after cancellation")
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

func TestNewDHCPClientRequestsDefaultParams(t *testing.T) {
	for _, hostname := range []string{"(none)", ""} {
		client := newDHCPClient(&net.Interface{Name: "eth0"}, hostname, nil)

		var params []byte
		var gotHostname []byte
		for _, opt := range client.DHCPOptions {
			switch opt.Type {
			case layers.DHCPOptParamsRequest:
				params = opt.Data
			case layers.DHCPOptHostname:
				gotHostname = opt.Data
			}
		}
		for _, want := range []layers.DHCPOpt{layers.DHCPOptSubnetMask, layers.DHCPOptRouter, layers.DHCPOptDNS} {
			if !bytes.Contains(params, []byte{byte(want)}) {
				t.Errorf("hostname %q: parameter request list %v misses %s", hostname, params, want)
			}
		}
		if string(gotHostname) != hostname {
			t.Errorf("hostname option = %q, want %q", gotHostname, hostname)
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

func TestOnBoundReplacesStateAndTeardownRemovesIt(t *testing.T) {
	origAddr, origAddrDel, origRoute, origRouteDel := dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace, dhcpRouteDel
	t.Cleanup(func() {
		dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace, dhcpRouteDel = origAddr, origAddrDel, origRoute, origRouteDel
	})
	var addrs, delAddrs []*netlink.Addr
	var routes, delRoutes []*netlink.Route
	dhcpAddrReplace = func(_ netlink.Link, a *netlink.Addr) error { addrs = append(addrs, a); return nil }
	dhcpAddrDel = func(_ netlink.Link, a *netlink.Addr) error { delAddrs = append(delAddrs, a); return nil }
	dhcpRouteReplace = func(r *netlink.Route) error { routes = append(routes, r); return nil }
	dhcpRouteDel = func(r *netlink.Route) error { delRoutes = append(delRoutes, r); return nil }

	d := NewDHCPMode()
	leased := make(chan struct{}, 1)
	var winner dhcpWinner
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 7}}
	d.onBound(link, "eth0", leased, &winner)(&dhclient.Lease{
		FixedAddress: net.ParseIP("10.0.2.15"),
		Netmask:      net.CIDRMask(24, 32),
		Router:       []net.IP{net.ParseIP("10.0.2.2")},
	})

	select {
	case <-leased:
	default:
		t.Fatal("lease was not reported as bound")
	}
	if len(addrs) != 1 || addrs[0].IPNet.String() != "10.0.2.15/24" {
		t.Fatalf("addresses = %v, want 10.0.2.15/24", addrs)
	}
	if len(routes) != 1 || routes[0].LinkIndex != 7 || !routes[0].Gw.Equal(net.ParseIP("10.0.2.2")) {
		t.Fatalf("default route = %v, want via 10.0.2.2 on link 7", routes)
	}

	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	if len(delAddrs) != 1 || len(delRoutes) != 1 {
		t.Fatalf("Teardown removed addrs=%v routes=%v, want the lease address and route", delAddrs, delRoutes)
	}
}

func TestOnBoundInvalidLeaseDoesNotClaimWinner(t *testing.T) {
	d := NewDHCPMode()
	var winner dhcpWinner
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}
	d.onBound(link, "eth0", make(chan struct{}, 1), &winner)(&dhclient.Lease{})
	if winner.won {
		t.Fatal("an invalid lease claimed the winner flag")
	}
}

func TestOnBoundFailedApplyLeavesWinnerForOtherNIC(t *testing.T) {
	origAddr, origRoute := dhcpAddrReplace, dhcpRouteReplace
	t.Cleanup(func() { dhcpAddrReplace, dhcpRouteReplace = origAddr, origRoute })
	dhcpRouteReplace = func(*netlink.Route) error { return nil }
	dhcpAddrReplace = func(link netlink.Link, _ *netlink.Addr) error {
		if link.Attrs().Name == "eth0" {
			return syscall.EPERM
		}
		return nil
	}

	d := NewDHCPMode()
	var winner dhcpWinner
	lease := &dhclient.Lease{FixedAddress: net.ParseIP("10.0.2.15"), Netmask: net.CIDRMask(24, 32)}
	leased0, leased1 := make(chan struct{}, 1), make(chan struct{}, 1)
	d.onBound(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}, "eth0", leased0, &winner)(lease)
	d.onBound(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1"}}, "eth1", leased1, &winner)(lease)

	select {
	case <-leased1:
	default:
		t.Fatal("lease on eth1 was not used after eth0 failed to apply")
	}
}

func TestOnBoundRouteFailureLeavesWinnerForOtherNIC(t *testing.T) {
	origAddr, origAddrDel, origRoute := dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace
	t.Cleanup(func() { dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace = origAddr, origAddrDel, origRoute })
	dhcpAddrReplace = func(netlink.Link, *netlink.Addr) error { return nil }
	var rolledBack []string
	dhcpAddrDel = func(link netlink.Link, _ *netlink.Addr) error {
		rolledBack = append(rolledBack, link.Attrs().Name)
		return nil
	}
	dhcpRouteReplace = func(r *netlink.Route) error {
		if r.LinkIndex == 1 {
			return syscall.ENETUNREACH
		}
		return nil
	}

	d := NewDHCPMode()
	var winner dhcpWinner
	lease := &dhclient.Lease{
		FixedAddress: net.ParseIP("10.0.2.15"),
		Netmask:      net.CIDRMask(24, 32),
		Router:       []net.IP{net.ParseIP("10.0.2.2")},
	}
	leased0, leased1 := make(chan struct{}, 1), make(chan struct{}, 1)
	d.onBound(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 1}}, "eth0", leased0, &winner)(lease)
	d.onBound(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1", Index: 2}}, "eth1", leased1, &winner)(lease)

	select {
	case <-leased0:
		t.Fatal("lease on eth0 was reported although its default route failed")
	default:
	}
	select {
	case <-leased1:
	default:
		t.Fatal("lease on eth1 was not used after eth0 failed to set its default route")
	}
	if len(rolledBack) != 1 || rolledBack[0] != "eth0" {
		t.Fatalf("rolled back addresses on %v, want eth0", rolledBack)
	}
}

func TestOnBoundAppliesRenewalOfWinningNIC(t *testing.T) {
	origAddr, origAddrDel, origRoute := dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace
	t.Cleanup(func() { dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace = origAddr, origAddrDel, origRoute })
	var set, deleted []string
	dhcpAddrReplace = func(link netlink.Link, a *netlink.Addr) error {
		set = append(set, link.Attrs().Name+" "+a.IPNet.String())
		return nil
	}
	dhcpAddrDel = func(link netlink.Link, a *netlink.Addr) error {
		deleted = append(deleted, link.Attrs().Name+" "+a.IPNet.String())
		return nil
	}
	dhcpRouteReplace = func(*netlink.Route) error { return nil }

	d := NewDHCPMode()
	var winner dhcpWinner
	eth0 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 1}}
	eth1 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1", Index: 2}}
	bound0 := d.onBound(eth0, "eth0", make(chan struct{}, 1), &winner)
	bound1 := d.onBound(eth1, "eth1", make(chan struct{}, 1), &winner)
	lease := func(ip string) *dhclient.Lease {
		return &dhclient.Lease{FixedAddress: net.ParseIP(ip), Netmask: net.CIDRMask(24, 32)}
	}

	bound0(lease("10.0.2.15"))
	bound1(lease("10.0.3.15"))
	bound0(lease("10.0.2.16"))

	if want := []string{"eth0 10.0.2.15/24", "eth0 10.0.2.16/24"}; !slices.Equal(set, want) {
		t.Fatalf("addresses set = %v, want %v", set, want)
	}
	if want := []string{"eth0 10.0.2.15/24"}; !slices.Equal(deleted, want) {
		t.Fatalf("addresses deleted = %v, want %v", deleted, want)
	}
	if len(d.applied) != 1 || d.applied[0].addr.IPNet.String() != "10.0.2.16/24" {
		t.Fatalf("applied state = %v, want only 10.0.2.16/24", d.applied)
	}
}

func TestOnBoundRenewalRouteFailureKeepsPreviousLease(t *testing.T) {
	origAddr, origAddrDel, origRoute := dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace
	t.Cleanup(func() { dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace = origAddr, origAddrDel, origRoute })
	var deleted []string
	dhcpAddrReplace = func(netlink.Link, *netlink.Addr) error { return nil }
	dhcpAddrDel = func(_ netlink.Link, a *netlink.Addr) error {
		deleted = append(deleted, a.IPNet.String())
		return nil
	}
	failRoute := false
	dhcpRouteReplace = func(*netlink.Route) error {
		if failRoute {
			return syscall.ENETUNREACH
		}
		return nil
	}

	d := NewDHCPMode()
	var winner dhcpWinner
	eth0 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 1}}
	bound := d.onBound(eth0, "eth0", make(chan struct{}, 1), &winner)
	lease := func(ip string) *dhclient.Lease {
		return &dhclient.Lease{FixedAddress: net.ParseIP(ip), Netmask: net.CIDRMask(24, 32), Router: []net.IP{net.ParseIP("10.0.2.2")}}
	}

	bound(lease("10.0.2.15"))
	failRoute = true
	bound(lease("10.0.2.16"))

	if want := []string{"10.0.2.16/24"}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted = %v, want only the new address rolled back", deleted)
	}
	if len(d.applied) != 1 || d.applied[0].addr.IPNet.String() != "10.0.2.15/24" {
		t.Fatalf("applied state = %v, want the previous lease 10.0.2.15/24", d.applied)
	}
}

func TestOnBoundRenewalDropsRouteAndRestoresMask(t *testing.T) {
	origAddr, origAddrDel, origRoute, origRouteDel := dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace, dhcpRouteDel
	t.Cleanup(func() {
		dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace, dhcpRouteDel = origAddr, origAddrDel, origRoute, origRouteDel
	})
	var routeDels int
	dhcpRouteDel = func(*netlink.Route) error { routeDels++; return nil }
	var set []string
	dhcpAddrReplace = func(_ netlink.Link, a *netlink.Addr) error {
		set = append(set, a.IPNet.String())
		return nil
	}
	dhcpAddrDel = func(netlink.Link, *netlink.Addr) error { return nil }
	failRoute := false
	dhcpRouteReplace = func(*netlink.Route) error {
		if failRoute {
			return syscall.ENETUNREACH
		}
		return nil
	}

	d := NewDHCPMode()
	var winner dhcpWinner
	eth0 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 1}}
	bound := d.onBound(eth0, "eth0", make(chan struct{}, 1), &winner)
	gw := []net.IP{net.ParseIP("10.0.2.2")}

	bound(&dhclient.Lease{FixedAddress: net.ParseIP("10.0.2.15"), Netmask: net.CIDRMask(24, 32), Router: gw})
	// Renewal without a router option removes the old default route.
	bound(&dhclient.Lease{FixedAddress: net.ParseIP("10.0.2.15"), Netmask: net.CIDRMask(24, 32)})
	if routeDels != 1 || len(d.applied) != 1 || d.applied[0].route != nil {
		t.Fatalf("route deletes = %d, applied state = %v, want the old default route removed", routeDels, d.applied)
	}

	// Renewal with a new mask whose route fails restores the old mask.
	failRoute = true
	bound(&dhclient.Lease{FixedAddress: net.ParseIP("10.0.2.15"), Netmask: net.CIDRMask(16, 32), Router: gw})
	if want := []string{"10.0.2.15/24", "10.0.2.15/24", "10.0.2.15/16", "10.0.2.15/24"}; !slices.Equal(set, want) {
		t.Fatalf("addresses set = %v, want %v", set, want)
	}
}

func TestOnBoundRenewalTracksAddressWhenRollbackFails(t *testing.T) {
	origAddr, origAddrDel, origRoute := dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace
	t.Cleanup(func() { dhcpAddrReplace, dhcpAddrDel, dhcpRouteReplace = origAddr, origAddrDel, origRoute })
	dhcpAddrReplace = func(netlink.Link, *netlink.Addr) error { return nil }
	dhcpAddrDel = func(netlink.Link, *netlink.Addr) error { return syscall.EBUSY }
	failRoute := false
	dhcpRouteReplace = func(*netlink.Route) error {
		if failRoute {
			return syscall.ENETUNREACH
		}
		return nil
	}

	d := NewDHCPMode()
	var winner dhcpWinner
	eth0 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 1}}
	bound := d.onBound(eth0, "eth0", make(chan struct{}, 1), &winner)
	gw := []net.IP{net.ParseIP("10.0.2.2")}
	bound(&dhclient.Lease{FixedAddress: net.ParseIP("10.0.2.15"), Netmask: net.CIDRMask(24, 32), Router: gw})
	failRoute = true
	bound(&dhclient.Lease{FixedAddress: net.ParseIP("10.0.2.16"), Netmask: net.CIDRMask(24, 32), Router: gw})

	found := false
	for _, a := range d.applied {
		if a.addr.IPNet.String() == "10.0.2.16/24" {
			found = true
		}
	}
	if !found {
		t.Fatalf("applied state = %v, want the address whose rollback failed tracked for Teardown", d.applied)
	}
}
