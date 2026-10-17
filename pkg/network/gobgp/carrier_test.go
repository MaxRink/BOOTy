//go:build linux

package gobgp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	"github.com/telekom/BOOTy/pkg/network"
)

func linkUpdate(name string, flags uint32) netlink.LinkUpdate {
	u := netlink.LinkUpdate{Link: &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name}}}
	u.IfInfomsg = nl.IfInfomsg{IfInfomsg: unix.IfInfomsg{Flags: flags}}
	return u
}

// Carrier loss stops the session once, a repeated loss does nothing, the
// carrier coming back starts it once, and other interfaces are ignored.
// IFF_LOWER_UP is the carrier; IFF_UP alone is not.
func TestCarrierWatchDisableEnable(t *testing.T) {
	var calls []string
	w := &carrierWatch{
		peers: map[string]string{"ens1f0np0": "fe80::1%ens1f0np0"},
		down:  map[string]bool{},
		disable: func(_ context.Context, addr string) error {
			calls = append(calls, "disable "+addr)
			return nil
		},
		enable: func(_ context.Context, addr string) error {
			calls = append(calls, "enable "+addr)
			return nil
		},
		log: slog.Default(),
	}
	steps := []struct {
		update netlink.LinkUpdate
		want   []string
	}{
		{linkUpdate("ens1f0np0", unix.IFF_UP|unix.IFF_LOWER_UP), nil},
		{linkUpdate("ens1f0np0", unix.IFF_UP), []string{"disable fe80::1%ens1f0np0"}},
		{linkUpdate("ens1f0np0", unix.IFF_UP), []string{"disable fe80::1%ens1f0np0"}},
		{linkUpdate("eth9", 0), []string{"disable fe80::1%ens1f0np0"}},
		{linkUpdate("ens1f0np0", unix.IFF_UP|unix.IFF_LOWER_UP), []string{"disable fe80::1%ens1f0np0", "enable fe80::1%ens1f0np0"}},
		{linkUpdate("ens1f0np0", unix.IFF_UP|unix.IFF_LOWER_UP), []string{"disable fe80::1%ens1f0np0", "enable fe80::1%ens1f0np0"}},
		{netlink.LinkUpdate{}, []string{"disable fe80::1%ens1f0np0", "enable fe80::1%ens1f0np0"}},
	}
	for i, s := range steps {
		w.handleUpdate(s.update)
		if len(calls) != len(s.want) {
			t.Fatalf("step %d: calls = %v, want %v", i, calls, s.want)
		}
		for j := range s.want {
			if calls[j] != s.want[j] {
				t.Fatalf("step %d: calls = %v, want %v", i, calls, s.want)
			}
		}
	}
}

// The fast-external-failover gate: off globally means no peers; a session
// with it off, a numbered session and an interface without a peer address
// are left out.
func TestCarrierPeers(t *testing.T) {
	sessions := []network.BGPSession{
		{Name: "ens1f0np0", Interface: true, FastExternalFailover: true},
		{Name: "ens1f1np1", Interface: true, FastExternalFailover: false},
		{Name: "192.0.2.1", FastExternalFailover: true},
		{Name: "ens1f2np2", Interface: true, FastExternalFailover: true},
	}
	iface := map[string]string{"ens1f0np0": "fe80::1%ens1f0np0", "ens1f1np1": "fe80::2%ens1f1np1", "192.0.2.1": "192.0.2.1"}
	for _, tt := range []struct {
		name   string
		global bool
		want   []string
	}{{"global off", false, nil}, {"global on", true, []string{"ens1f0np0"}}} {
		got := carrierPeers(&Config{FastExternalFailover: tt.global, Sessions: sessions}, iface)
		if len(got) != len(tt.want) {
			t.Errorf("%s: peers = %v, want %v", tt.name, got, tt.want)
		}
		for _, n := range tt.want {
			if got[n] != iface[n] {
				t.Errorf("%s: peers = %v, want %v", tt.name, got, tt.want)
			}
		}
	}
}

// The BGP_PEER_MODE peers: all unnumbered peers when the setting is on.
func TestCarrierPeersNative(t *testing.T) {
	iface := map[string]string{"ens1f0np0": "fe80::1%ens1f0np0"}
	if got := carrierPeers(&Config{FastExternalFailover: true}, iface); got["ens1f0np0"] == "" {
		t.Errorf("native peers on = %v, want ens1f0np0", got)
	}
	if got := carrierPeers(&Config{}, iface); len(got) != 0 {
		t.Errorf("native peers off = %v, want none", got)
	}
}

// ignore_routes_with_linkdown is 1 for IPv4 and IPv6 of every session NIC.
func TestIgnoreLinkdownRoutes(t *testing.T) {
	root := t.TempDir()
	for _, fam := range []string{"ipv4", "ipv6"} {
		if err := os.MkdirAll(filepath.Join(root, fam, "conf", "ens1f0np0"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if errs := ignoreLinkdownRoutes(root, []string{"ens1f0np0"}); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, fam := range []string{"ipv4", "ipv6"} {
		data, err := os.ReadFile(filepath.Join(root, fam, "conf", "ens1f0np0", "ignore_routes_with_linkdown"))
		if err != nil || string(data) != "1" {
			t.Errorf("%s: %q %v, want 1", fam, data, err)
		}
	}
	if errs := ignoreLinkdownRoutes(root, []string{"missing0"}); len(errs) != 2 {
		t.Errorf("missing NIC: %d errors, want 2", len(errs))
	}
}

// Leading-edge debounce: the first event reconciles at once, an event
// within the window waits for its rest, a scheduled reconcile absorbs more
// events.
func TestDebounce(t *testing.T) {
	for _, tt := range []struct {
		pending   bool
		sinceLast time.Duration
		now       bool
		wait      time.Duration
	}{
		{false, time.Hour, true, 0},
		{false, fibDebounce, true, 0},
		{false, 50 * time.Millisecond, false, fibDebounce - 50*time.Millisecond},
		{true, time.Hour, false, 0},
	} {
		now, wait := debounce(tt.pending, tt.sinceLast)
		if now != tt.now || wait != tt.wait {
			t.Errorf("debounce(%v, %s) = %v %s, want %v %s", tt.pending, tt.sinceLast, now, wait, tt.now, tt.wait)
		}
	}
}
