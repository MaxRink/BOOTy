//go:build linux

package network

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/network/dhcp6"
	"github.com/telekom/BOOTy/pkg/network/lldp"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

type dhcp6Stub struct {
	mu      sync.Mutex
	ra      func(managed, other bool)
	clients []*dhcp6.Client
	fakes   []*fakeDHCPClient
	added   []*netlink.Addr
	deleted []string
	written []string
	rs      int
}

func stubDHCPv6(t *testing.T) *dhcp6Stub {
	t.Helper()
	stubDHCPNetlink(t)
	s := &dhcp6Stub{}
	origRA, origNew, origAdd, origDel := dhcp6WatchRA, dhcp6New, dhcp6AddrAdd, dhcp6AddrDel
	origRS, origWait := dhcp6SendRS, dhcp6RSWait
	t.Cleanup(func() {
		dhcp6WatchRA, dhcp6New, dhcp6AddrAdd, dhcp6AddrDel = origRA, origNew, origAdd, origDel
		dhcp6SendRS, dhcp6RSWait = origRS, origWait
	})
	dhcp6RSWait = 20 * time.Millisecond
	dhcp6SendRS = func(string, net.HardwareAddr) error { s.mu.Lock(); s.rs++; s.mu.Unlock(); return nil }
	dhcp6WatchRA = func(ctx context.Context, _ *net.Interface, fn func(bool, bool)) error {
		s.ra = fn
		go func() { <-ctx.Done(); s.mu.Lock(); s.ra = nil; s.mu.Unlock() }()
		return nil
	}
	dhcp6New = func(c *dhcp6.Client) dhcp6Client {
		f := &fakeDHCPClient{}
		s.mu.Lock()
		s.clients, s.fakes = append(s.clients, c), append(s.fakes, f)
		s.mu.Unlock()
		return f
	}
	dhcp6AddrAdd = func(_ netlink.Link, a *netlink.Addr) error { s.added = append(s.added, a); return nil }
	dhcp6AddrDel = func(_ netlink.Link, a *netlink.Addr) error {
		s.deleted = append(s.deleted, a.IPNet.String())
		return nil
	}
	dhcpWriteResolvers = func(servers, search []string) error {
		s.written = append(s.written, strings.Join(servers, ",")+" search "+strings.Join(search, " "))
		return nil
	}
	return s
}

func (s *dhcp6Stub) watching() func(bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ra
}

