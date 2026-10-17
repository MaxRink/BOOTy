//go:build linux

package sriov

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

// fakeSysfs builds a /sys/class/net tree with parent devices under
// devices/, like the kernel: the netdev "device" link points at the device
// directory, and subsystem, driver and virtfn<N> are links.
type fakeSysfs struct {
	t    *testing.T
	root string
}

func newFakeSysfs(t *testing.T) *fakeSysfs {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"class/net", "devices"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeSysfs{t: t, root: root}
}

func (f *fakeSysfs) sysNet() string { return filepath.Join(f.root, "class/net") }

type fakeDev struct {
	bus, addr, vendor, device, driver string
	totalVFs, numVFs, numa            string
	vfs                               []string
	physfn                            bool
}

func (f *fakeSysfs) device(d fakeDev) string {
	f.t.Helper()
	dir := filepath.Join(f.root, "devices", d.addr)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.write(dir, "vendor", d.vendor)
	f.write(dir, "device", d.device)
	f.write(dir, "numa_node", d.numa)
	f.write(dir, "sriov_totalvfs", d.totalVFs)
	f.write(dir, "sriov_numvfs", d.numVFs)
	f.link(filepath.Join(dir, "subsystem"), "../../bus/"+d.bus)
	if d.driver != "" {
		f.link(filepath.Join(dir, "driver"), "../../bus/"+d.bus+"/drivers/"+d.driver)
	}
	for i, vf := range d.vfs {
		f.link(filepath.Join(dir, fmt.Sprintf("virtfn%d", i)), "../"+vf)
	}
	if d.physfn {
		f.link(filepath.Join(dir, "physfn"), "../0000:00:00.0")
	}
	return dir
}

