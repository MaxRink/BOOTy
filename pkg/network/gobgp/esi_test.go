//go:build linux

package gobgp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"syscall"
	"testing"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/vishvananda/netlink"
)

// esTest drives the remote ES handling against the netlink mock.
type esTest struct {
	t    *testing.T
	o    *OverlayTier
	mock *mockOverlayNetlinkOps
	esi  *apipb.EthernetSegmentIdentifier
}

func newESTest(t *testing.T) *esTest {
	t.Helper()
	mock := &mockOverlayNetlinkOps{}
	return &esTest{
		t:    t,
		mock: mock,
		esi:  &apipb.EthernetSegmentIdentifier{Value: []byte{0, 0x11, 0x22, 0x33, 0x44, 0x55, 0, 0, 1}},
		o: &OverlayTier{
			cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision", EnableL2: true},
			log:        slog.Default(),
			netlinkOps: mock,
		},
	}
}

func (e *esTest) rd(vtep string) *apipb.RouteDistinguisher {
	n := uint32(1)
	if len(vtep) > 0 {
		n = uint32(vtep[len(vtep)-1])
	}
	return testRD(n)
}

// ead sends an EAD route of the test ES with the given Ethernet Tag.
func (e *esTest) ead(vtep string, tag uint32, singleActive, withdraw bool) {
	e.o.handleType1Route(&apipb.EVPNEthernetAutoDiscoveryRoute{Rd: e.rd(vtep), Esi: e.esi, EthernetTag: tag}, vtep, singleActive, withdraw)
}

// activate sends both EAD routes of a VTEP.
func (e *esTest) activate(vtep string) {
	e.ead(vtep, maxET, false, false)
	e.ead(vtep, 0, false, false)
}

func (e *esTest) mac(vtep string) {
	e.o.handleType2Route(&apipb.EVPNMACIPAdvertisementRoute{Rd: e.rd(vtep), Esi: e.esi, MacAddress: "aa:bb:cc:00:00:01"}, vtep, 0, false)
}

func (e *esTest) installed() string { return e.o.type2FDB["aa:bb:cc:00:00:01"] }

func TestESActivationNeedsBothEADRoutes(t *testing.T) {
	t.Run("EAD-per-ES alone", func(t *testing.T) {
		e := newESTest(t)
		e.mac("10.0.0.1")
		e.ead("10.0.0.1", maxET, false, false)
		if e.installed() != "" || len(e.mock.fdbNhids) != 0 || len(e.mock.sets) != 0 {
			t.Fatalf("installed %q with only the EAD-per-ES route", e.installed())
		}
	})
	t.Run("per-ES is Ethernet Tag MAX-ET only", func(t *testing.T) {
		e := newESTest(t)
		e.mac("10.0.0.1")
		e.ead("10.0.0.1", 5, false, false) // an EAD-per-EVI of a VLAN-aware bundle
		e.ead("10.0.0.1", 0, false, false)
		if e.installed() != "" {
			t.Fatalf("two EAD-per-EVI routes activated the VTEP: %q", e.installed())
		}
	})
}

