//go:build linux && linux_e2e

package gobgp

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"os"
	"slices"
	"testing"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// kernelFDB is the FDB entry of a MAC on a VXLAN device: its VTEP or its
// nexthop ID. The netlink library does not decode NDA_NH_ID.
type kernelFDB struct {
	dst  net.IP
	nhid uint32
}

func dumpFDB(t *testing.T, index int, mac string) (kernelFDB, bool) {
	t.Helper()
	req := nl.NewNetlinkRequest(unix.RTM_GETNEIGH, unix.NLM_F_DUMP)
	req.AddData(&netlink.Ndmsg{Family: unix.AF_BRIDGE})
	msgs, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWNEIGH)
	if err != nil {
		t.Fatalf("fdb dump: %v", err)
	}
	for _, m := range msgs {
		if int(binary.NativeEndian.Uint32(m[4:8])) != index {
			continue
		}
		attrs, err := nl.ParseRouteAttr(m[unix.SizeofNdMsg:])
		if err != nil {
			t.Fatalf("fdb attrs: %v", err)
		}
		var e kernelFDB
		var lladdr net.HardwareAddr
		for _, a := range attrs {
			switch a.Attr.Type {
			case unix.NDA_LLADDR:
				lladdr = a.Value
			case unix.NDA_DST:
				e.dst = net.IP(a.Value)
			case netlink.NDA_NH_ID:
				e.nhid = binary.NativeEndian.Uint32(a.Value)
			}
		}
		if lladdr.String() == mac {
			return e, true
		}
	}
	return kernelFDB{}, false
}

// dumpNexthops returns the kernel nexthops: the members of a group, the
// gateway of a single nexthop.
func dumpNexthops(t *testing.T) (groups map[uint32][]uint32, gws map[uint32]string) {
	t.Helper()
	req := nl.NewNetlinkRequest(unix.RTM_GETNEXTHOP, unix.NLM_F_DUMP)
	req.AddData(&nhMsg{})
	msgs, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWNEXTHOP)
	if err != nil {
		t.Fatalf("nexthop dump: %v", err)
	}
	groups, gws = map[uint32][]uint32{}, map[uint32]string{}
	for _, m := range msgs {
		attrs, err := nl.ParseRouteAttr(m[unix.SizeofNhmsg:])
		if err != nil {
			t.Fatalf("nexthop attrs: %v", err)
		}
		var id uint32
		var members []uint32
		var gw string
		for _, a := range attrs {
			switch a.Attr.Type {
			case unix.NHA_ID:
				id = binary.NativeEndian.Uint32(a.Value)
			case unix.NHA_GATEWAY:
				gw = net.IP(a.Value).String()
			case unix.NHA_GROUP:
				for i := 0; i+unix.SizeofNexthopGrp <= len(a.Value); i += unix.SizeofNexthopGrp {
					members = append(members, binary.NativeEndian.Uint32(a.Value[i:]))
				}
			}
		}
		if members != nil {
			groups[id] = members
		} else {
			gws[id] = gw
		}
	}
	return groups, gws
}

