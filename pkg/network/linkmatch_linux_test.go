//go:build linux

package network

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

// ResolveLinkMatch on injected NICs: a unique glob or MAC selects one NIC,
// a match of several NICs is an error (the caller then uses the first NIC).
func TestResolveLinkMatchInjectedLinks(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()
	mac := func(s string) net.HardwareAddr {
		hw, _ := net.ParseMAC(s)
		return hw
	}
	dev := func(name, m string) netlink.Link {
		return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name, HardwareAddr: mac(m)}}
	}
	linkList = func() ([]netlink.Link, error) {
		return []netlink.Link{
			dev("lo", "00:00:00:00:00:00"),
			dev("eth0", "02:00:00:00:00:01"),
			dev("enp1s4", "02:00:00:00:00:02"),
			dev("enp2s4", "02:00:00:00:00:03"),
			&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "enp9s4"}}, // no MAC: not a NIC
			// A ContainerLab NIC is a renamed veth.
			&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "ens1f0np0", HardwareAddr: mac("02:00:00:00:00:04")}},
		}, nil
	}
	for _, tt := range []struct {
		m    LinkMatch
		want string
	}{
		{LinkMatch{Name: "enp1*"}, "enp1s4"},
		{LinkMatch{MAC: "02:00:00:00:00:03"}, "enp2s4"},
		{LinkMatch{Name: "enp*", MAC: "02:00:00:00:00:02"}, "enp1s4"},
		{LinkMatch{Name: "ens1f0*"}, "ens1f0np0"},
	} {
		if got, err := ResolveLinkMatch(context.Background(), tt.m); err != nil || got != tt.want {
			t.Errorf("ResolveLinkMatch(%+v) = %q, %v, want %q", tt.m, got, err, tt.want)
		}
	}
	for _, m := range []LinkMatch{{Name: "enp*"}, {Name: "wl*"}} {
		if got, err := ResolveLinkMatch(context.Background(), m); err == nil {
			t.Errorf("ResolveLinkMatch(%+v) = %q, want an error", m, got)
		}
	}
}

// A set-name that BOOTy would not see as a NIC fails before the rename.
func TestApplyLinkRenamesRejectsExcludedNames(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()
	listed := false
	linkList = func() ([]netlink.Link, error) {
		listed = true
		return nil, nil
	}
	for _, name := range []string{"local0", "br-prov", "bond1", "vx.3000"} {
		if err := ApplyLinkRenames(context.Background(), []LinkMatch{{Name: "enp1*", SetName: name}}); err == nil {
			t.Errorf("set-name %q: no error", name)
		}
	}
	if listed {
		t.Fatal("NICs were listed for a rejected set-name")
	}
}

// A failed rename undoes the renames before it, newest first.
func TestUndoRenamesJoinsErrors(t *testing.T) {
	err := undoRenames(nil, errors.New("rename failed"))
	if err == nil || err.Error() != "rename failed" {
		t.Fatalf("undoRenames without renames = %v", err)
	}
	// Undoing a rename of a link that does not exist reports both errors.
	err = undoRenames([][2]string{{"enp1s0", "booty-test-missing"}}, errors.New("rename failed"))
	if err == nil || !strings.Contains(err.Error(), "rename failed") || !strings.Contains(err.Error(), "undo rename enp1s0 to booty-test-missing") {
		t.Fatalf("undoRenames = %v, want both errors", err)
	}
}

// A canceled setup stops the NIC detection of a rename at once.
func TestApplyLinkRenamesCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ApplyLinkRenames(ctx, []LinkMatch{{Name: "enp1*", SetName: "hbn"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ApplyLinkRenames = %v, want context.Canceled", err)
	}
}

// If the NIC does not come up after the rename, renameLink restores the old
// name and brings the NIC up again: ApplyLinkRenames rolls back only the
// renames that succeeded.
func TestRenameLinkRestoresNameWhenSetUpFails(t *testing.T) {
	byName, down, setName, up := renameLinkByName, renameLinkSetDown, renameLinkSetName, renameLinkSetUp
	t.Cleanup(func() {
		renameLinkByName, renameLinkSetDown, renameLinkSetName, renameLinkSetUp = byName, down, setName, up
	})
	link := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "enp1s4", Flags: net.FlagUp}}
	var calls []string
	ups := 0
	renameLinkByName = func(string) (netlink.Link, error) { return link, nil }
	renameLinkSetDown = func(netlink.Link) error { calls = append(calls, "down"); return nil }
	renameLinkSetName = func(_ netlink.Link, name string) error { calls = append(calls, "name "+name); return nil }
	renameLinkSetUp = func(netlink.Link) error {
		calls = append(calls, "up")
		if ups++; ups == 1 {
			return errors.New("up failed")
		}
		return nil
	}

	err := renameLink("enp1s4", "hbn")

	if err == nil || !strings.Contains(err.Error(), "up failed") {
		t.Fatalf("renameLink error = %v, want the set-up error", err)
	}
	if want := "down,name hbn,up,name enp1s4,up"; strings.Join(calls, ",") != want {
		t.Fatalf("calls = %s, want %s", strings.Join(calls, ","), want)
	}
}