func TestESReservedAndFilteredRoutes(t *testing.T) {
	t.Run("MAX-ESI is no ES", func(t *testing.T) {
		e := newESTest(t)
		e.esi = &apipb.EthernetSegmentIdentifier{Type: 0xff, Value: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
		e.activate("10.0.0.1")
		e.mac("10.0.0.1")
		if len(e.o.type1Paths) != 0 || e.installed() != "10.0.0.1" {
			t.Fatalf("MAX-ESI: EAD routes %v, installed %q; want no ES and the VTEP entry", e.o.type1Paths, e.installed())
		}
	})
	t.Run("own router ID", func(t *testing.T) {
		e := newESTest(t)
		e.activate("10.0.0.99")
		if len(e.o.type1Paths) != 0 {
			t.Fatalf("own EAD routes recorded: %v", e.o.type1Paths)
		}
	})
	t.Run("IPv6 VTEP", func(t *testing.T) {
		e := newESTest(t)
		e.activate("2001:db8::1")
		if len(e.o.type1Paths) != 0 {
			t.Fatalf("IPv6 VTEP recorded: %v", e.o.type1Paths)
		}
	})
}

func TestESVTEPNexthopsFollowTheGroup(t *testing.T) {
	e := newESTest(t)
	e.activate("10.0.0.1")
	e.activate("10.0.0.2")
	e.mac("10.0.0.1")
	id1 := e.o.vtepNexthops["10.0.0.1"]
	e.ead("10.0.0.1", maxET, false, true)
	if _, ok := e.o.vtepNexthops["10.0.0.1"]; ok || !slices.Contains(e.mock.nexthopDels, id1) {
		t.Fatalf("the nexthop of the VTEP that left is kept: nexthops %v, deletes %v", e.o.vtepNexthops, e.mock.nexthopDels)
	}
}

// RFC 7432 sections 8.4 and 14.1.1: a single-active ES uses only the
// primary PE, the VTEP of the MAC route; a change between a VTEP entry and
// a group entry deletes the old entry first (the kernel refuses to
// replace one kind with the other).
func TestESSingleActiveUsesThePrimaryPE(t *testing.T) {
	e := newESTest(t)
	e.activate("10.0.0.1")
	e.activate("10.0.0.2")
	e.mac("10.0.0.1")
	if _, group := parseNhGroupKey(e.installed()); !group {
		t.Fatalf("all-active: installed %q, want the group", e.installed())
	}
	e.ead("10.0.0.2", maxET, true, false) // one Single-Active flag is enough
	if e.installed() != "10.0.0.1" {
		t.Fatalf("single-active: installed %q, want the primary PE 10.0.0.1", e.installed())
	}
	if len(e.mock.fdbDels) != 1 || len(e.mock.sets) != 1 {
		t.Fatalf("group -> VTEP: group entry deletes %v, VTEP sets %d; want the delete first", e.mock.fdbDels, len(e.mock.sets))
	}
	e.ead("10.0.0.2", maxET, false, false)
	if _, group := parseNhGroupKey(e.installed()); !group || len(e.mock.dels) != 1 {
		t.Fatalf("back to all-active: installed %q, VTEP entry deletes %d", e.installed(), len(e.mock.dels))
	}
}

// Without kernel FDB nexthops (before Linux 5.8) the MAC uses its VTEP.
func TestESGroupFailureFallsBackToTheVTEP(t *testing.T) {
	e := newESTest(t)
	e.mock.nexthopErr = errors.New("not supported")
	e.activate("10.0.0.1")
	e.mac("10.0.0.1")
	if e.installed() != "10.0.0.1" || len(e.o.esGroups) != 0 {
		t.Fatalf("installed %q, groups %v; want the VTEP entry", e.installed(), e.o.esGroups)
	}
}

// A nexthop whose delete failed is kept and deleted later.
func TestESFailedNexthopDeleteIsRetried(t *testing.T) {
	e := newESTest(t)
	e.activate("10.0.0.1")
	e.mac("10.0.0.1")
	g := e.o.esGroups[esiKey(e.esi)].id
	e.mock.nexthopDelErr = errors.New("busy")
	e.ead("10.0.0.1", maxET, false, true)
	if !slices.Contains(e.o.staleNexthops, g) {
		t.Fatalf("failed delete of group %d not kept: %v", g, e.o.staleNexthops)
	}
	e.mock.nexthopDelErr = nil
	e.mock.nexthopDels = nil
	e.activate("10.0.0.2")
	if !slices.Contains(e.mock.nexthopDels, g) || slices.Contains(e.o.staleNexthops, g) {
		t.Fatalf("group %d not deleted at the next change: deletes %v, stale %v", g, e.mock.nexthopDels, e.o.staleNexthops)
	}
}

// Teardown deletes every nexthop (global kernel objects), and a callback
// after Teardown installs nothing.
func TestESTeardownRemovesNexthopsAndStops(t *testing.T) {
	e := newESTest(t)
	e.activate("10.0.0.1")
	e.mac("10.0.0.1")
	ids := []uint32{e.o.esGroups[esiKey(e.esi)].id, e.o.vtepNexthops["10.0.0.1"]}
	if err := e.o.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !slices.Contains(e.mock.nexthopDels, id) {
			t.Errorf("nexthop %d not deleted at teardown: %v", id, e.mock.nexthopDels)
		}
	}
	before := fmt.Sprint(len(e.mock.nexthops), len(e.mock.fdbNhids), len(e.mock.sets))
	e.activate("10.0.0.2")
	e.mac("10.0.0.2")
	if after := fmt.Sprint(len(e.mock.nexthops), len(e.mock.fdbNhids), len(e.mock.sets)); after != before {
		t.Fatalf("callbacks after teardown installed state: %s -> %s", before, after)
	}
}