// TestESINetlinkAliasing runs the remote ES handling against the kernel: the
// FDB nexthops, the group and the FDB entry with nhid must be accepted, the
// group must follow an EAD withdraw, and the last withdraw must remove the
// entry and the nexthops.
func TestESINetlinkAliasing(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	cfg := &Config{RouterID: "192.0.2.10", ProvisionVNI: 4242, BridgeName: "br-esie2e", VXLANName: "vx4242", EnableL2: true}
	_, vxlan := setupType5E2ELinks(t, cfg)
	o := NewOverlayTier(cfg)
	o.log = slog.Default()

	esi := &apipb.EthernetSegmentIdentifier{Value: []byte{0, 0x11, 0x22, 0x33, 0x44, 0x55, 0, 0, 1}}
	vteps := []string{"192.0.2.21", "192.0.2.22"}
	rds := make([]*apipb.RouteDistinguisher, len(vteps))
	for i, vtep := range vteps {
		rd := buildRouteDistinguisher(65000, uint32(i+1))
		rds[i] = rd
		for _, tag := range []uint32{maxET, 0} {
			o.handleType1Route(&apipb.EVPNEthernetAutoDiscoveryRoute{Rd: rd, Esi: esi, EthernetTag: tag}, vtep, false, false)
		}
	}
	const mac = "aa:bb:cc:00:00:01"
	o.handleType2Route(&apipb.EVPNMACIPAdvertisementRoute{Rd: rds[0], Esi: esi, MacAddress: mac}, vteps[0], 0, false)
	index := vxlan.Attrs().Index
	hasMAC := func() bool {
		_, ok := dumpFDB(t, index, mac)
		return ok
	}
	viaGroup := func(step string) {
		t.Helper()
		g := o.esGroups[esiKey(esi)]
		e, ok := dumpFDB(t, index, mac)
		groups, gws := dumpNexthops(t)
		members := slices.Clone(groups[g.id])
		slices.Sort(members)
		want := []uint32{o.vtepNexthops[vteps[0]], o.vtepNexthops[vteps[1]]}
		slices.Sort(want)
		if !ok || e.nhid != g.id || e.dst != nil || !slices.Equal(members, want) ||
			gws[want[0]] == "" || gws[want[1]] == "" {
			t.Fatalf("%s: fdb %+v (in kernel %v), group %d members %v, nexthops %v; want the MAC on the group of %v",
				step, e, ok, g.id, groups[g.id], gws, want)
		}
	}
	viaVTEP := func(step string) {
		t.Helper()
		if e, ok := dumpFDB(t, index, mac); !ok || e.nhid != 0 || !e.dst.Equal(net.ParseIP(vteps[0])) {
			t.Fatalf("%s: fdb %+v (in kernel %v), want the MAC on VTEP %s without nhid", step, e, ok, vteps[0])
		}
	}
	singleActive := func(on bool) {
		o.handleType1Route(&apipb.EVPNEthernetAutoDiscoveryRoute{Rd: rds[1], Esi: esi, EthernetTag: maxET}, vteps[1], on, false)
	}

	// All-active: the group. Single-active: the VTEP of the MAC route, then
	// the group again. The kernel refuses to replace one kind with the
	// other, so each move deletes the entry first.
	viaGroup("all-active")
	singleActive(true)
	viaVTEP("single-active")
	singleActive(false)
	viaGroup("all-active again")

	o.handleType1Route(&apipb.EVPNEthernetAutoDiscoveryRoute{Rd: rds[0], Esi: esi, EthernetTag: maxET}, "", false, true)
	if g := o.esGroups[esiKey(esi)]; len(g.vteps) != 1 || g.vteps[0] != vteps[1] || !hasMAC() {
		t.Fatalf("after the EAD-per-ES withdraw of %s: group %+v, MAC in kernel %v", vteps[0], g, hasMAC())
	}
	o.handleType1Route(&apipb.EVPNEthernetAutoDiscoveryRoute{Rd: rds[1], Esi: esi, EthernetTag: maxET}, "", false, true)
	if groups, gws := dumpNexthops(t); hasMAC() || len(o.esGroups) != 0 || len(o.vtepNexthops) != 0 || len(groups)+len(gws) != 0 {
		t.Fatalf("after the last EAD-per-ES withdraw: MAC in kernel %v, groups %v, nexthops %v, kernel %v %v",
			hasMAC(), o.esGroups, o.vtepNexthops, groups, gws)
	}

	// Teardown deletes the nexthops: global kernel objects that outlive
	// the VXLAN device.
	for i, vtep := range vteps {
		for _, tag := range []uint32{maxET, 0} {
			o.handleType1Route(&apipb.EVPNEthernetAutoDiscoveryRoute{Rd: rds[i], Esi: esi, EthernetTag: tag}, vtep, false, false)
		}
	}
	if groups, _ := dumpNexthops(t); len(groups) != 1 {
		t.Fatalf("kernel groups %v, want the ES group back", groups)
	}
	if err := o.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if groups, gws := dumpNexthops(t); len(groups)+len(gws) != 0 {
		t.Fatalf("nexthops after teardown: %v %v", groups, gws)
	}
}
