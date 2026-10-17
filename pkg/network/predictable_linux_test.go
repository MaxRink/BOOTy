//go:build linux

package network

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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
	// The NICs that have the names as predictable names end the wait.
	start := time.Now()
	if err := ApplyPredictableNames(context.Background(), &Config{InterfaceNames: []string{"enp1s0", "enp2s0"}, NICWait: 10 * time.Second}); err != nil {
		t.Fatalf("ApplyPredictableNames() = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %s although eth0 and eth1 have the predictable names", d)
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

// The configured NICs that do not exist yet are waited for (they appear
// late, like containerlab veths or a slow driver), up to NICWait; the
// VXLAN link, which BOOTy creates, is not waited for.
func TestApplyPredictableNamesWaitsForNICs(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()
	exists, interval := linkExists, nicWaitInterval
	t.Cleanup(func() { linkExists, nicWaitInterval = exists, interval })
	sleepFunc = func(time.Duration) {}
	nicWaitInterval = time.Millisecond
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	var mu sync.Mutex
	present := map[string]bool{"ens1f0np0": true}
	lists := 0
	linkExists = func(name string) bool { mu.Lock(); defer mu.Unlock(); return present[name] }
	linkList = func() ([]netlink.Link, error) {
		mu.Lock()
		defer mu.Unlock()
		if lists++; lists == 3 {
			present["ens1f1np1"] = true
		}
		links := make([]netlink.Link, 0, len(present))
		for name := range present {
			links = append(links, &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name, HardwareAddr: mac}})
		}
		return links, nil
	}
	c := &Config{InterfaceNames: []string{"ens1f0np0", "ens1f1np1"}, VXLANLink: "dum.underlay", NICWait: 10 * time.Second}
	start := time.Now()
	if err := ApplyPredictableNames(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if !linkExists("ens1f1np1") || time.Since(start) > 5*time.Second {
		t.Fatalf("returned after %s before ens1f1np1 appeared (lists %d)", time.Since(start), lists)
	}

	// A NIC that never appears ends the wait after NICWait, without error.
	c = &Config{InterfaceNames: []string{"ens1f0np0", "ens9f9"}, NICWait: 20 * time.Millisecond}
	start = time.Now()
	if err := ApplyPredictableNames(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 20*time.Millisecond || d > 5*time.Second {
		t.Fatalf("wait for a missing NIC took %s, want NICWait", d)
	}
}

// Names that BOOTy creates or gives itself are not waited for: bond0 of a
// netplan bond, a netplan set-name target. A NIC that never appears ends
// the wait after NICWait with a warning; a canceled context ends it at
// once with an error.
func TestWaitForConfiguredNICsProducedAndEnd(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()
	exists, interval := linkExists, nicWaitInterval
	t.Cleanup(func() { linkExists, nicWaitInterval = exists, interval })
	sleepFunc = func(time.Duration) {}
	nicWaitInterval = time.Millisecond
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	linkExists = func(name string) bool { return name == "eth0" || name == "eth1" }
	linkList = func() ([]netlink.Link, error) {
		return []netlink.Link{
			&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", HardwareAddr: mac}},
			&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth1", HardwareAddr: mac}},
		}, nil
	}
	for name, c := range map[string]*Config{
		"bond":     {StaticIface: "bond0", BondInterfaces: "eth0,eth1", NICWait: 10 * time.Second},
		"set-name": {InterfaceNames: []string{"hbn"}, LinkRenames: []LinkMatch{{Name: "eth0", SetName: "hbn"}}, NICWait: 10 * time.Second},
	} {
		start := time.Now()
		if err := ApplyPredictableNames(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: waited %s for a name that BOOTy gives itself", name, d)
		}
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if err := ApplyPredictableNames(context.Background(), &Config{InterfaceNames: []string{"ens9f9"}, NICWait: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "configured NICs are missing after the wait") || !strings.Contains(buf.String(), "ens9f9") {
		t.Fatalf("log = %s, want the missing NIC at WARN", buf.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	err := ApplyPredictableNames(ctx, &Config{InterfaceNames: []string{"ens9f9"}, NICWait: 10 * time.Second})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("canceled wait = %v after %s, want an error at once", err, time.Since(start))
	}
}