// The Single-Active flag comes from the ESI Label community of the
// received EAD-per-ES path (RFC 7432 section 7.5).
func TestESSingleActiveFlagFromThePath(t *testing.T) {
	for _, single := range []bool{false, true} {
		e := newESTest(t)
		e.o.cfg.ASN = 65000
		p := mustMarshalPath(t, &apipb.EVPNEthernetAutoDiscoveryRoute{Rd: e.rd("10.0.0.1"), Esi: e.esi, EthernetTag: maxET}, "10.0.0.1", false)
		label, err := wrapExt(&apipb.ESILabelExtended{IsSingleActive: single})
		if err != nil {
			t.Fatal(err)
		}
		ec, err := wrapAttr(&apipb.ExtendedCommunitiesAttribute{Communities: []*apipb.ExtendedCommunity{mustRT2(t, 65000, 100), label}})
		if err != nil {
			t.Fatal(err)
		}
		p.Pattrs = append(p.Pattrs, ec)
		e.o.processRouteUpdate(p)
		if len(e.o.type1Paths) != 1 || e.o.esSingleActive(esiKey(e.esi)) != single {
			t.Fatalf("Single-Active %v: EAD routes %+v", single, e.o.type1Paths)
		}
	}
}

// RFC 7432 sections 8.2 and 14.1.1: when the primary PE of a single-active
// ES withdraws its EAD routes, its MACs leave the FDB at once (they flood)
// instead of staying on the failed PE until each MAC route is withdrawn.
func TestESSingleActivePrimaryWithdrawRemovesTheMAC(t *testing.T) {
	e := newESTest(t)
	e.ead("10.0.0.1", maxET, true, false)
	e.ead("10.0.0.1", 0, false, false)
	e.ead("10.0.0.2", maxET, true, false)
	e.ead("10.0.0.2", 0, false, false)
	e.mac("10.0.0.1")
	if e.installed() != "10.0.0.1" {
		t.Fatalf("installed %q, want the primary PE", e.installed())
	}
	e.ead("10.0.0.1", maxET, true, true)
	if e.installed() != "" || len(e.mock.dels) != 1 {
		t.Fatalf("after the primary's EAD withdraw: installed %q, deletes %v; want the MAC removed", e.installed(), e.mock.dels)
	}
}

// Only the EAD-per-ES route carries the Single-Active flag; a flagged
// EAD-per-EVI route does not make the ES single-active.
func TestESSingleActiveFlagOnlyFromPerES(t *testing.T) {
	e := newESTest(t)
	e.ead("10.0.0.1", maxET, false, false)
	e.ead("10.0.0.1", 0, true, false)
	e.activate("10.0.0.2")
	e.mac("10.0.0.1")
	if _, group := parseNhGroupKey(e.installed()); !group {
		t.Fatalf("installed %q, want the group of an all-active ES", e.installed())
	}
}

// Two ESs: a single-active one does not change the MACs of the other, and
// a VTEP nexthop that another ES group still uses stays.
func TestESTwoSegments(t *testing.T) {
	e := newESTest(t)
	esiA := e.esi
	esiB := &apipb.EthernetSegmentIdentifier{Value: []byte{0, 0x11, 0x22, 0x33, 0x44, 0x55, 0, 0, 2}}
	for _, vtep := range []string{"10.0.0.1", "10.0.0.2"} {
		e.esi = esiA
		e.ead(vtep, maxET, true, false)
		e.ead(vtep, 0, false, false)
		e.esi = esiB
		e.activate(vtep)
	}
	e.mac("10.0.0.1") // a MAC of ES B
	if _, group := parseNhGroupKey(e.installed()); !group {
		t.Fatalf("ES B MAC installed %q; the single-active ES A changed it", e.installed())
	}
	id, ok := e.o.vtepNexthops["10.0.0.1"]
	if !ok || len(e.mock.nexthopDels) != 0 {
		t.Fatalf("VTEP 1 nexthop %v, deletes %v; want it in both groups", ok, e.mock.nexthopDels)
	}
	e.esi = esiA
	e.ead("10.0.0.1", maxET, true, true) // VTEP 1 leaves ES A only
	if got, ok := e.o.vtepNexthops["10.0.0.1"]; !ok || got != id || slices.Contains(e.mock.nexthopDels, id) {
		t.Fatalf("the nexthop of VTEP 1, still in the group of ES B, went: nexthops %v, deletes %v", e.o.vtepNexthops, e.mock.nexthopDels)
	}
}

