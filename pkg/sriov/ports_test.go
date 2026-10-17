package sriov

import "testing"

func TestGroupPortsCountsAdapterPorts(t *testing.T) {
	pfs := []PF{
		{Interface: "a0", Bus: "pci", PCIAddress: "0000:3b:00.0"},
		{Interface: "a1", Bus: "pci", PCIAddress: "0000:3b:00.1"},
		{Interface: "b0", Bus: "pci", PCIAddress: "0000:3b:01.0"},
		{Interface: "c0", Bus: "pci", PCIAddress: "0001:3b:00.0"},
		// netdevsim: two ports of one device.
		{Interface: "n0", Bus: "netdevsim", PCIAddress: "netdevsim10"},
		{Interface: "n1", Bus: "netdevsim", PCIAddress: "netdevsim10"},
		{Interface: "n2", Bus: "netdevsim", PCIAddress: "netdevsim11"},
	}
	groupPorts(pfs)
	want := map[string]int{"a0": 2, "a1": 2, "b0": 1, "c0": 1, "n0": 2, "n1": 2, "n2": 1}
	for _, pf := range pfs {
		if pf.PortCount != want[pf.Interface] {
			t.Errorf("%s PortCount = %d, want %d", pf.Interface, pf.PortCount, want[pf.Interface])
		}
	}
}

func TestVFLAG(t *testing.T) {
	esw := Eswitch{Capability: CapSupported, Mode: EswitchLegacy}
	cases := []struct {
		name string
		pf   PF
		want Capability
	}{
		{"mlx5 dual port", PF{Driver: "mlx5_core", LinkType: "eth", PortCount: 2, Eswitch: esw}, CapSupported},
		{"mlx5 unknown link type", PF{Driver: "mlx5_core", PortCount: 2, Eswitch: esw}, CapSupported},
		{"mlx5 single port", PF{Driver: "mlx5_core", LinkType: "eth", PortCount: 1, Eswitch: esw}, CapUnsupported},
		{"mlx5 ib", PF{Driver: "mlx5_core", LinkType: "ib", PortCount: 2, Eswitch: esw}, CapUnsupported},
		{"mlx5 no eswitch", PF{Driver: "mlx5_core", PortCount: 2}, CapUnsupported},
		{"ice dual port", PF{Driver: "ice", PortCount: 2}, CapUnknown},
		{"ice single port", PF{Driver: "ice", PortCount: 1}, CapUnsupported},
		{"bnxt dual port", PF{Driver: "bnxt_en", PortCount: 2, Eswitch: esw}, CapUnknown},
		{"bnxt no eswitch", PF{Driver: "bnxt_en", PortCount: 2}, CapUnsupported},
		{"i40e", PF{Driver: "i40e", PortCount: 2}, CapUnsupported},
		{"no driver", PF{PortCount: 2}, CapUnknown},
	}
	for _, c := range cases {
		got, reason := vfLAG(&c.pf)
		if got != c.want || reason == "" {
			t.Errorf("%s: %s (%q), want %s", c.name, got, reason, c.want)
		}
	}
}
