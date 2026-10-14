package netname

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakeSysfs builds the sysfs entries net_id reads.
type fakeSysfs struct {
	t    *testing.T
	root string
}

func (f fakeSysfs) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f fakeSysfs) link(rel, target string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, target), p); err != nil {
		f.t.Fatal(err)
	}
}

// pciDev creates a PCI device below devices/ and its bus/pci/devices link.
// multifunction sets the bit in the configuration header.
func (f fakeSysfs) pciDev(path string, multifunction bool) {
	cfg := make([]byte, 64)
	if multifunction {
		cfg[0x0e] = 0x80
	}
	f.write(filepath.Join("devices", path, "config"), string(cfg))
	f.link(filepath.Join("bus", "pci", "devices", filepath.Base(path)), filepath.Join("devices", path))
}

// nic creates class/net/<name> pointing at a device directory.
func (f fakeSysfs) nic(name, devicePath, typ string) {
	f.write(filepath.Join("class", "net", name, "type"), typ+"\n")
	if devicePath != "" {
		f.link(filepath.Join("class", "net", name, "device"), filepath.Join("devices", devicePath))
	}
}

func TestNames(t *testing.T) {
	f := fakeSysfs{t: t, root: t.TempDir()}

	// QEMU q35: virtio-net behind a pcie-root-port whose slot is "0-1".
	f.pciDev("pci0000:00/0000:00:1c.0", true)
	f.pciDev("pci0000:00/0000:00:1c.0/0000:01:00.0", false)
	f.write("devices/pci0000:00/0000:00:1c.0/0000:01:00.0/virtio1/x", "")
	f.write("bus/pci/slots/0-1/address", "0000:01:00\n")
	f.nic("eth0", "pci0000:00/0000:00:1c.0/0000:01:00.0/virtio1", "1")

	// QEMU i440fx: ACPI hotplug slot 3.
	f.pciDev("pci0000:00/0000:00:03.0", false)
	f.write("devices/pci0000:00/0000:00:03.0/virtio0/x", "")
	f.write("bus/pci/slots/3/address", "0000:00:03\n")
	f.nic("eth1", "pci0000:00/0000:00:03.0/virtio0", "1")

	// Dual port ConnectX in switchdev mode in slot 1: both ports.
	f.pciDev("pci0000:3a/0000:3a:00.0", false)
	f.pciDev("pci0000:3a/0000:3a:00.0/0000:3b:00.0", true)
	f.pciDev("pci0000:3a/0000:3a:00.0/0000:3b:00.1", true)
	f.write("bus/pci/slots/1/address", "0000:3b:00\n")
	f.nic("eth2", "pci0000:3a/0000:3a:00.0/0000:3b:00.1", "1")
	f.write("class/net/eth2/phys_port_name", "p1\n")
	f.nic("eth5", "pci0000:3a/0000:3a:00.0/0000:3b:00.0", "1")
	f.write("class/net/eth5/phys_port_name", "p0\n")
	// A switchdev VF representor of the first port.
	f.nic("eth6", "pci0000:3a/0000:3a:00.0/0000:3b:00.0", "1")
	f.write("class/net/eth6/phys_port_name", "pf0vf3\n")

	// Onboard NIC with an ACPI index, second port by dev_port, domain 1.
	f.pciDev("pci0001:00/0001:02:00.0", false)
	f.write("devices/pci0001:00/0001:02:00.0/acpi_index", "2\n")
	f.nic("eth3", "pci0001:00/0001:02:00.0", "1")
	f.write("class/net/eth3/dev_port", "1\n")

	// acpi_index wins over the SMBIOS index, also when it is 0.
	f.pciDev("pci0000:10/0000:10:00.0", false)
	f.write("devices/pci0000:10/0000:10:00.0/acpi_index", "0\n")
	f.write("devices/pci0000:10/0000:10:00.0/index", "5\n")
	f.nic("eth7", "pci0000:10/0000:10:00.0", "1")
	f.pciDev("pci0000:11/0000:11:00.0", false)
	f.write("devices/pci0000:11/0000:11:00.0/acpi_index", "3\n")
	f.write("devices/pci0000:11/0000:11:00.0/index", "5\n")
	f.nic("eth8", "pci0000:11/0000:11:00.0", "1")
	// SMBIOS index up to 65535; above it no onboard name.
	f.pciDev("pci0000:12/0000:12:00.0", false)
	f.write("devices/pci0000:12/0000:12:00.0/index", "65535\n")
	f.nic("eth9", "pci0000:12/0000:12:00.0", "1")
	f.pciDev("pci0000:13/0000:13:00.0", false)
	f.write("devices/pci0000:13/0000:13:00.0/index", "70000\n")
	f.nic("eth10", "pci0000:13/0000:13:00.0", "1")

	// ARI: function 0 of device 1 is function 8.
	f.pciDev("pci0000:40/0000:40:00.0", false)
	f.write("devices/pci0000:40/0000:40:00.0/ari_enabled", "1\n")
	f.pciDev("pci0000:40/0000:40:00.0/0000:41:01.0", false)
	f.nic("eth11", "pci0000:40/0000:40:00.0/0000:41:01.0", "1")

	// A slot of the parent bridge does not name the device behind it.
	f.pciDev("pci0000:50/0000:50:02.0", false)
	f.write("bus/pci/slots/7/address", "0000:50:02\n")
	f.pciDev("pci0000:50/0000:50:02.0/0000:51:00.0", false)
	f.nic("eth12", "pci0000:50/0000:50:02.0/0000:51:00.0", "1")

	// SR-IOV VF, InfiniBand and a virtual link get no names.
	f.pciDev("pci0000:3a/0000:3a:00.0/0000:3b:00.2", false)
	f.write("devices/pci0000:3a/0000:3a:00.0/0000:3b:00.2/physfn/x", "")
	f.nic("eth4", "pci0000:3a/0000:3a:00.0/0000:3b:00.2", "1")
	f.nic("ib0", "pci0000:3a/0000:3a:00.0/0000:3b:00.0", "32")
	f.nic("dummy0", "", "1")

	tests := map[string][]string{
		"eth0":   {"enp1s0"},
		"eth1":   {"ens3", "enp0s3"},
		"eth2":   {"ens1f1np1", "enp59s0f1np1"},
		"eth5":   {"ens1f0np0", "enp59s0f0np0"},
		"eth6":   {"ens1f0r3", "enp59s0f0r3"},
		"eth3":   {"eno2d1", "enP1p2s0d1"},
		"eth7":   {"eno0", "enp16s0"},
		"eth8":   {"eno3", "enp17s0"},
		"eth9":   {"eno65535", "enp18s0"},
		"eth10":  {"enp19s0"},
		"eth11":  {"enp65s0f8"},
		"eth12":  {"enp81s0"},
		"eth4":   nil,
		"ib0":    nil,
		"dummy0": nil,
		"absent": nil,
	}
	for link, want := range tests {
		if got := Names(f.root, link); !reflect.DeepEqual(got, want) {
			t.Errorf("Names(%s) = %v, want %v", link, got, want)
		}
	}
}

