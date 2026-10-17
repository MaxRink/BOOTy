//go:build linux

package network

import (
	"strings"
	"testing"
)

func stubLinks(t *testing.T, links, nics []string) {
	t.Helper()
	origLinks, origNICs := linkNames, physicalNICs
	linkNames = func() []string { return links }
	physicalNICs = func() []string { return nics }
	t.Cleanup(func() { linkNames, physicalNICs = origLinks, origNICs })
}

func TestUsedInterfacesOf(t *testing.T) {
	stubLinks(t, []string{"lo", "ens1f0np0", "ens1f1np1", "ens2f0", "eth0"}, []string{"ens1f0np0", "ens1f1np1", "ens2f0"})
	cases := []struct {
		name string
		cfg  Config
		want string
		all  bool
	}{
		{"static", Config{StaticIface: "eth0", StaticIP: "10.0.0.2/24"}, "eth0", false},
		{"static auto-detect", Config{StaticIface: "auto-detect", StaticIP: "10.0.0.2/24"}, "ens1f0np0", false},
		{"static without interface", Config{StaticIP: "10.0.0.2/24"}, "ens1f0np0", false},
		{"bond and vlan", Config{BondInterfaces: "ens1f0np0,ens1f1np1", VLANs: []VLANConfig{{Parent: "bond0", ID: 10}}},
			"ens1f0np0,ens1f1np1,bond0", false},
		{"glob", Config{BondInterfaces: "ens1f*"}, "ens1f0np0,ens1f1np1", false},
		{"netplan ethernets and frr.conf peers", Config{InterfaceNames: []string{"ens1f0np0", "ens1f1np1"}},
			"ens1f0np0,ens1f1np1", false},
		{"gobgp frr.conf sessions", Config{NetworkMode: "gobgp", InterfaceNames: []string{"ens1f0np0"},
			BGPSessions: []BGPSession{{Name: "ens1f0np0", Interface: true}, {Name: "192.0.2.1"}}}, "ens1f0np0", false},
		{"gobgp named interfaces", Config{NetworkMode: "gobgp", BGPInterfaces: "ens2f0,ens2f1"}, "ens2f0,ens2f1", false},
		// GoBGP without sessions or BGP_INTERFACES peers on every NIC, even
		// when netplan names some of them.
		{"gobgp on every NIC", Config{NetworkMode: "gobgp", InterfaceNames: []string{"ens1f0np0"}}, "ens1f0np0", true},
		// The FRR stack peers on every NIC, also with BGP_INTERFACES.
		{"frr", Config{UnderlayIP: "10.0.0.1", ASN: 65000, BGPInterfaces: "ens2f0"}, "ens2f0", true},
		{"dhcp", Config{}, "", false},
	}
	for _, c := range cases {
		u := UsedInterfacesOf(&c.cfg)
		if got := strings.Join(u.Names, ","); got != c.want || u.AllPhysical != c.all {
			t.Errorf("%s: %q all=%v, want %q all=%v", c.name, got, u.AllPhysical, c.want, c.all)
		}
	}
}

func TestRecordUsedInterfaces(t *testing.T) {
	stubLinks(t, nil, nil)
	orig := activeInterfaces.Load()
	t.Cleanup(func() { activeInterfaces.Store(orig) })
	activeInterfaces.Store(nil)
	if _, known := ActiveInterfaces(); known {
		t.Fatal("known before the record")
	}
	RecordUsedInterfaces(&Config{StaticIface: "eth0"})
	if u, known := ActiveInterfaces(); !known || len(u.Names) != 1 || u.Names[0] != "eth0" {
		t.Fatalf("%+v %v", u, known)
	}
}