// TestDHCPv6FollowsRAAndCarrier: an RA with the M flag starts a stateful
// client with networkd's DUID and IAID (those of DHCPv4 option 61); its
// address goes on the NIC as /128 with the lifetimes and its DNS servers
// after the DHCPv4 ones. Carrier loss stops the client and drops both; the
// next RA starts a new one; Teardown stops it and the RA watch.
func TestDHCPv6FollowsRAAndCarrier(t *testing.T) {
	s := stubDHCPv6(t)
	d := NewDHCPMode()
	d.manageDNS = true
	link := dummyLink("eth0", 1)
	d.clients = map[int]*nicClient{1: {client: &fakeDHCPClient{}, link: link}}
	l4 := lease24("10.0.2.15", "10.0.2.2")
	l4.DNS = []net.IP{net.ParseIP("10.0.2.3")}
	d.onBound(link, "eth0", make(chan string, 4))(l4)

	iface := &net.Interface{Name: "eth0", Index: 1}
	d.mu.Lock()
	d.startRA(link, iface, "host-0")
	d.mu.Unlock()
	ra := s.watching()
	ra(false, false) // SLAAC only: no client
	if len(s.clients) != 0 {
		t.Fatal("client started without the M or O flag")
	}
	ra(true, true)
	ra(true, true) // a running client is not started again
	if len(s.clients) != 1 {
		t.Fatalf("%d clients, want 1", len(s.clients))
	}
	c := s.clients[0]
	id := dhcpClientID("eth0", lldp.MachineID())
	if !c.Stateful || !c.SendRelease || c.Hostname != "host-0" || !bytes.Equal(c.DUID, id[5:]) || c.IAID != binary.BigEndian.Uint32(id[1:5]) {
		t.Fatalf("client = %+v", c)
	}

	c.OnBound(&dhcp6.Lease{Addr: net.ParseIP("fd00:10::1a4"), Preferred: 120 * time.Second, Valid: 180 * time.Second,
		DNS: []net.IP{net.ParseIP("fd00:10::1"), net.ParseIP("10.0.2.3")}})
	if len(s.added) != 1 || s.added[0].IPNet.String() != "fd00:10::1a4/128" || s.added[0].ValidLft != 180 || s.added[0].PreferedLft != 120 {
		t.Fatalf("added = %v", s.added)
	}
	wantDNS := []string{"10.0.2.3 search ", "10.0.2.3,fd00:10::1 search "}
	if !slices.Equal(s.written, wantDNS) {
		t.Fatalf("resolv.conf = %q, want %q", s.written, wantDNS)
	}

	down := netlink.LinkUpdate{Link: link}
	down.Header.Type = unix.RTM_NEWLINK
	down.Flags = unix.IFF_UP
	d.carrierChange(down)
	if s.fakes[0].stopped != 1 || !slices.Equal(s.deleted, []string{"fd00:10::1a4/128"}) {
		t.Fatalf("carrier loss: stopped %d, deleted %v", s.fakes[0].stopped, s.deleted)
	}
	ra(true, false) // no carrier: no client
	up := down
	up.Flags = unix.IFF_UP | unix.IFF_LOWER_UP
	d.carrierChange(up)
	rs := func() int { s.mu.Lock(); defer s.mu.Unlock(); return s.rs }
	for deadline := time.Now().Add(5 * time.Second); rs() < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no router solicitations after the carrier came back")
		}
	}
	ra(true, false)
	time.Sleep(100 * time.Millisecond)
	if n := rs(); n > 3 {
		t.Fatalf("%d router solicitations, want at most 3 (RFC 4861) and none after the client started", n)
	}
	if len(s.clients) != 2 {
		t.Fatalf("%d clients after the carrier came back, want 2", len(s.clients))
	}

	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.fakes[1].stopped != 1 {
		t.Fatal("Teardown did not stop the DHCPv6 client")
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.watching() != nil {
		if time.Now().After(deadline) {
			t.Fatal("Teardown did not stop the RA watch")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDHCPv6InformationRequestAndExpire: the O flag alone starts a
// stateless client; an expired address goes away.
func TestDHCPv6InformationRequestAndExpire(t *testing.T) {
	s := stubDHCPv6(t)
	d := NewDHCPMode()
	link := dummyLink("eth0", 1)
	d.mu.Lock()
	d.startRA(link, &net.Interface{Name: "eth0", Index: 1}, "")
	d.mu.Unlock()
	s.watching()(false, true)
	if len(s.clients) != 1 || s.clients[0].Stateful {
		t.Fatalf("clients = %+v, want one stateless", s.clients)
	}
	l := &dhcp6.Lease{Addr: net.ParseIP("fd00:10::1a4"), Valid: time.Minute}
	s.clients[0].OnBound(l)
	s.clients[0].OnExpire(l)
	if !slices.Equal(s.deleted, []string{"fd00:10::1a4/128"}) {
		t.Fatalf("deleted = %v", s.deleted)
	}
	d.stopClients()
	if s.clients[0].OnBound(l); len(s.added) != 1 {
		t.Fatal("a lease after the stop was applied")
	}
}

func TestValidRA(t *testing.T) {
	ra := []byte{134, 0, 0, 0, 64, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	ll := &net.IPAddr{IP: net.ParseIP("fe80::1")}
	for _, tt := range []struct {
		name string
		b    []byte
		hop  int
		src  net.Addr
		want bool
	}{
		{"valid", ra, 255, ll, true},
		{"forwarded", ra, 64, ll, false},
		{"global source", ra, 255, &net.IPAddr{IP: net.ParseIP("fd00::1")}, false},
		{"short", ra[:8], 255, ll, false},
		{"not an RA", append([]byte{133}, ra[1:]...), 255, ll, false},
		{"code 1", append([]byte{134, 1}, ra[2:]...), 255, ll, false},
	} {
		if got := validRA(tt.b, &ipv6.ControlMessage{HopLimit: tt.hop}, tt.src); got != tt.want {
			t.Errorf("%s: validRA = %v, want %v", tt.name, got, tt.want)
		}
	}
	if validRA(ra, nil, ll) {
		t.Error("an RA without the hop limit passed")
	}
}

// A later RA with the M flag replaces the stateless client with a
// stateful one; more RAs change nothing.
func TestDHCPv6UpgradesToStateful(t *testing.T) {
	s := stubDHCPv6(t)
	d := NewDHCPMode()
	d.mu.Lock()
	d.startRA(dummyLink("eth0", 1), &net.Interface{Name: "eth0", Index: 1}, "")
	d.mu.Unlock()
	ra := s.watching()
	ra(false, true)
	ra(false, true)
	ra(true, true)
	ra(true, false)
	if len(s.clients) != 2 || s.clients[0].Stateful || !s.clients[1].Stateful || s.fakes[0].stopped != 1 || s.fakes[1].stopped != 0 {
		t.Fatalf("clients %d, stateful %v %v, stops %d %d: want the stateless one stopped and one stateful",
			len(s.clients), s.clients[0].Stateful, len(s.clients) > 1 && s.clients[1].Stateful, s.fakes[0].stopped, s.fakes[len(s.fakes)-1].stopped)
	}
}

// A link-local DHCPv6 DNS server goes to resolv.conf with its zone; the
// domain search list (option 24) joins the search line.
func TestDHCPv6LinkLocalDNSAndDomains(t *testing.T) {
	s := stubDHCPv6(t)
	d := NewDHCPMode()
	d.manageDNS = true
	link := dummyLink("eth0", 1)
	d.mu.Lock()
	d.startRA(link, &net.Interface{Name: "eth0", Index: 1}, "")
	d.mu.Unlock()
	s.watching()(false, true)
	s.clients[0].OnBound(&dhcp6.Lease{DNS: []net.IP{net.ParseIP("fe80::1")}, Domains: []string{"v6.example"}})
	if want := []string{"fe80::1%eth0 search v6.example"}; !slices.Equal(s.written, want) {
		t.Fatalf("resolv.conf = %q, want %q", s.written, want)
	}
}