func (f *fakeSysfs) netdev(name, addr string, attrs map[string]string) {
	f.t.Helper()
	dir := filepath.Join(f.sysNet(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if addr != "" {
		f.link(filepath.Join(dir, "device"), "../../../devices/"+addr)
	}
	for k, v := range attrs {
		f.write(dir, k, v)
	}
}

func (f *fakeSysfs) write(dir, name, value string) {
	f.t.Helper()
	if value == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSysfs) link(path, target string) {
	f.t.Helper()
	if err := os.Symlink(target, path); err != nil {
		f.t.Fatal(err)
	}
}

// fakeDevlink answers per devlink handle; a missing handle is
// ErrNotSupported like a device without devlink.
type fakeDevlink struct {
	eswitch    map[string]Eswitch
	eswitchErr map[string]error
	info       map[string]*DevlinkInfo
}

func (d *fakeDevlink) Eswitch(bus, device string) (Eswitch, error) {
	h := bus + "/" + device
	if err := d.eswitchErr[h]; err != nil {
		return Eswitch{}, err
	}
	if e, ok := d.eswitch[h]; ok {
		return e, nil
	}
	return Eswitch{}, fmt.Errorf("eswitch %s: %w", h, ErrNotSupported)
}

func (d *fakeDevlink) Info(bus, device string) (*DevlinkInfo, error) {
	h := bus + "/" + device
	if i, ok := d.info[h]; ok {
		c := *i
		return &c, nil
	}
	return nil, fmt.Errorf("info %s: %w", h, ErrNotSupported)
}

// hostFixture is a host with a dual-port ConnectX-6 Dx (one port with two
// VFs and a VF representor), a dual-port X710 without devlink eSwitch, an
// IPoIB port, a VF netdev and a virtio NIC.
func hostFixture(t *testing.T) (*fakeSysfs, *fakeDevlink) {
	t.Helper()
	f := newFakeSysfs(t)
	f.device(fakeDev{bus: "pci", addr: "0000:3b:00.0", vendor: "0x15b3", device: "0x101d", driver: "mlx5_core",
		totalVFs: "64", numVFs: "2", numa: "0", vfs: []string{"0000:3b:00.2", "0000:3b:00.3"}})
	f.device(fakeDev{bus: "pci", addr: "0000:3b:00.1", vendor: "0x15b3", device: "0x101d", driver: "mlx5_core",
		totalVFs: "64", numVFs: "0", numa: "0"})
	f.device(fakeDev{bus: "pci", addr: "0000:3b:00.2", vendor: "0x15b3", device: "0x101e", driver: "mlx5_core", physfn: true})
	f.device(fakeDev{bus: "pci", addr: "0000:5e:00.0", vendor: "0x8086", device: "0x1572", driver: "i40e",
		totalVFs: "64", numVFs: "0", numa: "1"})
	f.device(fakeDev{bus: "pci", addr: "0000:5e:00.1", vendor: "0x8086", device: "0x1572", driver: "i40e",
		totalVFs: "64", numVFs: "0", numa: "1"})
	f.device(fakeDev{bus: "pci", addr: "0000:af:00.0", vendor: "0x15b3", device: "0x1021", driver: "mlx5_core",
		totalVFs: "0", numa: "-1"})
	f.device(fakeDev{bus: "virtio", addr: "virtio0", driver: "virtio_net"})

	eth := map[string]string{"type": "1"}
	f.netdev("ens1f0np0", "0000:3b:00.0", map[string]string{"type": "1", "address": "b8:ce:f6:00:00:01",
		"phys_port_name": "p0", "phys_switch_id": "f6cecb0000b8"})
	f.netdev("ens1f1np1", "0000:3b:00.1", map[string]string{"type": "1", "phys_port_name": "p1"})
	f.netdev("pf0vf0", "0000:3b:00.0", map[string]string{"type": "1", "phys_port_name": "pf0vf0"})
	f.netdev("ens1f0v0", "0000:3b:00.2", eth)
	f.netdev("ens2f0", "0000:5e:00.0", eth)
	f.netdev("ens2f1", "0000:5e:00.1", eth)
	f.netdev("ib0", "0000:af:00.0", map[string]string{"type": "32"})
	f.netdev("eth0", "virtio0", eth)
	f.netdev("lo", "", eth)

	dl := &fakeDevlink{
		eswitch: map[string]Eswitch{
			"pci/0000:3b:00.0": {Capability: CapSupported, Mode: EswitchSwitchdev, InlineMode: "none", EncapMode: "basic"},
			"pci/0000:3b:00.1": {Capability: CapSupported, Mode: EswitchLegacy},
			"pci/0000:af:00.0": {Capability: CapSupported, Mode: EswitchLegacy},
		},
		eswitchErr: map[string]error{},
		info: map[string]*DevlinkInfo{
			"pci/0000:3b:00.0": {Handle: "pci/0000:3b:00.0", Driver: "mlx5_core", SerialNumber: "MT2116X00001",
				Running: []Version{{"fw.version", "22.39.1002"}}, Stored: []Version{{"fw.version", "22.41.1000"}}},
			"pci/0000:5e:00.0": {Handle: "pci/0000:5e:00.0", Driver: "i40e", Running: []Version{{"fw.mgmt", "9.20.71847"}}},
		},
	}
	return f, dl
}

func newTestInspector(f *fakeSysfs, dl Devlink) *Inspector {
	return &Inspector{
		SysNet:  f.sysNet(),
		Devlink: dl,
		Drvinfo: func(iface string) (string, string, error) {
			if iface == "ens1f1np1" {
				return "mlx5_core", "22.39.1002 (MT_0000000359)", nil
			}
			return "", "", errors.New("no drvinfo in the fixture")
		},
		VFs: func(iface string) ([]netlink.VfInfo, error) {
			if iface != "ens1f0np0" {
				return nil, fmt.Errorf("no VFs on %s", iface)
			}
			mac, _ := net.ParseMAC("02:00:00:00:00:01")
			return []netlink.VfInfo{
				{ID: 0, Mac: mac, Vlan: 100, Qos: 3, Spoofchk: true, Trust: 1, MaxTxRate: 1000, LinkState: netlink.VF_LINK_STATE_ENABLE},
				{ID: 1, Spoofchk: false, LinkState: netlink.VF_LINK_STATE_AUTO},
			}, nil
		},
	}
}

func pfByName(t *testing.T, pfs []PF, name string) PF {
	t.Helper()
	for _, pf := range pfs {
		if pf.Interface == name {
			return pf
		}
	}
	t.Fatalf("PF %s not reported; got %v", name, ifaceNames(pfs))
	return PF{}
}

func ifaceNames(pfs []PF) []string {
	names := make([]string, 0, len(pfs))
	for _, pf := range pfs {
		names = append(names, pf.Interface)
	}
	return names
}

func TestDiscoverReportsOnlyPFPorts(t *testing.T) {
	f, dl := hostFixture(t)
	pfs, err := newTestInspector(f, dl).Discover()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ifaceNames(pfs), ",")
	// pf0vf0 is a representor, ens1f0v0 a VF, lo has no device.
	if want := "ens1f0np0,ens1f1np1,ens2f0,ens2f1,eth0,ib0"; got != want {
		t.Fatalf("PF ports = %s, want %s", got, want)
	}
}

func TestDiscoverReadsSysfsAndDevlink(t *testing.T) {
	f, dl := hostFixture(t)
	pfs, err := newTestInspector(f, dl).Discover()
	if err != nil {
		t.Fatal(err)
	}
	p0 := pfByName(t, pfs, "ens1f0np0")
	want := PF{
		Interface: "ens1f0np0", MAC: "b8:ce:f6:00:00:01", Bus: "pci", PCIAddress: "0000:3b:00.0",
		VendorID: "15b3", DeviceID: "101d", Driver: "mlx5_core", Firmware: "22.39.1002", NUMANode: 0,
		LinkType: "eth", SRIOVCapable: true, TotalVFs: 64, NumVFs: 2,
		Eswitch:      Eswitch{Capability: CapSupported, Mode: EswitchSwitchdev, InlineMode: "none", EncapMode: "basic"},
		PhysSwitchID: "f6cecb0000b8", PhysPortName: "p0", PortCount: 2, VFLAG: CapSupported,
	}
	if diff := comparePF(&p0, &want); diff != "" {
		t.Fatalf("ens1f0np0: %s", diff)
	}
	if !p0.Devlink.PendingReset() {
		t.Fatal("stored fw.version differs from running: PendingReset must be true")
	}
	if len(p0.VFs) != 2 || p0.VFs[0].PCIAddress != "0000:3b:00.2" || p0.VFs[0].MAC != "02:00:00:00:00:01" ||
		p0.VFs[0].VLAN != 100 || p0.VFs[0].QoS != 3 || !p0.VFs[0].Trust || !p0.VFs[0].SpoofChk ||
		p0.VFs[0].MaxTxRate != 1000 || p0.VFs[0].LinkState != "enable" || p0.VFs[1].LinkState != "auto" ||
		p0.VFs[1].SpoofChk || p0.VFs[1].PCIAddress != "0000:3b:00.3" {
		t.Fatalf("VFs = %+v", p0.VFs)
	}
	// The drvinfo firmware wins over devlink, and a port without VFs does
	// not query rtnetlink.
	p1 := pfByName(t, pfs, "ens1f1np1")
	if p1.Firmware != "22.39.1002 (MT_0000000359)" || len(p1.Warnings) != 0 || p1.Eswitch.Mode != EswitchLegacy {
		t.Fatalf("ens1f1np1 = %+v", p1)
	}
}

func TestDiscoverCapabilities(t *testing.T) {
	f, dl := hostFixture(t)
	dl.eswitchErr["pci/0000:5e:00.1"] = errors.New("netlink: EBUSY")
	pfs, err := newTestInspector(f, dl).Discover()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		iface     string
		eswitch   Capability
		vfLAG     Capability
		linkType  string
		numa      int
		capable   bool
		ports     int
		firmware  string
		warnings  int
		hasDevlnk bool
	}{
		// No devlink eSwitch on i40e: unsupported, no warning.
		{"ens2f0", CapUnsupported, CapUnsupported, "eth", 1, true, 2, "9.20.71847", 1, true},
		// Another devlink error is unknown and a warning.
		{"ens2f1", CapUnknown, CapUnsupported, "eth", 1, true, 2, "", 2, false},
		// SR-IOV disabled in the firmware: sriov_totalvfs 0.
		{"ib0", CapSupported, CapUnsupported, "ib", -1, false, 1, "", 1, false},
		{"eth0", CapUnsupported, CapUnsupported, "eth", -1, false, 1, "", 1, false},
	}
	for _, c := range cases {
		pf := pfByName(t, pfs, c.iface)
		if pf.Eswitch.Capability != c.eswitch || pf.VFLAG != c.vfLAG || pf.LinkType != c.linkType ||
			pf.NUMANode != c.numa || pf.SRIOVCapable != c.capable || pf.PortCount != c.ports ||
			pf.Firmware != c.firmware || len(pf.Warnings) != c.warnings || (pf.Devlink != nil) != c.hasDevlnk {
			t.Errorf("%s = %+v", c.iface, pf)
		}
	}
}

