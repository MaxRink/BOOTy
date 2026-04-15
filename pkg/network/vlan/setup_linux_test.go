//go:build linux

package vlan

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/vishvananda/netlink"
)

func snapshotNetlinkFns() func() {
	origLinkByName := linkByName
	origLinkSetUp := linkSetUp
	origLinkSetMTU := linkSetMTU
	origLinkAdd := linkAdd
	origLinkDel := linkDel
	origParseAddr := parseAddr
	origAddrAdd := addrAdd
	origRouteAdd := routeAdd
	return func() {
		linkByName = origLinkByName
		linkSetUp = origLinkSetUp
		linkSetMTU = origLinkSetMTU
		linkAdd = origLinkAdd
		linkDel = origLinkDel
		parseAddr = origParseAddr
		addrAdd = origAddrAdd
		routeAdd = origRouteAdd
	}
}

func TestSetup_AppliesCustomNameAndMTU(t *testing.T) {
	restore := snapshotNetlinkFns()
	defer restore()

	parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 10}}
	var mtuSet int32

	linkByName = func(name string) (netlink.Link, error) {
		if name == "eth0" {
			return parent, nil
		}
		return nil, errors.New("not found")
	}
	linkSetUp = func(netlink.Link) error { return nil }
	linkAdd = func(link netlink.Link) error {
		v, ok := link.(*netlink.Vlan)
		if !ok {
			t.Fatalf("expected vlan link, got %T", link)
		}
		if v.Name != "mgmt0" {
			t.Fatalf("vlan name = %q, want %q", v.Name, "mgmt0")
		}
		return nil
	}
	linkSetMTU = func(link netlink.Link, mtu int) error {
		if link.Attrs().Name != "mgmt0" {
			t.Fatalf("mtu set on %q, want %q", link.Attrs().Name, "mgmt0")
		}
		if mtu != 9000 {
			t.Fatalf("mtu = %d, want %d", mtu, 9000)
		}
		atomic.AddInt32(&mtuSet, 1)
		return nil
	}

	name, err := Setup(&Config{ID: 100, Parent: "eth0", Name: "mgmt0", MTU: 9000})
	if err != nil {
		t.Fatalf("Setup() error: %v", err)
	}
	if name != "mgmt0" {
		t.Fatalf("name = %q, want %q", name, "mgmt0")
	}
	if atomic.LoadInt32(&mtuSet) != 1 {
		t.Fatalf("expected MTU to be applied once")
	}
}

func TestSetupAll_RollbackUsesCustomName(t *testing.T) {
	restore := snapshotNetlinkFns()
	defer restore()

	parent0 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 10}}
	parent1 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1", Index: 11}}
	created := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "mgmt0", Index: 20}}
	var deleted int32

	linkByName = func(name string) (netlink.Link, error) {
		switch name {
		case "eth0":
			return parent0, nil
		case "eth1":
			return parent1, nil
		case "mgmt0":
			return created, nil
		default:
			return nil, errors.New("not found")
		}
	}
	linkSetUp = func(netlink.Link) error { return nil }
	linkSetMTU = func(netlink.Link, int) error { return nil }
	linkAdd = func(link netlink.Link) error {
		if link.Attrs().Name == "bad1" {
			return errors.New("boom")
		}
		return nil
	}
	linkDel = func(link netlink.Link) error {
		if link.Attrs().Name == "mgmt0" {
			atomic.AddInt32(&deleted, 1)
		}
		return nil
	}

	_, err := SetupAll([]Config{
		{ID: 100, Parent: "eth0", Name: "mgmt0"},
		{ID: 200, Parent: "eth1", Name: "bad1"},
	})
	if err == nil {
		t.Fatal("expected setup failure")
	}
	if atomic.LoadInt32(&deleted) != 1 {
		t.Fatalf("expected rollback teardown for custom interface name")
	}
}

func TestTeardownAll_UsesCustomName(t *testing.T) {
	restore := snapshotNetlinkFns()
	defer restore()

	custom := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "vlan-mgmt", Index: 30}}
	var deleted int32

	linkByName = func(name string) (netlink.Link, error) {
		if name == "vlan-mgmt" {
			return custom, nil
		}
		return nil, errors.New("not found")
	}
	linkDel = func(netlink.Link) error {
		atomic.AddInt32(&deleted, 1)
		return nil
	}

	TeardownAll([]Config{{ID: 100, Parent: "eth0", Name: "vlan-mgmt"}})
	if atomic.LoadInt32(&deleted) != 1 {
		t.Fatalf("expected custom-named vlan to be deleted")
	}
}

func TestSetup_CleansUpOnMTUFailure(t *testing.T) {
	restore := snapshotNetlinkFns()
	defer restore()

	parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 10}}
	created := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "mgmt0", Index: 20}}
	var deleted int32

	linkByName = func(name string) (netlink.Link, error) {
		switch name {
		case "eth0":
			return parent, nil
		case "mgmt0":
			return created, nil
		default:
			return nil, errors.New("not found")
		}
	}
	linkSetUp = func(netlink.Link) error { return nil }
	linkAdd = func(netlink.Link) error { return nil }
	linkSetMTU = func(link netlink.Link, _ int) error {
		if link.Attrs().Name == "mgmt0" {
			return errors.New("mtu set failed")
		}
		return nil
	}
	linkDel = func(link netlink.Link) error {
		if link.Attrs().Name == "mgmt0" {
			atomic.AddInt32(&deleted, 1)
		}
		return nil
	}

	_, err := Setup(&Config{ID: 100, Parent: "eth0", Name: "mgmt0", MTU: 9000})
	if err == nil {
		t.Fatal("expected setup error")
	}
	if atomic.LoadInt32(&deleted) != 1 {
		t.Fatalf("expected partial setup cleanup")
	}
}

func TestTeardownConfig_Nil(t *testing.T) {
	if err := TeardownConfig(nil); err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestSetupAll_FailureIsFatal(t *testing.T) {
	restore := snapshotNetlinkFns()
	defer restore()

	setupErr := errors.New("interface not found")
	linkByName = func(string) (netlink.Link, error) { return nil, setupErr }

	_, err := SetupAll([]Config{{ID: 100, Parent: "eth0"}})
	if err == nil {
		t.Fatal("expected SetupAll to return an error on VLAN setup failure")
	}
	if !errors.Is(err, setupErr) {
		t.Fatalf("expected error to wrap the original error, got: %v", err)
	}
}

func TestSetupAll_HappyPath(t *testing.T) {
	restore := snapshotNetlinkFns()
	defer restore()

	parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 10}}
	linkByName = func(name string) (netlink.Link, error) {
		if name == "eth0" {
			return parent, nil
		}
		return nil, errors.New("not found")
	}
	linkSetUp = func(netlink.Link) error { return nil }
	linkAdd = func(netlink.Link) error { return nil }

	names, err := SetupAll([]Config{{ID: 100, Parent: "eth0"}})
	if err != nil {
		t.Fatalf("SetupAll() unexpected error: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("expected 1 name, got %d", len(names))
	}
}
