//go:build linux

package gobgp

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/telekom/BOOTy/pkg/network"
)

func TestSysctlBool(t *testing.T) {
	for _, tt := range []struct {
		b    *bool
		def  bool
		want string
	}{
		{nil, true, "1"}, {nil, false, "0"},
		{new(true), false, "1"}, {new(false), true, "0"},
	} {
		if got := sysctlBool(tt.b, tt.def); got != tt.want {
			t.Errorf("sysctlBool(%v, %v) = %q, want %q", tt.b, tt.def, got, tt.want)
		}
	}
}

func TestBoolValue(t *testing.T) {
	if boolValue(nil) || boolValue(new(false)) || !boolValue(new(true)) {
		t.Fatal("boolValue: unset and false are false, true is true")
	}
}

// The netplan tunnel MTU wins; without it the VXLAN device gets the NIC MTU
// minus the VXLAN overhead.
func TestVXLANDeviceMTU(t *testing.T) {
	o := &OverlayTier{cfg: &Config{MTU: 9100}}
	if got := o.vxlanDeviceMTU(); got != 9050 {
		t.Errorf("default VXLAN MTU = %d, want 9050", got)
	}
	o.cfg.OverlayDevices.VXLANMTU = 9000
	if got := o.vxlanDeviceMTU(); got != 9000 {
		t.Errorf("netplan VXLAN MTU = %d, want 9000", got)
	}
}

// The netplan MAC, mac-learning and arp-proxy go onto the VXLAN device;
// unset learning and proxy are off, and a bad MAC fails.
func TestApplyVXLANSettings(t *testing.T) {
	o := &OverlayTier{cfg: &Config{OverlayDevices: network.OverlayDevices{
		VXLANMAC: "02:54:c0:00:02:13", VXLANLearning: new(true), VXLANARPProxy: new(true),
	}}}
	v := &netlink.Vxlan{Learning: false, Proxy: false}
	if err := o.applyVXLANSettings(v); err != nil {
		t.Fatal(err)
	}
	if v.HardwareAddr.String() != "02:54:c0:00:02:13" || !v.Learning || !v.Proxy {
		t.Errorf("vxlan = mac %s learning %v proxy %v", v.HardwareAddr, v.Learning, v.Proxy)
	}
	o.cfg.OverlayDevices = network.OverlayDevices{}
	v = &netlink.Vxlan{Learning: true, Proxy: true}
	if err := o.applyVXLANSettings(v); err != nil {
		t.Fatal(err)
	}
	if v.HardwareAddr != nil || v.Learning || v.Proxy {
		t.Errorf("unset settings: mac %s learning %v proxy %v, want none, off, off", v.HardwareAddr, v.Learning, v.Proxy)
	}
	o.cfg.OverlayDevices.VXLANMAC = "02:54"
	if err := o.applyVXLANSettings(&netlink.Vxlan{}); err == nil {
		t.Error("bad VXLAN MAC accepted")
	}
}

// The forwarding defaults of the legacy ramdisk: IPv4 on unless turned
// off, IPv6 off unless turned on; rp_filter always 0.
func TestForwardingSysctls(t *testing.T) {
	for _, tt := range []struct {
		ipv4, ipv6 *bool
		v4, v6     string
	}{
		{nil, nil, "1", "0"},
		{new(false), new(true), "0", "1"},
		{new(true), new(false), "1", "0"},
	} {
		got := forwardingSysctls(tt.ipv4, tt.ipv6)
		if got["/proc/sys/net/ipv4/ip_forward"] != tt.v4 || got["/proc/sys/net/ipv6/conf/all/forwarding"] != tt.v6 ||
			got["/proc/sys/net/ipv4/conf/all/rp_filter"] != "0" || got["/proc/sys/net/ipv4/conf/default/rp_filter"] != "0" {
			t.Errorf("forwardingSysctls(%v, %v) = %v, want ip_forward %s, IPv6 forwarding %s", tt.ipv4, tt.ipv6, got, tt.v4, tt.v6)
		}
	}
}

// Setup applies netplan "accept-ra: false" (ACCEPT_RA_OFF) to the listed
// NICs and leaves the others alone.
func TestDisableAcceptRA(t *testing.T) {
	orig := procSysNet
	t.Cleanup(func() { procSysNet = orig })
	procSysNet = t.TempDir()
	for _, nic := range []string{"ens1f0np0", "ens1f1np1"} {
		dir := filepath.Join(procSysNet, "ipv6", "conf", nic)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "accept_ra"), []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	u := &UnderlayTier{cfg: &Config{NoAcceptRA: []string{"ens1f0np0", "missing0"}}, log: slog.Default()}
	u.disableAcceptRA()
	for nic, want := range map[string]string{"ens1f0np0": "0", "ens1f1np1": "1"} {
		b, err := os.ReadFile(filepath.Join(procSysNet, "ipv6", "conf", nic, "accept_ra"))
		if err != nil || string(b) != want {
			t.Errorf("%s accept_ra = %q (%v), want %s", nic, b, err, want)
		}
	}
}

// NewConfig passes the netplan device settings and the forwarding options
// to the GoBGP stack.
func TestNewConfigDeviceOptions(t *testing.T) {
	d := network.OverlayDevices{VXLANMTU: 9000, BridgeSTP: new(false), PortHairpin: new(true)}
	cfg, err := NewConfig(&network.Config{
		UnderlayIP: "10.0.0.1", ASN: 65000, ProvisionVNI: 4000, BGPPeerMode: network.PeerModeUnnumbered, EVPNType5Only: true,
		OverlayDevices: d, NoAcceptRA: []string{"ens1f0np0"}, EmitLLDP: []string{"ens1f1np1"},
		IPForwarding: new(false), IPv6Forwarding: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.OverlayDevices, d) || !reflect.DeepEqual(cfg.NoAcceptRA, []string{"ens1f0np0"}) ||
		!reflect.DeepEqual(cfg.EmitLLDP, []string{"ens1f1np1"}) ||
		cfg.IPForwarding == nil || *cfg.IPForwarding || cfg.IPv6Forwarding == nil || !*cfg.IPv6Forwarding {
		t.Fatalf("config = devices %+v noAcceptRA %v emitLLDP %v forwarding %v %v", cfg.OverlayDevices, cfg.NoAcceptRA, cfg.EmitLLDP, cfg.IPForwarding, cfg.IPv6Forwarding)
	}
}
