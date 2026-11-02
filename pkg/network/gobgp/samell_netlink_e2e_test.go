//go:build linux && linux_e2e

package gobgp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/telekom/BOOTy/pkg/network"
)

// sameLLPort is the BGP port of the switches, so the test needs no port 179.
const sameLLPort = 1790

// sameLLAddr gives name only the link-local address addr (no DAD, no
// address from the MAC) and sets it up.
func sameLLAddr(t *testing.T, name, addr string) {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+name+"/addr_gen_mode", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := netlink.ParseAddr(addr)
	if err != nil {
		t.Fatal(err)
	}
	a.Flags = unix.IFA_F_NODAD
	if err := netlink.AddrAdd(l, a); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(l); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
	}
}

// received returns the IPv4 unicast routes that the peer at addr sent
// (before the import policy) and whether its session is Established.
func received(ctx context.Context, s *server.BgpServer, addr string) (rcv uint64, est bool) {
	_ = s.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
		est = p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_ESTABLISHED
		for _, a := range p.GetAfiSafis() {
			rcv += a.GetState().GetReceived()
		}
	})
	return rcv, est
}

// sameLLRuns numbers the runs of TestSameLinkLocalPeersOnTwoNICs.
var sameLLRuns int

// TestHelperSameLinkLocalSwitches is the switch side of
// TestSameLinkLocalPeersOnTwoNICs in its own network namespace: both
// switch ports (llvt, llvu) have fe80::1, like a fabric that uses one
// link-local address on every port, and announce 198.51.100.0/24. It
// writes the routes it received per port to SAMELL_OUT.
func TestHelperSameLinkLocalSwitches(t *testing.T) {
	out := os.Getenv("SAMELL_OUT")
	if out == "" {
		t.Skip("helper of TestSameLinkLocalPeersOnTwoNICs")
	}
	for {
		_, e1 := netlink.LinkByName("llvt")
		_, e2 := netlink.LinkByName("llvu")
		if e1 == nil && e2 == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo)
	}
	sameLLAddr(t, "llvt", "fe80::1/64")
	sameLLAddr(t, "llvu", "fe80::1/64")
	ctx := context.Background()
	sw := startServerAt(t, 65510, "192.0.2.1", "::", sameLLPort)
	for _, nic := range []string{"llvt", "llvu"} {
		if err := sw.AddPeer(ctx, &apipb.AddPeerRequest{Peer: &apipb.Peer{
			Conf:      &apipb.PeerConf{NeighborAddress: "fe80::2%" + nic, PeerAsn: 4210000017},
			Transport: &apipb.Transport{PassiveMode: true},
			AfiSafis:  ipv4Families(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	addRoute(t, sw, "198.51.100.0/24", 0)
	tmp := out + ".tmp"
	for {
		vt, _ := received(ctx, sw, "fe80::2%llvt")
		vu, _ := received(ctx, sw, "fe80::2%llvu")
		// The reader opens the file by name. Replace it atomically so it
		// never sees the empty file between truncate and write.
		if err := os.WriteFile(tmp, fmt.Appendf(nil, "%d %d", vt, vu), 0o600); err == nil {
			_ = os.Rename(tmp, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Two unnumbered sessions on different NICs whose switches have the same
// link-local address: llva with a permit-all route-map, llvb with "bgp
// ebgp-requires-policy" and no route-map. Like the per-interface peers of
// FRR, each session keeps its own policy: llva exchanges routes both
// ways, llvb none. Before the neighbor sets had zones, the llvb reject
// applied to llva too, so llva was Established but carried no routes.
func TestSameLinkLocalPeersOnTwoNICs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	out := t.TempDir() + "/out"
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "unshare", "-n", exe, "-test.run", "^TestHelperSameLinkLocalSwitches$", "-test.v")
	cmd.Env = append(os.Environ(), "SAMELL_OUT="+out)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	// The switch ends move into the helper's namespace, so wait until
	// unshare made it.
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the helper network namespace", func() bool {
		ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", cmd.Process.Pid))
		return err == nil && ns != self
	})
	// Go caches the index of an interface name (the zone) for up to 60 s,
	// so a run of -count=N must not reuse the names of the run before.
	sameLLRuns++
	a, b := fmt.Sprintf("llva%d", sameLLRuns), fmt.Sprintf("llvb%d", sameLLRuns)
	for _, p := range [][2]string{{a, "llvt"}, {b, "llvu"}} {
		v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: p[0]}, PeerName: p[1]}
		if err := netlink.LinkAdd(v); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = netlink.LinkDel(v) })
		peer, err := netlink.LinkByName(p[1])
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetNsPid(peer, cmd.Process.Pid); err != nil {
			t.Fatal(err)
		}
		sameLLAddr(t, p[0], "fe80::2/64")
	}

	// The switches listen once the helper writes its first counts.
	waitFor(t, "the switches", func() bool {
		_, err := os.Stat(out)
		return err == nil
	})

	ctx := context.Background()
	s := startServer(t, 4210000017, "192.0.2.17", -1)
	cfg := &Config{ASN: 4210000017,
		RouteMaps: []network.RouteMap{{Name: "ALL", Entries: []network.RouteMapEntry{{Seq: 10}}}},
		Sessions: []network.BGPSession{
			{Name: a, Interface: true, RemoteASN: 65510, IPv4Unicast: true,
				RouteMaps: []network.SessionRouteMaps{{Family: "ipv4-unicast", In: "ALL", Out: "ALL"}}},
			{Name: b, Interface: true, RemoteASN: 65510, IPv4Unicast: true, EBGPRequiresPolicy: true},
		}}
	u := &UnderlayTier{bgp: s, cfg: cfg, log: slog.Default()}
	for i, nic := range []string{a, b} {
		addr := "fe80::1%" + nic
		u.recordInterfacePeer(nic, addr)
		peer := buildSessionPeer(cfg, &cfg.Sessions[i], addr)
		peer.Transport.RemotePort = sameLLPort
		peer.Timers.Config.ConnectAtOnce = true
		peer.Timers.Config.ConnectRetry = 1
		u.ifacePeerConfs[nic] = peer
		if err := u.addPeer(ctx, peer); err != nil {
			t.Fatal(err)
		}
	}
	if err := u.installPolicies(ctx); err != nil {
		t.Fatal(err)
	}
	addRoute(t, s, "203.0.113.0/24", 0)

	v4 := &apipb.Family{Afi: apipb.Family_AFI_IP, Safi: apipb.Family_SAFI_UNICAST}
	var state string
	check := func() bool {
		aRcv, aEst := received(ctx, s, "fe80::1%"+a)
		bRcv, bEst := received(ctx, s, "fe80::1%"+b)
		var from []string // the neighbors of the switch route in the RIB
		_ = s.ListPath(apiutil.ListPathRequest{TableType: apipb.TableType_TABLE_TYPE_GLOBAL, Family: apiutil.ToFamily(v4)},
			func(prefix bgp.NLRI, paths []*apiutil.Path) {
				for _, p := range paths {
					if prefix.String() == "198.51.100.0/24" {
						from = append(from, p.PeerAddress.String())
					}
				}
			})
		sw, _ := os.ReadFile(out)
		state = fmt.Sprintf("llva established=%v received=%d, llvb established=%v received=%d, switch route in the RIB from %v, switch received on llvt/llvu %q",
			aEst, aRcv, bEst, bRcv, from, sw)
		return aEst && bEst && aRcv == 1 && bRcv == 1 && slices.Equal(from, []string{"fe80::1%" + a}) && strings.TrimSpace(string(sw)) == "1 0"
	}
	deadline := time.Now().Add(30 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("want llva to exchange the routes and llvb none: %s", state)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Nothing changes once both sessions sent their updates.
	time.Sleep(2 * time.Second)
	if !check() {
		t.Fatalf("routes changed: %s", state)
	}
	t.Log(state)
}
