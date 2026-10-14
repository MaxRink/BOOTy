//go:build linux

package network

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
)

// fakeNICSysfs gives eth0 the path name enp1s0 and eth1 enp2s0.
func fakeNICSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for i, nic := range []string{"eth0", "eth1"} {
		dev := filepath.Join(root, "devices", "pci0000:00", "0000:00:1c."+string(rune('0'+i)), "0000:0"+string(rune('1'+i))+":00.0")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		netDir := filepath.Join(root, "class", "net", nic)
		if err := os.MkdirAll(netDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(netDir, "type"), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dev, filepath.Join(netDir, "device")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestApplyPredictableNames(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()
	oldSysfs := predictableSysfs
	predictableSysfs = fakeNICSysfs(t)
	byName, down, setName, up, exists := renameLinkByName, renameLinkSetDown, renameLinkSetName, renameLinkSetUp, linkExists
	t.Cleanup(func() {
		predictableSysfs = oldSysfs
		renameLinkByName, renameLinkSetDown, renameLinkSetName, renameLinkSetUp, linkExists = byName, down, setName, up, exists
	})
	present := map[string]bool{"eth0": true, "eth1": true}
	linkExists = func(name string) bool { return present[name] }
	listed := false
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	linkList = func() ([]netlink.Link, error) {
		listed = true
		return []netlink.Link{
			&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", HardwareAddr: mac}},
			&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth1", HardwareAddr: mac}},
		}, nil
	}
	var renamed []string
	renameLinkByName = func(name string) (netlink.Link, error) {
		return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name}}, nil
	}
	renameLinkSetDown = func(netlink.Link) error { return nil }
	renameLinkSetUp = func(netlink.Link) error { return nil }
	renameLinkSetName = func(l netlink.Link, name string) error {
		renamed = append(renamed, l.Attrs().Name+">"+name)
		return nil
	}

	// bm4x frr.conf: neighbor enp1s0 interface, neighbor enp2s0 interface.
	if err := ApplyPredictableNames(context.Background(), &Config{InterfaceNames: []string{"enp1s0", "enp2s0"}}); err != nil {
		t.Fatalf("ApplyPredictableNames() = %v", err)
	}
	if len(renamed) != 2 || !reflect.DeepEqual(map[string]bool{renamed[0]: true, renamed[1]: true},
		map[string]bool{"eth0>enp1s0": true, "eth1>enp2s0": true}) {
		t.Fatalf("renames = %v, want eth0>enp1s0 and eth1>enp2s0", renamed)
	}

	// A config with the kernel names renames nothing and does not even
	// list the NICs.
	renamed, listed = nil, false
	if err := ApplyPredictableNames(context.Background(), &Config{StaticIface: "eth0", BondInterfaces: "eth0,eth1"}); err != nil {
		t.Fatalf("ApplyPredictableNames() = %v", err)
	}
	if len(renamed) != 0 || listed {
		t.Fatalf("renames = %v, NICs listed = %v, want none and false", renamed, listed)
	}
}

func TestConfiguredInterfaceNames(t *testing.T) {
	c := &Config{
		StaticIface:    "enp1s0",
		InterfaceNames: []string{"enp1s0", "enp2s0"},
		BGPInterfaces:  "enp2s0,ens1f0np0",
		LinkRenames:    []LinkMatch{{Name: "enp*s4", SetName: "hbn"}, {Name: "eno1", SetName: "mgmt"}},
		StaticMatch:    &LinkMatch{Name: "ens2f0"},
		Interfaces:     []string{"auto-detect"},
	}
	want := []string{"enp1s0", "enp2s0", "ens1f0np0", "eno1", "ens2f0"}
	if got := ConfiguredInterfaceNames(c); !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfiguredInterfaceNames() = %v, want %v", got, want)
	}
}