func TestDiscoverFailsWithoutSysNet(t *testing.T) {
	in := &Inspector{SysNet: filepath.Join(t.TempDir(), "missing")}
	if _, err := in.Discover(); err == nil {
		t.Fatal("Discover without /sys/class/net returned no error")
	}
}

func TestDiscoverWithoutOptionalReaders(t *testing.T) {
	f, _ := hostFixture(t)
	pfs, err := (&Inspector{SysNet: f.sysNet()}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	p0 := pfByName(t, pfs, "ens1f0np0")
	if p0.Eswitch.Capability != CapUnknown || p0.Firmware != "" || p0.VFs != nil || len(p0.Warnings) != 0 {
		t.Fatalf("without devlink, drvinfo and rtnetlink = %+v", p0)
	}
}

func TestLinkTypeAndPCIID(t *testing.T) {
	for in, want := range map[string]string{"1": "eth", "32": "ib", "772": "arphrd-772", "": ""} {
		if got := linkType(in); got != want {
			t.Errorf("linkType(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"0x15B3": "15b3", "8086": "8086", "0X14e4": "14e4"} {
		if got := pciID(in); got != want {
			t.Errorf("pciID(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[uint32]string{0: "auto", 1: "enable", 2: "disable", 7: "state-7"} {
		if got := vfLinkState(in); got != want {
			t.Errorf("vfLinkState(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRepresentorNames(t *testing.T) {
	for name, want := range map[string]bool{
		"pf0vf0": true, "pf1vf63": true, "c1pf0sf2": true, "pf0sf1": true,
		"p0": false, "p1": false, "pf0": false, "": false, "vf0": false,
	} {
		if got := representorName.MatchString(name); got != want {
			t.Errorf("representor %q = %v, want %v", name, got, want)
		}
	}
}

// comparePF compares the scalar fields that the report contract names.
func comparePF(got, want *PF) string {
	g := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%d|%s|%v|%d|%d|%+v|%s|%s|%d|%s",
		got.Interface, got.MAC, got.Bus, got.PCIAddress, got.VendorID, got.DeviceID, got.Driver, got.Firmware,
		got.NUMANode, got.LinkType, got.SRIOVCapable, got.TotalVFs, got.NumVFs, got.Eswitch, got.PhysSwitchID,
		got.PhysPortName, got.PortCount, got.VFLAG)
	w := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%d|%s|%v|%d|%d|%+v|%s|%s|%d|%s",
		want.Interface, want.MAC, want.Bus, want.PCIAddress, want.VendorID, want.DeviceID, want.Driver, want.Firmware,
		want.NUMANode, want.LinkType, want.SRIOVCapable, want.TotalVFs, want.NumVFs, want.Eswitch, want.PhysSwitchID,
		want.PhysPortName, want.PortCount, want.VFLAG)
	if g != w {
		return "got  " + g + "\nwant " + w
	}
	return ""
}