func TestPlan(t *testing.T) {
	f := fakeSysfs{t: t, root: t.TempDir()}
	f.pciDev("pci0000:00/0000:00:1c.0/0000:01:00.0", false)
	f.nic("eth0", "pci0000:00/0000:00:1c.0/0000:01:00.0", "1")
	f.pciDev("pci0000:00/0000:00:1c.1/0000:02:00.0", false)
	f.nic("eth1", "pci0000:00/0000:00:1c.1/0000:02:00.0", "1")
	// eth2 and eth3 both claim acpi_index 1 (a firmware bug): eno1 is
	// ambiguous. eth2 also has the slot name ens9.
	f.pciDev("pci0000:20/0000:20:00.0", false)
	f.write("devices/pci0000:20/0000:20:00.0/acpi_index", "1\n")
	f.write("bus/pci/slots/9/address", "0000:20:00\n")
	f.nic("eth2", "pci0000:20/0000:20:00.0", "1")
	f.pciDev("pci0000:21/0000:21:00.0", false)
	f.write("devices/pci0000:21/0000:21:00.0/acpi_index", "1\n")
	f.nic("eth3", "pci0000:21/0000:21:00.0", "1")
	links := []string{"lo", "eth0", "eth1", "eth2", "eth3"}

	tests := []struct {
		name   string
		wanted []string
		want   []Rename
	}{
		{name: "bm4x frr.conf names", wanted: []string{"enp1s0", "enp2s0"}, want: []Rename{{"eth0", "enp1s0"}, {"eth1", "enp2s0"}}},
		{name: "kernel names stay", wanted: []string{"eth0", "eth1"}},
		{name: "unknown name is left to the network setup", wanted: []string{"ens7", "enp2s0", "enp2s0"}, want: []Rename{{"eth1", "enp2s0"}}},
		{name: "two NICs compute the same name", wanted: []string{"eno1"}},
		{name: "one NIC computes two wanted names", wanted: []string{"ens9", "enp32s0"}, want: []Rename{{"eth2", "ens9"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Plan(f.root, links, tt.wanted); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Plan() = %v, want %v", got, tt.want)
			}
		})
	}
}