// A VTEP nexthop is installed once and reused by later group changes.
func TestESVTEPNexthopReused(t *testing.T) {
	e := newESTest(t)
	e.activate("10.0.0.1")
	e.activate("10.0.0.2")
	e.activate("10.0.0.3")
	e.ead("10.0.0.3", maxET, false, true)
	vtepInstalls := 0
	for _, nh := range e.mock.nexthops {
		if nh.vtep != nil {
			vtepInstalls++
		}
	}
	if vtepInstalls != 3 {
		t.Fatalf("%d VTEP nexthop installs for 3 VTEPs, want each once", vtepInstalls)
	}
}

// A failed VTEP nexthop install, or a failed replace of an existing group,
// moves the MACs of the ES to their VTEP; the old group goes.
func TestESNexthopFailuresFallBackToTheVTEP(t *testing.T) {
	t.Run("VTEP nexthop", func(t *testing.T) {
		e := newESTest(t)
		e.activate("10.0.0.1")
		e.mac("10.0.0.1")
		g := e.o.esGroups[esiKey(e.esi)].id
		e.mock.nexthopVTEPErr = errors.New("boom")
		e.activate("10.0.0.2")
		if e.installed() != "10.0.0.1" || len(e.o.esGroups) != 0 || !slices.Contains(e.mock.nexthopDels, g) {
			t.Fatalf("installed %q, groups %v, deletes %v; want the VTEP entry and the old group deleted",
				e.installed(), e.o.esGroups, e.mock.nexthopDels)
		}
	})
	t.Run("replace of an existing group", func(t *testing.T) {
		e := newESTest(t)
		e.activate("10.0.0.1")
		e.mac("10.0.0.1")
		g := e.o.esGroups[esiKey(e.esi)].id
		e.mock.nexthopErr = errors.New("boom")
		e.activate("10.0.0.2")
		if e.installed() != "10.0.0.1" || len(e.o.esGroups) != 0 || !slices.Contains(e.mock.nexthopDels, g) {
			t.Fatalf("installed %q, groups %v, deletes %v; want the VTEP entry and the old group deleted",
				e.installed(), e.o.esGroups, e.mock.nexthopDels)
		}
	})
}

// Teardown also deletes the nexthops of earlier failed deletes.
func TestESTeardownDeletesStaleNexthops(t *testing.T) {
	e := newESTest(t)
	e.activate("10.0.0.1")
	e.mac("10.0.0.1")
	g := e.o.esGroups[esiKey(e.esi)].id
	e.mock.nexthopDelErr = errors.New("busy")
	e.ead("10.0.0.1", maxET, false, true)
	e.mock.nexthopDelErr = nil
	e.mock.nexthopDels = nil
	if err := e.o.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.mock.nexthopDels, g) {
		t.Fatalf("Teardown deletes %v, want the stale group %d", e.mock.nexthopDels, g)
	}
}

// A MAC/IP route with a non-reserved ESI gets its IP -> MAC bridge neighbor
// like any MAC/IP route (#766): while the ES is not active yet (no FDB
// entry), after the EAD routes activate it (FDB entry via the group), and
// the neighbor goes with the withdraw.
func TestESMACIPRouteGetsItsNeighbor(t *testing.T) {
	e := newESTest(t)
	route := &apipb.EVPNMACIPAdvertisementRoute{Rd: e.rd("10.0.0.1"), Esi: e.esi, MacAddress: "aa:bb:cc:00:00:01", IpAddress: "10.0.0.50"}
	neigh := func(ops []*netlink.Neigh) (n []*netlink.Neigh) {
		for _, x := range ops {
			if x.Family != syscall.AF_BRIDGE && x.IP.Equal(net.ParseIP("10.0.0.50")) {
				n = append(n, x)
			}
		}
		return n
	}
	e.o.handleType2Route(route, "10.0.0.1", 0, false)
	if sets := neigh(e.mock.sets); e.installed() != "" || len(sets) != 1 || sets[0].HardwareAddr.String() != "aa:bb:cc:00:00:01" {
		t.Fatalf("inactive ES: FDB %q, neighbor sets %v; want no FDB entry and the neighbor", e.installed(), sets)
	}
	e.activate("10.0.0.1")
	if _, group := parseNhGroupKey(e.installed()); !group || len(neigh(e.mock.dels)) != 0 {
		t.Fatalf("active ES: FDB %q, neighbor deletes %v; want the group and the neighbor kept", e.installed(), neigh(e.mock.dels))
	}
	e.o.handleType2Route(route, "10.0.0.1", 0, true)
	if dels := neigh(e.mock.dels); len(dels) != 1 {
		t.Fatalf("withdraw: neighbor deletes %v, want the entry removed", dels)
	}
}
