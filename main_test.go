//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/gobgp"
	"github.com/telekom/BOOTy/pkg/network/netplan"
	"github.com/telekom/BOOTy/pkg/provision"
	"github.com/telekom/BOOTy/pkg/realm"
	"github.com/telekom/BOOTy/pkg/runmode"
)

type teardownRecorderMode struct {
	setup    func() error
	teardown func() error
}

func (m teardownRecorderMode) Setup(context.Context, *network.Config) error {
	if m.setup == nil {
		return nil
	}
	return m.setup()
}

func (m teardownRecorderMode) WaitForConnectivity(context.Context, string, time.Duration) error {
	return nil
}

func (m teardownRecorderMode) Teardown(context.Context) error {
	if m.teardown == nil {
		return nil
	}
	return m.teardown()
}

func TestMergeNetplanConfigPreservesProvisionPrefixForSameHostMask(t *testing.T) {
	dst := &network.Config{ProvisionIP: "10.200.0.10/24"}
	src := &network.Config{ProvisionIP: "10.200.0.10/32"}

	mergeNetplanConfig(dst, src)

	if dst.ProvisionIP != "10.200.0.10/24" {
		t.Fatalf("ProvisionIP = %q, want existing /24 prefix", dst.ProvisionIP)
	}
}

// The two merge rules: a key that exists on main keeps "netplan wins" for
// CAPRF compatibility; a key that this release adds follows "native wins":
// a set native scalar wins, a non-empty native list replaces the parsed
// list as a whole.
func TestMergeNetplanConfigPrecedence(t *testing.T) {
	parsed := &network.Config{
		UnderlayIP: "192.0.2.17", VXLANName: "vx.parsed", BridgeName: "br.parsed",
		LoopbackAddrs: []string{"2001:db8::17/128", "2001:db8::18/128"},
	}
	parsed.MTU, parsed.OverlayVRFSet = 9100, true
	native := &network.Config{
		UnderlayIP: "192.0.2.99", VXLANName: "vx.native", BridgeName: "br.native", LoopbackAddrs: []string{"2001:db8::99/128"},
		MTU: 9000, OverlayVRFName: "Vrf_native", OverlayVRFSet: true,
	}
	mergeNetplanConfig(native, parsed)
	if native.MTU != 9000 || native.OverlayVRFName != "Vrf_native" {
		t.Errorf("native MTU %d, OVERLAY_VRF_NAME %q, want the native values", native.MTU, native.OverlayVRFName)
	}
	if native.UnderlayIP != "192.0.2.17" {
		t.Errorf("legacy underlay_ip = %s, want the netplan value", native.UnderlayIP)
	}
	if native.BridgeName != "br.native" {
		t.Errorf("native BRIDGE_NAME %s, want the native value", native.BridgeName)
	}
	if native.VXLANName != "vx.native" || !slices.Equal(native.LoopbackAddrs, []string{"2001:db8::99/128"}) {
		t.Errorf("native VXLAN_NAME %s, LOOPBACK_ADDRESSES %v, want the native values", native.VXLANName, native.LoopbackAddrs)
	}
	unset := &network.Config{}
	mergeNetplanConfig(unset, parsed)
	if unset.BridgeName != "br.parsed" {
		t.Errorf("unset BRIDGE_NAME %s, want the netplan bridge", unset.BridgeName)
	}
	if unset.VXLANName != "vx.parsed" || len(unset.LoopbackAddrs) != 2 || unset.MTU != 9100 || !unset.OverlayVRFSet || unset.OverlayVRFName != "" {
		t.Errorf("unset native keys = %s %v, want the parsed values", unset.VXLANName, unset.LoopbackAddrs)
	}
}

func TestMergeNetplanConfigBridgeMAC(t *testing.T) {
	tests := []struct {
		name      string
		dstBridge string
		srcBridge string
		want      string
	}{
		{"default_bridge", "", "", "02:00:00:00:10:01"},
		{"same_bridge", "", "br.provision", "02:00:00:00:10:01"},
		// The netplan bridge becomes the provision bridge (native unset),
		// so its MAC applies.
		{"netplan_bridge", "", "br_cluster", "02:00:00:00:10:01"},
		// BRIDGE_NAME names another bridge: the netplan MAC is not its MAC.
		{"native_other_bridge", "br.native", "br_cluster", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := &network.Config{BridgeName: tt.dstBridge}
			src := &network.Config{BridgeName: tt.srcBridge, BridgeMAC: "02:00:00:00:10:01"}

			mergeNetplanConfig(dst, src)

			if dst.BridgeMAC != tt.want {
				t.Fatalf("BridgeMAC = %q, want %q", dst.BridgeMAC, tt.want)
			}
		})
	}
}

func TestNetworkConfigFromMachineConfigWiresType5Only(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Network.EVPN.Type5Only = new(true)

	netCfg := networkConfigFromMachineConfig(cfg)

	if !netCfg.EVPNType5Only {
		t.Fatal("EVPN Type-5-only mode was not wired into the network stack config")
	}
}

// NETWORK_WAIT_MODE=rounds also restarts FRR after each failed round like
// wait_for_network; the preset selects it and an explicit default wins.
func TestNetworkConfigFromMachineConfigWiresLegacyFRRRecovery(t *testing.T) {
	for name, tt := range map[string]struct {
		cfg  config.MachineConfig
		want bool
	}{
		"default":               {},
		"native rounds":         {cfg: config.MachineConfig{Network: config.NetworkConfig{WaitMode: config.NetworkWaitRounds}}, want: true},
		"preset":                {cfg: config.MachineConfig{LegacyCompat: true}, want: true},
		"explicit default wins": {cfg: config.MachineConfig{LegacyCompat: true, Network: config.NetworkConfig{WaitMode: config.NetworkWaitDefault}}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.ApplyPresets()
			if got := networkConfigFromMachineConfig(&cfg).LegacyFRRRecovery; got != tt.want {
				t.Fatalf("LegacyFRRRecovery = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeProvisionIPUsesDetectedNetworkPrefix(t *testing.T) {
	got := mergeProvisionIP("10.200.0.10/32", "10.200.0.10/24")

	if got != "10.200.0.10/24" {
		t.Fatalf("mergeProvisionIP() = %q, want detected /24", got)
	}
}

func TestMergeProvisionIPUsesDetectedDifferentHost(t *testing.T) {
	got := mergeProvisionIP("10.200.0.10/24", "10.200.0.11/32")

	if got != "10.200.0.11/32" {
		t.Fatalf("mergeProvisionIP() = %q, want detected different host", got)
	}
}

func TestMergeProvisionIPUsesDetectedInvalidCIDR(t *testing.T) {
	got := mergeProvisionIP("10.200.0.10/24", "not-a-cidr")

	if got != "not-a-cidr" {
		t.Fatalf("mergeProvisionIP() = %q, want detected invalid value", got)
	}
}

func TestMergeNetplanConfigEVPNMode(t *testing.T) {
	tests := []struct {
		name                   string
		dstL2, dstType5, srcL2 bool
		dstType5Set            bool
		dstVNI, srcVNI         uint32
		srcL3VNIs              []uint32
		dstGateway             string
		wantL2, wantType5      bool
	}{
		{name: "no mode anywhere", wantL2: false},
		{name: "L2 inferred from FRR", srcL2: true, wantL2: true},
		{name: "L2 from vars kept", dstL2: true, wantL2: true},
		{name: "explicit Type-5 only wins over inferred L2", dstType5: true, srcL2: true, wantType5: true},
		{name: "explicit Type-5 only kept", dstType5: true, wantType5: true},
		{name: "L3VNI provision VNI from vars", dstVNI: 1000, srcL2: true, srcL3VNIs: []uint32{1000}, wantType5: true},
		{name: "L2VNI provision VNI from vars", dstVNI: 100, srcL2: true, srcL3VNIs: []uint32{1000}, wantL2: true},
		{name: "L3VNI provision VNI from netplan", dstVNI: 100, srcVNI: 1000, srcL2: true, srcL3VNIs: []uint32{1000}, wantType5: true},
		{name: "L3VNI without advertise-all-vni", srcVNI: 3000, srcL3VNIs: []uint32{3000}, wantType5: true},
		// CAPRF sets provision_gateway: the mode stays, the BUM FDB entry too.
		{name: "L3VNI with provision gateway", dstVNI: 1000, dstGateway: "10.0.0.1", srcL2: true, srcL3VNIs: []uint32{1000}},
		{name: "explicit L2 wins over the L3VNI default", dstL2: true, dstVNI: 3000, srcL3VNIs: []uint32{3000}, wantL2: true},
		{name: "explicit EVPN_TYPE5_ONLY=false wins over the L3VNI default", dstType5Set: true, dstVNI: 3000, srcL2: true, srcL3VNIs: []uint32{3000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := &network.Config{EVPNL2Enabled: tt.dstL2, EVPNType5Only: tt.dstType5, EVPNType5OnlySet: tt.dstType5Set || tt.dstType5, ProvisionVNI: tt.dstVNI, ProvisionGateway: tt.dstGateway}
			mergeNetplanConfig(dst, &network.Config{EVPNL2Enabled: tt.srcL2, EVPNL3VNIs: tt.srcL3VNIs, ProvisionVNI: tt.srcVNI})
			if dst.EVPNL2Enabled != tt.wantL2 || dst.EVPNType5Only != tt.wantType5 {
				t.Fatalf("EVPNL2Enabled=%v EVPNType5Only=%v, want %v %v",
					dst.EVPNL2Enabled, dst.EVPNType5Only, tt.wantL2, tt.wantType5)
			}
		})
	}
}

func TestMergeNetplanConfigOverridesStaticAddressPair(t *testing.T) {
	dst := &network.Config{StaticIP: "192.0.2.10/24", StaticIface: "eth9"}
	src := &network.Config{StaticIP: "10.1.2.3/24"}

	mergeNetplanConfig(dst, src)

	if dst.StaticIP != "10.1.2.3/24" {
		t.Fatalf("StaticIP = %q, want netplan address", dst.StaticIP)
	}
	if dst.StaticIface != "" {
		t.Fatalf("StaticIface = %q, want netplan auto-detect", dst.StaticIface)
	}
}

func TestSetupBondModeRollsBackOnSetupFailure(t *testing.T) {
	var calls []string
	bond := teardownRecorderMode{
		setup: func() error {
			calls = append(calls, "setup")
			return fmt.Errorf("setup failed")
		},
		teardown: func() error {
			calls = append(calls, "teardown")
			return nil
		},
	}

	got, err := setupBondMode(context.Background(), &network.Config{}, bond)
	if err == nil {
		t.Fatal("setupBondMode() error = nil, want setup failure")
	}
	if got != nil {
		t.Fatalf("setupBondMode() mode = %T, want nil", got)
	}
	if strings.Join(calls, ",") != "setup,teardown" {
		t.Fatalf("calls = %v, want setup then teardown", calls)
	}
}

func TestPrepareLinkLayersCreatesBondBeforeVLAN(t *testing.T) {
	previousBond := setupBondLayer
	previousVLAN := setupVLANLayer
	var calls []string
	setupBondLayer = func(_ context.Context, _ *network.Config) (network.Mode, error) {
		calls = append(calls, "bond")
		return teardownRecorderMode{}, nil
	}
	setupVLANLayer = func(v network.VLANConfig) (string, error) {
		calls = append(calls, "vlan:"+v.Parent)
		return "bond0.100", nil
	}
	t.Cleanup(func() {
		setupBondLayer = previousBond
		setupVLANLayer = previousVLAN
	})

	cfg := &network.Config{
		BondInterfaces: "eth0,eth1",
		VLANs:          []network.VLANConfig{{ID: 100, Parent: "bond0", Address: "10.0.0.2/24"}},
	}
	if _, err := prepareLinkLayers(context.Background(), cfg); err != nil {
		t.Fatalf("prepareLinkLayers: %v", err)
	}

	want := []string{"bond", "vlan:bond0"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
	if cfg.StaticIface != "bond0.100" {
		t.Fatalf("StaticIface = %q, want bond0.100", cfg.StaticIface)
	}
}

func TestPrepareLinkLayersBondOnlySelectsBond0(t *testing.T) {
	previousBond := setupBondLayer
	setupBondLayer = func(_ context.Context, _ *network.Config) (network.Mode, error) {
		return teardownRecorderMode{}, nil
	}
	t.Cleanup(func() {
		setupBondLayer = previousBond
	})

	cfg := &network.Config{BondInterfaces: "eth0,eth1"}
	if _, err := prepareLinkLayers(context.Background(), cfg); err != nil {
		t.Fatalf("prepareLinkLayers: %v", err)
	}
	if cfg.StaticIface != "bond0" {
		t.Fatalf("StaticIface = %q, want bond0", cfg.StaticIface)
	}
}

func TestLinkLayerNetworkModeTeardownCleansInnerThenLinkLayers(t *testing.T) {
	previousVLAN := teardownVLANLayer
	var calls []string
	teardownVLANLayer = func(v network.VLANConfig) error {
		calls = append(calls, fmt.Sprintf("vlan:%s.%d", v.Parent, v.ID))
		return nil
	}
	t.Cleanup(func() { teardownVLANLayer = previousVLAN })

	inner := teardownRecorderMode{teardown: func() error {
		calls = append(calls, "inner")
		return nil
	}}
	cleanup := &linkLayerCleanup{
		bond: teardownRecorderMode{teardown: func() error {
			calls = append(calls, "bond")
			return nil
		}},
		vlans: []network.VLANConfig{{ID: 100, Parent: "bond0"}},
	}

	if err := wrapLinkLayerMode(inner, cleanup).Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	want := []string{"inner", "vlan:bond0.100", "bond"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestPrepareLinkLayersRollsBackOnVLANFailure(t *testing.T) {
	previousBond := setupBondLayer
	previousVLAN := setupVLANLayer
	previousTeardownVLAN := teardownVLANLayer
	var calls []string
	setupBondLayer = func(_ context.Context, _ *network.Config) (network.Mode, error) {
		calls = append(calls, "bond")
		return teardownRecorderMode{teardown: func() error {
			calls = append(calls, "teardown-bond")
			return nil
		}}, nil
	}
	setupVLANLayer = func(v network.VLANConfig) (string, error) {
		calls = append(calls, fmt.Sprintf("vlan:%d", v.ID))
		if v.ID == 200 {
			return "", fmt.Errorf("vlan setup failed")
		}
		return fmt.Sprintf("%s.%d", v.Parent, v.ID), nil
	}
	teardownVLANLayer = func(v network.VLANConfig) error {
		calls = append(calls, fmt.Sprintf("teardown-vlan:%d", v.ID))
		return nil
	}
	t.Cleanup(func() {
		setupBondLayer = previousBond
		setupVLANLayer = previousVLAN
		teardownVLANLayer = previousTeardownVLAN
	})

	cfg := &network.Config{
		BondInterfaces: "eth0,eth1",
		VLANs: []network.VLANConfig{
			{ID: 100, Parent: "bond0"},
			{ID: 200, Parent: "bond0"},
		},
	}
	if _, err := prepareLinkLayers(context.Background(), cfg); err == nil {
		t.Fatal("prepareLinkLayers() error = nil, want VLAN setup failure")
	}
	want := []string{"bond", "vlan:100", "vlan:200", "teardown-vlan:100", "teardown-bond"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestNetworkModeWithResolversCleansModeAndLinkLayersOnResolverFailure(t *testing.T) {
	previousTeardownVLAN := teardownVLANLayer
	var calls []string
	teardownVLANLayer = func(v network.VLANConfig) error {
		calls = append(calls, fmt.Sprintf("teardown-vlan:%d", v.ID))
		return nil
	}
	t.Cleanup(func() {
		teardownVLANLayer = previousTeardownVLAN
	})

	mode := teardownRecorderMode{teardown: func() error {
		calls = append(calls, "teardown-mode")
		return nil
	}}
	cleanup := &linkLayerCleanup{
		bond: teardownRecorderMode{teardown: func() error {
			calls = append(calls, "teardown-bond")
			return nil
		}},
		vlans: []network.VLANConfig{{ID: 100, Parent: "bond0"}},
	}
	netCfg := &network.Config{DNSResolvers: "8.8.8.8\nsearch evil.example"}

	got, err := networkModeWithResolvers(context.Background(), netCfg, mode, cleanup)
	if err == nil {
		t.Fatal("networkModeWithResolvers() error = nil, want resolver validation failure")
	}
	if got != nil {
		t.Fatalf("networkModeWithResolvers() mode = %T, want nil", got)
	}
	if !strings.Contains(err.Error(), "configure initramfs DNS") {
		t.Fatalf("networkModeWithResolvers() error = %q, want DNS context", err.Error())
	}

	want := []string{"teardown-mode", "teardown-vlan:100", "teardown-bond"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestRequiresABKexec(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		preserve bool
		want     bool
	}{
		{name: "preserve existing A/B upgrade", mode: config.ImageModeAB, preserve: true, want: true},
		{name: "fresh A/B install", mode: config.ImageModeAB, preserve: false, want: false},
		{name: "whole disk ignores preserve flag", mode: config.ImageModeWholeDisk, preserve: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Provision.Image.Mode = tt.mode
			cfg.Provision.AB.PreserveExisting = tt.preserve
			if got := requiresABKexec(cfg); got != tt.want {
				t.Fatalf("requiresABKexec() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestTryKexecSkipsWhenSecureBootReEnableRequested(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.SecureBoot.ReEnable = true

	if tryKexec(cfg, false, func() (disk.ReleaseReport, error) { return disk.ReleaseReport{}, nil }) {
		t.Fatal("tryKexec returned true when secure boot re-enable requires hard reboot")
	}
}

func TestResolveKexecPath(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "boot", "vmlinuz-6.1.0"))
	mustWrite(t, filepath.Join(root, "boot", "initrd.img-6.1.0"))
	mustWrite(t, filepath.Join(root, "boot", "initramfs-6.1.0.img"))
	mustWrite(t, filepath.Join(root, "boot", "explicit-root-kernel"))
	mustWrite(t, filepath.Join(root, "vmlinuz-local"))
	mustWrite(t, filepath.Join(root, "boot", "vmlinuz-local"))
	if err := os.Mkdir(filepath.Join(root, "vmlinuz-directory"), 0o755); err != nil {
		t.Fatalf("mkdir root vmlinuz-directory: %v", err)
	}
	mustWrite(t, filepath.Join(root, "boot", "vmlinuz-directory"))

	tests := []struct {
		name     string
		grubPath string
		want     string
	}{
		{
			name:     "keeps explicit boot path",
			grubPath: "/boot/vmlinuz-6.1.0",
			want:     filepath.Join(root, "boot", "vmlinuz-6.1.0"),
		},
		{
			name:     "resolves root relative vmlinuz below mounted boot",
			grubPath: "/vmlinuz-6.1.0",
			want:     filepath.Join(root, "boot", "vmlinuz-6.1.0"),
		},
		{
			name:     "resolves root relative initrd below mounted boot",
			grubPath: "/initrd.img-6.1.0",
			want:     filepath.Join(root, "boot", "initrd.img-6.1.0"),
		},
		{
			name:     "resolves root relative initramfs below mounted boot",
			grubPath: "/initramfs-6.1.0.img",
			want:     filepath.Join(root, "boot", "initramfs-6.1.0.img"),
		},
		{
			name:     "prefers root path when it exists",
			grubPath: "/vmlinuz-local",
			want:     filepath.Join(root, "vmlinuz-local"),
		},
		{
			name:     "does not move non standard root relative path under boot",
			grubPath: "/explicit-root-kernel",
			want:     filepath.Join(root, "explicit-root-kernel"),
		},
		{
			name:     "skips root directory when boot file exists",
			grubPath: "/vmlinuz-directory",
			want:     filepath.Join(root, "boot", "vmlinuz-directory"),
		},
		{
			name:     "leaves empty path empty",
			grubPath: "  ",
			want:     "",
		},
		{
			name:     "does not move non boot artifact under boot",
			grubPath: "/EFI/BOOT/BOOTX64.EFI",
			want:     filepath.Join(root, "EFI", "BOOT", "BOOTX64.EFI"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveKexecPath(root, tt.grubPath); got != tt.want {
				t.Fatalf("resolveKexecPath(%q) = %q, want %q", tt.grubPath, got, tt.want)
			}
		})
	}
}

func TestResolveKexecPathRejectsSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "vmlinuz")
	mustWrite(t, outside)
	link := filepath.Join(root, "boot", "vmlinuz")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir boot dir: %v", err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink outside root: %v", err)
	}

	if got := resolveKexecPath(root, "/boot/vmlinuz"); got != "" {
		t.Fatalf("resolveKexecPath() = %q, want rejected outside-root symlink", got)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestHandleProvisionHandoffReportsFailedReboot(t *testing.T) {
	reporter := &provisionHandoffReporter{}
	ops, state := provisionHandoffTestOps(t, func() error {
		return errors.New("permission denied")
	}, nil)

	handleProvisionHandoff(
		context.Background(),
		&config.MachineConfig{},
		reporter,
		&runmode.ProvisionCompleteError{},
		false,
		ops,
	)

	assertProvisionHandoffFailure(t, reporter, state, "reboot handoff failed after provisioning success")
	assertProvisionHandoffFailure(t, reporter, state, "permission denied")
}

func TestHandleProvisionHandoffReportsFailedABPowerOff(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.PreserveExisting = true
	reporter := &provisionHandoffReporter{}
	ops, state := provisionHandoffTestOps(t, nil, func() error {
		return errors.New("power control denied")
	})

	handleProvisionHandoff(
		context.Background(),
		cfg,
		reporter,
		&runmode.ProvisionCompleteError{},
		false,
		ops,
	)

	assertProvisionHandoffFailure(t, reporter, state, "a/b preserveExisting requires kexec")
	assertProvisionHandoffFailure(t, reporter, state, "power control denied")
}

func TestHandleProvisionHandoffRequestsSuccessfulReboot(t *testing.T) {
	reporter := &provisionHandoffReporter{}
	rebootCalls := 0
	ops, state := provisionHandoffTestOps(t, func() error {
		rebootCalls++
		return nil
	}, nil)

	handleProvisionHandoff(
		context.Background(),
		&config.MachineConfig{},
		reporter,
		&runmode.ProvisionCompleteError{},
		false,
		ops,
	)

	if rebootCalls != 1 {
		t.Fatalf("reboot calls = %d, want 1", rebootCalls)
	}
	assertProvisionHandoffSuccess(t, reporter, state)
}

func TestHandleProvisionHandoffRequestsSuccessfulPowerOff(t *testing.T) {
	reporter := &provisionHandoffReporter{}
	powerOffCalls := 0
	ops, state := provisionHandoffTestOps(t, nil, func() error {
		powerOffCalls++
		return nil
	})

	handleProvisionHandoff(
		context.Background(),
		&config.MachineConfig{},
		reporter,
		&runmode.ProvisionCompleteError{PowerOff: true},
		false,
		ops,
	)

	if powerOffCalls != 1 {
		t.Fatalf("power off calls = %d, want 1", powerOffCalls)
	}
	assertProvisionHandoffSuccess(t, reporter, state)
}

func TestProvisionCompleteExitKeepsNetworkUntilHandoff(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	source := string(data)
	start := strings.Index(source, "case errors.As(modeErr, &provisionErr):")
	if start < 0 {
		t.Fatal("provision complete branch not found")
	}
	end := strings.Index(source[start:], "handleProvisionHandoff")
	if end < 0 {
		t.Fatal("provision complete handoff call not found")
	}
	branch := source[start : start+end]
	if strings.Contains(branch, ".Teardown(") {
		t.Fatalf("provision complete branch tears network down before handoff:\n%s", branch)
	}
}

func TestSetupNetworkModeExplicitGoBGPFailsClosed(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Network.Mode = "gobgp"
	cfg.Network.BGP.ASN = 65000
	cfg.Network.BGP.UnderlayAF = "invalid"
	cfg.Network.EVPN.UnderlayIP = "10.0.0.1"
	cfg.Network.EVPN.ProvisionVNI = 4000
	cfg.Network.EVPN.ProvisionGateway = "10.0.0.254"

	mode, err := setupNetworkMode(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected explicit GoBGP setup error")
	}
	if mode != nil {
		t.Fatalf("mode = %T, want nil on explicit GoBGP setup failure", mode)
	}
	if !strings.Contains(err.Error(), "gobgp network setup") ||
		!strings.Contains(err.Error(), "invalid underlay AF") {
		t.Fatalf("error = %q, want GoBGP setup failure context", err.Error())
	}
}

func TestStartPID1ChildReaperNoopWhenNotPID1(t *testing.T) {
	notified := false
	started := startPID1ChildReaperWith(
		context.Background(),
		42,
		0,
		func(chan<- os.Signal, ...os.Signal) {
			notified = true
		},
		func(chan<- os.Signal) {
			t.Fatal("stop called for pid != 1")
		},
		func() {
			t.Fatal("reap called for pid != 1")
		},
	)

	if started {
		t.Fatal("startPID1ChildReaperWith() = true, want false for pid != 1")
	}
	if notified {
		t.Fatal("signal notification registered for pid != 1")
	}
}

func TestStartPID1ChildReaperSignalTriggersReapAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	notified := make(chan chan<- os.Signal, 1)
	reaped := make(chan struct{}, 1)
	stopped := make(chan struct{}, 1)

	started := startPID1ChildReaperWith(
		ctx,
		1,
		0,
		func(ch chan<- os.Signal, _ ...os.Signal) {
			notified <- ch
		},
		func(chan<- os.Signal) {
			close(stopped)
		},
		func() {
			reaped <- struct{}{}
		},
	)
	if !started {
		t.Fatal("startPID1ChildReaperWith() = false, want true for pid 1")
	}

	signals := receiveSignalSink(t, notified, "signal notification")
	signals <- syscall.SIGCHLD
	receiveEvent(t, reaped, "reap after SIGCHLD")

	cancel()
	receiveEvent(t, stopped, "signal stop after cancellation")
}

func TestStartPID1ChildReaperCancelsDelayedReap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	notified := make(chan chan<- os.Signal, 1)
	reaped := make(chan struct{}, 1)
	stopped := make(chan struct{}, 1)

	startPID1ChildReaperWith(
		ctx,
		1,
		time.Hour,
		func(ch chan<- os.Signal, _ ...os.Signal) {
			notified <- ch
		},
		func(chan<- os.Signal) {
			close(stopped)
		},
		func() {
			reaped <- struct{}{}
		},
	)

	signals := receiveSignalSink(t, notified, "signal notification")
	signals <- syscall.SIGCHLD
	cancel()

	receiveEvent(t, stopped, "signal stop after delayed reap cancellation")
	assertNoEvent(t, reaped, "reap after cancellation")
}

func TestReapExitedChildrenWithReapsOnlyUnmanagedChildren(t *testing.T) {
	var waited []int
	reapExitedChildrenWith(childReaper{
		childPIDs: func() ([]int, error) {
			return []int{11, 22, 33}, nil
		},
		managed: func(pid int) bool {
			return pid == 22
		},
		wait: func(pid int) (int, syscall.WaitStatus, error) {
			waited = append(waited, pid)
			return pid, 0, nil
		},
	})

	want := []int{11, 33}
	if !slices.Equal(waited, want) {
		t.Fatalf("waited pids = %v, want unmanaged pids %v", waited, want)
	}
}

func TestReapExitedChildrenWithStopsWhenNoProcessReady(t *testing.T) {
	calls := 0

	reapExitedChildWith(42, func(pid int) (int, syscall.WaitStatus, error) {
		calls++
		if pid != 42 {
			t.Fatalf("pid = %d, want 42", pid)
		}
		return 0, 0, nil
	})

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestReapExitedChildrenWithStopsOnUnexpectedError(t *testing.T) {
	calls := 0
	wantErr := errors.New("wait failure")

	reapExitedChildWith(42, func(pid int) (int, syscall.WaitStatus, error) {
		calls++
		if pid != 42 {
			t.Fatalf("pid = %d, want 42", pid)
		}
		return -1, 0, wantErr
	})

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestReapExitedChildrenWithRetriesInterruptedWait(t *testing.T) {
	calls := 0

	reapExitedChildWith(42, func(pid int) (int, syscall.WaitStatus, error) {
		calls++
		if pid != 42 {
			t.Fatalf("pid = %d, want 42", pid)
		}
		switch calls {
		case 1:
			return -1, 0, syscall.EINTR
		case 2:
			return 42, 0, nil
		default:
			return -1, 0, syscall.ECHILD
		}
	})

	if calls != 2 {
		t.Fatalf("calls = %d, want EINTR retry then child reap", calls)
	}
}

func TestProcStatState(t *testing.T) {
	tests := []struct {
		name    string
		stat    string
		want    byte
		wantErr bool
	}{
		{
			name: "simple process",
			stat: "123 (sleep) S 1 2 3 4 5",
			want: 'S',
		},
		{
			name: "zombie process",
			stat: "456 (daemon) Z 1 2 3 4 5",
			want: 'Z',
		},
		{
			name: "command contains closing paren",
			stat: "789 (worker) helper) R 1 2 3 4 5",
			want: 'R',
		},
		{
			name: "command contains spaces and parentheses",
			stat: "101 (worker (with spaces)) D 1 2 3 4 5",
			want: 'D',
		},
		{
			name:    "missing command terminator",
			stat:    "123 sleep S 1 2 3",
			wantErr: true,
		},
		{
			name:    "empty stat",
			stat:    "",
			wantErr: true,
		},
		{
			name:    "missing state",
			stat:    "123 (sleep)",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := procStatState(tt.stat)
			if tt.wantErr {
				if err == nil {
					t.Fatal("procStatState() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("procStatState(): %v", err)
			}
			if got != tt.want {
				t.Fatalf("procStatState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWaitBeforeReapingRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	waitBeforeReaping(ctx, time.Hour, func() {
		called = true
	})

	if called {
		t.Fatal("reap called after context cancellation")
	}
}

func receiveSignalSink(t *testing.T, ch <-chan chan<- os.Signal, desc string) chan<- os.Signal {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case got := <-ch:
		return got
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", desc)
	}
	return nil
}

func receiveEvent(t *testing.T, ch <-chan struct{}, desc string) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", desc)
	}
}

func assertNoEvent(t *testing.T, ch <-chan struct{}, desc string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected %s", desc)
	default:
	}
}

func TestSetupMountsAndDevicesFailsOnMissingRequiredMount(t *testing.T) {
	mounts := newFakeEarlyMounts()
	delete(mounts.mounts, "proc")

	err := setupMountsAndDevicesWith(mounts, &fakeEarlyDevices{})
	if err == nil {
		t.Fatal("expected missing mount error")
	}
	if !strings.Contains(err.Error(), `required early mount "proc" not configured`) {
		t.Fatalf("error = %q, want missing proc mount context", err.Error())
	}
	if len(mounts.calls) != 0 {
		t.Fatalf("calls = %v, want no setup after missing mount", mounts.calls)
	}
}

func TestSetupMountsAndDevicesStopsOnMountAllError(t *testing.T) {
	mounts := newFakeEarlyMounts()
	mounts.mountAllErr = errors.New("proc unavailable")
	devices := &fakeEarlyDevices{}

	err := setupMountsAndDevicesWith(mounts, devices)
	if err == nil {
		t.Fatal("expected mount all error")
	}
	if !strings.Contains(err.Error(), "mount early filesystems") ||
		!strings.Contains(err.Error(), "proc unavailable") {
		t.Fatalf("error = %q, want mount all context", err.Error())
	}
	wantCalls := []string{"create-folders", "mount-named:dev", "mount-all"}
	if !slices.Equal(mounts.calls, wantCalls) {
		t.Fatalf("mount calls = %v, want %v", mounts.calls, wantCalls)
	}
	if devices.calls != 1 {
		t.Fatalf("device calls = %d, want 1", devices.calls)
	}
}

func TestSetupMountsAndDevicesStopsOnCreateFolderError(t *testing.T) {
	mounts := newFakeEarlyMounts()
	mounts.createErr = errors.New("mkdir denied")
	devices := &fakeEarlyDevices{}

	err := setupMountsAndDevicesWith(mounts, devices)
	if err == nil {
		t.Fatal("expected create folder error")
	}
	if !strings.Contains(err.Error(), "create early mount folders") ||
		!strings.Contains(err.Error(), "mkdir denied") {
		t.Fatalf("error = %q, want create folder context", err.Error())
	}
	wantCalls := []string{"create-folders"}
	if !slices.Equal(mounts.calls, wantCalls) {
		t.Fatalf("mount calls = %v, want %v", mounts.calls, wantCalls)
	}
	if devices.calls != 0 {
		t.Fatalf("device calls = %d, want 0", devices.calls)
	}
}

func TestSetupMountsAndDevicesStopsOnMountDevError(t *testing.T) {
	mounts := newFakeEarlyMounts()
	mounts.mountDevErr = errors.New("devtmpfs unavailable")
	devices := &fakeEarlyDevices{}

	err := setupMountsAndDevicesWith(mounts, devices)
	if err == nil {
		t.Fatal("expected mount dev error")
	}
	if !strings.Contains(err.Error(), "mount early /dev") ||
		!strings.Contains(err.Error(), "devtmpfs unavailable") {
		t.Fatalf("error = %q, want mount dev context", err.Error())
	}
	wantCalls := []string{"create-folders", "mount-named:dev"}
	if !slices.Equal(mounts.calls, wantCalls) {
		t.Fatalf("mount calls = %v, want %v", mounts.calls, wantCalls)
	}
	if devices.calls != 0 {
		t.Fatalf("device calls = %d, want 0", devices.calls)
	}
}

func TestSetupMountsAndDevicesStopsOnCreateDeviceError(t *testing.T) {
	mounts := newFakeEarlyMounts()
	devices := &fakeEarlyDevices{err: errors.New("mknod denied")}

	err := setupMountsAndDevicesWith(mounts, devices)
	if err == nil {
		t.Fatal("expected create device error")
	}
	if !strings.Contains(err.Error(), "create early devices") ||
		!strings.Contains(err.Error(), "mknod denied") {
		t.Fatalf("error = %q, want create device context", err.Error())
	}
	wantCalls := []string{"create-folders", "mount-named:dev"}
	if !slices.Equal(mounts.calls, wantCalls) {
		t.Fatalf("mount calls = %v, want %v", mounts.calls, wantCalls)
	}
	if devices.calls != 1 {
		t.Fatalf("device calls = %d, want 1", devices.calls)
	}
}

func TestSetupMountsAndDevicesSuccess(t *testing.T) {
	mounts := newFakeEarlyMounts()
	devices := &fakeEarlyDevices{}

	if err := setupMountsAndDevicesWith(mounts, devices); err != nil {
		t.Fatalf("setupMountsAndDevicesWith: %v", err)
	}
	wantCalls := []string{"create-folders", "mount-named:dev", "mount-all"}
	if !slices.Equal(mounts.calls, wantCalls) {
		t.Fatalf("mount calls = %v, want %v", mounts.calls, wantCalls)
	}
	if devices.calls != 1 {
		t.Fatalf("device calls = %d, want 1", devices.calls)
	}
	for _, name := range []string{"dev", "proc", "run", "tmp", "sys"} {
		mt := mounts.mounts[name]
		if mt == nil || !mt.CreateMount || !mt.EnableMount {
			t.Fatalf("mount %s = %+v, want create+enable", name, mt)
		}
	}
}

type fakeEarlyMounts struct {
	mounts      map[string]*realm.Mount
	calls       []string
	createErr   error
	mountDevErr error
	mountAllErr error
}

func newFakeEarlyMounts() *fakeEarlyMounts {
	mounts := make(map[string]*realm.Mount)
	for _, name := range []string{"dev", "proc", "run", "tmp", "sys"} {
		mounts[name] = &realm.Mount{Name: name}
	}
	return &fakeEarlyMounts{mounts: mounts}
}

func (f *fakeEarlyMounts) GetMount(name string) *realm.Mount {
	return f.mounts[name]
}

func (f *fakeEarlyMounts) CreateFolder() error {
	f.calls = append(f.calls, "create-folders")
	return f.createErr
}

func (f *fakeEarlyMounts) MountNamed(name string, _ bool) error {
	f.calls = append(f.calls, "mount-named:"+name)
	return f.mountDevErr
}

func (f *fakeEarlyMounts) MountAll() error {
	f.calls = append(f.calls, "mount-all")
	return f.mountAllErr
}

type fakeEarlyDevices struct {
	calls int
	err   error
}

func (f *fakeEarlyDevices) CreateDevice() error {
	f.calls++
	return f.err
}

type provisionHandoffReporter struct {
	status  config.Status
	message string
}

type provisionHandoffState struct {
	shellCalls int
	exitCode   int
}

func (r *provisionHandoffReporter) ReportStatus(_ context.Context, status config.Status, message string) error {
	r.status = status
	r.message = message
	return nil
}

func provisionHandoffTestOps(t *testing.T, reboot, powerOff func() error) (provisionHandoffOps, *provisionHandoffState) {
	t.Helper()
	state := &provisionHandoffState{exitCode: -1}
	ops := provisionHandoffOps{}
	ops.reboot = func() error {
		if reboot == nil {
			t.Fatal("unexpected reboot request")
		}
		return reboot()
	}
	ops.powerOff = func() error {
		if powerOff == nil {
			t.Fatal("unexpected power off request")
		}
		return powerOff()
	}
	ops.shell = func() {
		state.shellCalls++
	}
	ops.exit = func(code int) {
		state.exitCode = code
	}
	return ops, state
}

func assertProvisionHandoffFailure(
	t *testing.T,
	reporter *provisionHandoffReporter,
	state *provisionHandoffState,
	want string,
) {
	t.Helper()
	if reporter.status != config.StatusError {
		t.Fatalf("status = %q, want %q", reporter.status, config.StatusError)
	}
	if !strings.Contains(reporter.message, want) {
		t.Fatalf("message = %q, want to contain %q", reporter.message, want)
	}
	if state.shellCalls != 1 {
		t.Fatalf("shell calls = %d, want 1", state.shellCalls)
	}
	if state.exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", state.exitCode)
	}
}

func assertProvisionHandoffSuccess(
	t *testing.T,
	reporter *provisionHandoffReporter,
	state *provisionHandoffState,
) {
	t.Helper()
	if reporter.status == config.StatusError {
		t.Fatalf("status = %q, want no error status", reporter.status)
	}
	if reporter.message != "" {
		t.Fatalf("message = %q, want empty", reporter.message)
	}
	if state.shellCalls != 0 {
		t.Fatalf("shell calls = %d, want 0", state.shellCalls)
	}
	if state.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", state.exitCode)
	}
}

func TestFlushObservabilityBeforeReboot(t *testing.T) {
	var logShipped atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logShipped.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.MachineConfig{Transport: config.TransportConfig{LogURL: srv.URL}}
	client := caprf.NewFromConfig(cfg)

	baseHandler := slog.NewTextHandler(os.Stderr, nil)
	originalSlogHandler = baseHandler
	caprfRemoteLogHandler = caprf.NewRemoteHandler(client, baseHandler, slog.LevelInfo, 100)
	slog.SetDefault(slog.New(caprfRemoteLogHandler))
	t.Cleanup(flushObservability)

	slog.Info("test log")
	flushObservability()

	// Handler should be restored.
	slog.Info("post-flush log") // Should not panic

	if !logShipped.Load() {
		t.Error("log was not shipped before flush returned")
	}
}

type recordingReporter struct{ statuses []config.Status }

func (r *recordingReporter) ReportStatus(_ context.Context, status config.Status, _ string) error {
	r.statuses = append(r.statuses, status)
	return nil
}

func TestFailRunFailureActionWaitReportsAndWaits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reporter := &recordingReporter{}
	rebooted := make(chan struct{})
	go func() {
		failRun(ctx, &config.MachineConfig{FailureAction: config.FailureActionWait}, reporter, "setup failed", func() { close(rebooted) })
	}()
	select {
	case <-rebooted:
		t.Fatal("FAILURE_ACTION=wait rebooted before the context ended")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-rebooted:
	case <-time.After(5 * time.Second):
		t.Fatal("failRun did not return after the context ended")
	}
	if len(reporter.statuses) != 1 || reporter.statuses[0] != config.StatusError {
		t.Fatalf("statuses = %v, want one error report like the legacy fail()", reporter.statuses)
	}

	called := false
	failRun(context.Background(), &config.MachineConfig{}, nil, "x", func() { called = true })
	if !called {
		t.Fatal("default mode must reboot right away")
	}
}

func TestTokenRenewalFatalStopsTheTask(t *testing.T) {
	prevGrace := tokenShutdownGrace
	tokenShutdownGrace = 10 * time.Millisecond
	t.Cleanup(func() { tokenShutdownGrace = prevGrace; tokenFatalReason.Store(nil) })

	// FAILURE_ACTION=wait: stop the task, no reboot.
	var rebooted atomic.Bool
	stopped := false
	tokenRenewalFatal(&config.MachineConfig{FailureAction: config.FailureActionWait}, errors.New("renewal failed after 5 attempts"),
		func() { rebooted.Store(true) }, func() { stopped = true })
	time.Sleep(50 * time.Millisecond)
	if rebooted.Load() || !stopped {
		t.Fatalf("FAILURE_ACTION=wait: rebooted=%v stopped=%v, want the task stopped without reboot", rebooted.Load(), stopped)
	}
	if got := tokenStopMessage(); got != "token renewal failed, task stopped: renewal failed after 5 attempts" {
		t.Fatalf("stop message = %q", got)
	}

	// Default: stop the task; reboot when it does not stop in time.
	stopped = false
	rebootedCh := make(chan struct{})
	tokenRenewalFatal(&config.MachineConfig{}, errors.New("expired"), func() { close(rebootedCh) }, func() { stopped = true })
	if !stopped {
		t.Fatal("default: the task was not stopped")
	}
	select {
	case <-rebootedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("default: no reboot after the grace period")
	}
}

// A task that completes at the same moment cancels the grace reboot.
func TestTokenGraceRebootStopsOnTerminalSuccess(t *testing.T) {
	prevGrace := tokenShutdownGrace
	tokenShutdownGrace = 50 * time.Millisecond
	t.Cleanup(func() { tokenShutdownGrace = prevGrace; tokenFatalReason.Store(nil) })
	var rebooted atomic.Bool
	tokenRenewalFatal(&config.MachineConfig{}, errors.New("expired"), func() { rebooted.Store(true) }, func() {})
	stopTokenGraceReboot()
	time.Sleep(150 * time.Millisecond)
	if rebooted.Load() {
		t.Fatal("the grace reboot fired after the task completed")
	}
}

// A token failure after the task completed arms no grace reboot.
func TestTokenRenewalFatalAfterTaskDone(t *testing.T) {
	prevGrace := tokenShutdownGrace
	tokenShutdownGrace = 20 * time.Millisecond
	tokenTaskDone.Store(true)
	t.Cleanup(func() { tokenShutdownGrace = prevGrace; tokenTaskDone.Store(false); tokenFatalReason.Store(nil) })
	var rebooted, stopped atomic.Bool
	tokenRenewalFatal(&config.MachineConfig{}, errors.New("expired"), func() { rebooted.Store(true) }, func() { stopped.Store(true) })
	time.Sleep(100 * time.Millisecond)
	if rebooted.Load() || stopped.Load() {
		t.Fatalf("rebooted=%v stopped=%v after the task completed, want neither", rebooted.Load(), stopped.Load())
	}
}

// messageReporter records the reported messages.
type messageReporter struct{ messages []string }

func (r *messageReporter) ReportStatus(_ context.Context, _ config.Status, message string) error {
	r.messages = append(r.messages, message)
	return nil
}

// tokenShutdown reports the reason without a token, then reboots.
func TestTokenShutdownReportsTheReason(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev); tokenFatalReason.Store(nil) })
	prevGrace := tokenShutdownGrace
	tokenShutdownGrace = time.Hour
	t.Cleanup(func() { tokenShutdownGrace = prevGrace; stopTokenGraceReboot() })

	// The reason as the auth package builds it: URL and status, no token.
	tokenRenewalFatal(&config.MachineConfig{}, errors.New("renewal failed after 5 attempts: renew token from https://caprf.example.com/agent/token: status 503"),
		func() {}, func() {})
	reporter := &messageReporter{}
	rebooted := false
	tokenShutdown(context.Background(), &config.MachineConfig{}, reporter, tokenStopMessage(), func() { rebooted = true })
	if !rebooted || len(reporter.messages) != 1 || !strings.Contains(reporter.messages[0], "status 503") {
		t.Fatalf("rebooted=%v messages=%v, want one error report and a reboot", rebooted, reporter.messages)
	}
	for _, text := range []string{reporter.messages[0], logs.String()} {
		if strings.Contains(text, "Bearer") || strings.Contains(text, "secret") {
			t.Fatalf("token material in %q", text)
		}
	}
}

func TestIsTerminalSuccess(t *testing.T) {
	if !isTerminalSuccess(&runmode.ProvisionCompleteError{}) || !isTerminalSuccess(&runmode.PowerOffRequestedError{}) {
		t.Fatal("completed task results must count as success")
	}
	if isTerminalSuccess(errors.New("canceled")) || isTerminalSuccess(nil) {
		t.Fatal("errors and nil are not terminal success")
	}
}

// Legacy evidence: copy_files puts the provisioner resolv.conf in place and
// the legacy scripts never rewrite it, also when netplan names DNS servers.
func TestKeepProvisionerResolvConfWithRamdiskFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "etc", "resolv.conf"), []byte("nameserver 10.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := provision.SetProvisionerFilesDirForTest(dir)
	defer restore()

	netCfg := &network.Config{DNSResolvers: "10.9.9.9"}
	keepProvisionerResolvConf(&config.MachineConfig{}, netCfg)
	if netCfg.DNSResolvers != "10.9.9.9" {
		t.Fatal("default mode must keep the configured resolvers")
	}
	keepProvisionerResolvConf(&config.MachineConfig{ProvisionerFilesTarget: config.ProvisionerFilesRamdisk}, netCfg)
	if netCfg.DNSResolvers != "" {
		t.Fatalf("PROVISIONER_FILES_TARGET=ramdisk would rewrite the provisioner resolv.conf with %q", netCfg.DNSResolvers)
	}
}

func TestLoadModulesWarnsWhenAModuleNeverLoads(t *testing.T) {
	origDir, origLoad, origLogger := moduleDir, loadModuleFn, slog.Default()
	t.Cleanup(func() {
		moduleDir, loadModuleFn = origDir, origLoad
		slog.SetDefault(origLogger)
	})
	moduleDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(moduleDir, "broken.ko"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	loadModuleFn = func(string) error { return errors.New("invalid module format") }
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))

	loadModules()

	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "broken.ko") {
		t.Fatalf("expected a warning for broken.ko at the default level, got:\n%s", logs.String())
	}
}

// A bm4x-style netplan and frr.conf name the NICs only by their predictable
// names; all of them must reach the predictable-name rename, from the
// netplan ethernet IDs and from the frr.conf neighbors alone too.
func TestPredictableNamesFromNetplanAndFRR(t *testing.T) {
	literal := `network:
  version: 2
  ethernets:
    enp1s0:
      mtu: 9000
      link-local: [ipv6]
    enp2s0:
      mtu: 9000
      link-local: [ipv6]
`
	matched := `network:
  version: 2
  ethernets:
    fabric0:
      match: {macaddress: "02:00:00:00:00:01"}
      mtu: 9000
    fabric1:
      match: {macaddress: "02:00:00:00:00:02"}
      mtu: 9000
`
	frrConf := `router bgp 4200000101
 bgp router-id 10.0.0.11
 neighbor fabric peer-group
 neighbor fabric remote-as external
 neighbor enp1s0 interface peer-group fabric
 neighbor enp2s0 interface peer-group fabric
`
	for _, tt := range []struct {
		name, netplan, frr string
	}{
		{name: "netplan and frr.conf", netplan: literal, frr: frrConf},
		{name: "frr.conf only", netplan: matched, frr: frrConf},
		{name: "netplan only", netplan: literal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			netplanDir := filepath.Join(dir, "netplan")
			if err := os.MkdirAll(netplanDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(netplanDir, "50-bm4x.yaml"), []byte(tt.netplan), 0o600); err != nil {
				t.Fatal(err)
			}
			frrPath := filepath.Join(dir, "frr.conf")
			if tt.frr != "" {
				if err := os.WriteFile(frrPath, []byte(tt.frr), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			npCfg, err := detectNetplanConfig(netplanDir, frrPath, true, true)
			if err != nil || npCfg == nil {
				t.Fatalf("detectNetplanConfig() = %v, %v", npCfg, err)
			}
			netCfg := &network.Config{}
			mergeNetplanConfig(netCfg, npCfg)
			got := network.ConfiguredInterfaceNames(netCfg)
			for _, want := range []string{"enp1s0", "enp2s0"} {
				if !slices.Contains(got, want) {
					t.Errorf("configured names %v miss %s", got, want)
				}
			}
		})
	}
}

func TestLoadModulesSkipsEFIVarFSWithoutEFIFirmware(t *testing.T) {
	origDir, origLoad, origEFI := moduleDir, loadModuleFn, efiFirmwareDir
	t.Cleanup(func() { moduleDir, loadModuleFn, efiFirmwareDir = origDir, origLoad, origEFI })
	moduleDir = t.TempDir()
	for _, m := range []string{"efivarfs.ko", "nvme.ko"} {
		if err := os.WriteFile(filepath.Join(moduleDir, m), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, uefi := range []bool{false, true} {
		efiFirmwareDir = filepath.Join(t.TempDir(), "efi")
		if uefi {
			if err := os.MkdirAll(efiFirmwareDir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		var loaded []string
		loadModuleFn = func(path string) error {
			loaded = append(loaded, filepath.Base(path))
			return nil
		}
		loadModules()
		if got := slices.Contains(loaded, "efivarfs.ko"); got != uefi {
			t.Errorf("UEFI=%v: efivarfs.ko loaded = %v (loaded %v)", uefi, got, loaded)
		}
		if !slices.Contains(loaded, "nvme.ko") {
			t.Errorf("UEFI=%v: nvme.ko not loaded", uefi)
		}
	}
}

func TestDetectNetplanConfigRejectsMalformedFiles(t *testing.T) {
	writeFile := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	validNetplan := "network:\n  version: 2\n  ethernets:\n    eth0:\n      dhcp4: true\n"

	t.Run("no netplan files", func(t *testing.T) {
		cfg, err := detectNetplanConfig(t.TempDir(), "/nonexistent/frr.conf", true, true)
		if cfg != nil || err != nil {
			t.Fatalf("detectNetplanConfig() = %v, %v, want nil, nil", cfg, err)
		}
	})
	t.Run("unreadable netplan dir", func(t *testing.T) {
		notDir := filepath.Join(t.TempDir(), "netplan")
		writeFile(t, notDir, "x")
		if _, err := detectNetplanConfig(notDir, "/nonexistent/frr.conf", true, true); err == nil {
			t.Fatal("detectNetplanConfig() = nil error, want error for a netplan path that is not a dir")
		}
	})
	t.Run("valid netplan without frr.conf", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "50-booty.yaml"), validNetplan)
		cfg, err := detectNetplanConfig(dir, filepath.Join(dir, "missing-frr.conf"), true, true)
		if err != nil || cfg == nil {
			t.Fatalf("detectNetplanConfig() = %v, %v, want a config", cfg, err)
		}
	})
	t.Run("malformed netplan", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "50-booty.yaml"), "network: [unclosed\n")
		if _, err := detectNetplanConfig(dir, filepath.Join(dir, "frr.conf"), true, true); err == nil {
			t.Fatal("detectNetplanConfig() = nil error, want parse error")
		}
	})
	t.Run("malformed frr.conf", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "50-booty.yaml"), validNetplan)
		frr := filepath.Join(dir, "frr.conf")
		writeFile(t, frr, "router bgp 99999999999\n")
		if _, err := detectNetplanConfig(dir, frr, true, true); err == nil {
			t.Fatal("detectNetplanConfig() = nil error, want FRR parse error")
		}
	})
	// NETPLAN_STRICT=false and FRR_CONFIG_STRICT=false skip what BOOTy does
	// not implement and keep the rest.
	t.Run("unknown key and statement, strict and lenient", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "50-booty.yaml"),
			"network:\n  version: 2\n  ethernets:\n    eth0:\n      dhcp4: true\n      wakeonlan: true\n")
		frr := filepath.Join(dir, "frr.conf")
		writeFile(t, frr, "router bgp 65000\n maximum-paths 8\n neighbor eth0 interface remote-as external\n")
		_, err := detectNetplanConfig(dir, frr, true, false)
		if err == nil || !strings.Contains(err.Error(), "wakeonlan") {
			t.Fatalf("strict netplan: %v, want an error naming wakeonlan", err)
		}
		_, err = detectNetplanConfig(dir, frr, false, true)
		if err == nil || !strings.Contains(err.Error(), `line 2 "maximum-paths 8"`) {
			t.Fatalf("strict frr.conf: %v, want an error naming line 2", err)
		}
		// One prefix: main names the file, the parse error the line.
		if want := "parse FRR config " + frr + `: line 2 "maximum-paths 8": statement not supported by BOOTy (in router bgp 65000)`; err.Error() != want {
			t.Fatalf("strict frr.conf error\n got  %s\n want %s", err, want)
		}
		cfg, err := detectNetplanConfig(dir, frr, false, false)
		if err != nil || cfg == nil || cfg.ASN != 65000 {
			t.Fatalf("lenient: %+v, %v, want the config with ASN 65000", cfg, err)
		}
	})
}

// The FRR config sets the numbered peers and their remote ASN together.
// An iBGP peer (remote ASN 0) must not keep an eBGP ASN from the vars.
func TestMergeNetplanConfigRemoteASNFollowsNeighbors(t *testing.T) {
	dst := &network.Config{BGPNeighbors: "10.0.0.1", BGPRemoteASN: 65500}
	mergeNetplanConfig(dst, &network.Config{BGPNeighbors: "192.0.2.1"})
	if dst.BGPNeighbors != "192.0.2.1" || dst.BGPRemoteASN != 0 {
		t.Fatalf("merge = %q/%d, want 192.0.2.1 with iBGP (0)", dst.BGPNeighbors, dst.BGPRemoteASN)
	}
	dst = &network.Config{BGPNeighbors: "10.0.0.1", BGPRemoteASN: 65500}
	mergeNetplanConfig(dst, &network.Config{})
	if dst.BGPRemoteASN != 65500 {
		t.Fatalf("merge without netplan peers = %d, want the vars value 65500", dst.BGPRemoteASN)
	}
}

// The static address of a netplan match without set-name goes to the NIC
// that the match selects; without a single match BOOTy keeps the first NIC.
func TestPrepareLinkLayersResolvesStaticMatch(t *testing.T) {
	orig := resolveLinkMatch
	t.Cleanup(func() { resolveLinkMatch = orig })
	for _, tt := range []struct {
		name string
		nic  string
		err  error
		want string
	}{
		{"unique match", "enp1s4", nil, "enp1s4"},
		{"several matches", "", errors.New("selects 2 NICs"), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resolveLinkMatch = func(context.Context, network.LinkMatch) (string, error) { return tt.nic, tt.err }
			netCfg := &network.Config{StaticIP: "10.0.0.5/24", StaticMatch: &network.LinkMatch{Name: "enp*"}}
			if _, err := prepareLinkLayers(context.Background(), netCfg); err != nil {
				t.Fatalf("prepareLinkLayers: %v", err)
			}
			if netCfg.StaticIface != tt.want {
				t.Fatalf("StaticIface = %q, want %q", netCfg.StaticIface, tt.want)
			}
		})
	}
}

// Unset strictness options mean strict; each key sets only its own input.
func TestInputStrictness(t *testing.T) {
	off := false
	cfg := &config.MachineConfig{}
	if np, frr := inputStrictness(cfg); !np || !frr {
		t.Fatalf("unset: netplan %v frr %v, want both strict", np, frr)
	}
	cfg.Network.FRRStrict = &off
	if np, frr := inputStrictness(cfg); !np || frr {
		t.Fatalf("FRR_CONFIG_STRICT=false: netplan %v frr %v", np, frr)
	}
	cfg.Network.FRRStrict, cfg.Network.NetplanStrict = nil, &off
	if np, frr := inputStrictness(cfg); np || !frr {
		t.Fatalf("NETPLAN_STRICT=false: netplan %v frr %v", np, frr)
	}
}

// network.bgp.sessions replaces the frr.conf sessions as a whole, with the
// merged ASN, and the scalar peer vars are reported as ignored.
func TestApplyNativeSessions(t *testing.T) {
	netCfg := &network.Config{ASN: 65000, BGPSessions: []network.BGPSession{{Name: "192.0.2.9"}, {Name: "192.0.2.10"}}}
	bgp := &config.BGPConfig{Sessions: []config.BGPSessionConfig{{Name: "192.0.2.1", RemoteAS: "internal", Families: []string{"l2vpn-evpn"}}}}
	if err := applyNativeSessions(bgp, netCfg); err != nil {
		t.Fatal(err)
	}
	if len(netCfg.BGPSessions) != 1 || netCfg.BGPSessions[0].Name != "192.0.2.1" || netCfg.BGPSessions[0].RemoteASN != 65000 || !netCfg.BGPSessions[0].Internal {
		t.Fatalf("sessions = %+v, want only the native iBGP session with AS 65000", netCfg.BGPSessions)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	vars := &network.Config{BGPNeighbors: "192.0.2.1", BGPKeepalive: 3}
	keys := setPeerVars(vars)
	warnIgnoredPeerVars(netCfg, keys)
	for _, k := range []string{"key=BGP_NEIGHBORS", "key=BGP_KEEPALIVE"} {
		if !strings.Contains(buf.String(), k) {
			t.Errorf("no warning for %s in %q", k, buf.String())
		}
	}
	if strings.Contains(buf.String(), "BGP_HOLD") {
		t.Errorf("warning for an unset key: %q", buf.String())
	}
}

// "bgp bestpath as-path multipath-relax" and network.bgp.multipathRelax
// give the same flag; a native value overrides the parsed one. The
// unsupported variant fails in both forms.
func TestApplyNativeMultipathRelax(t *testing.T) {
	parsed, err := netplan.ParseFRRConfigBytes([]byte("router bgp 65000\n bgp bestpath as-path multipath-relax\n"))
	if err != nil {
		t.Fatal(err)
	}
	vars, err := caprf.ParseVars(strings.NewReader("BGP_MULTIPATH_RELAX=true\n"))
	if err != nil {
		t.Fatal(err)
	}
	native := &network.Config{}
	applyBGPOptions(&vars.Network.BGP, native)
	if !parsed.MultipathRelax || native.BGPMultipathRelax != parsed.MultipathRelax {
		t.Fatalf("frr.conf %v, native %v, want both true", parsed.MultipathRelax, native.BGPMultipathRelax)
	}

	merged := &network.Config{BGPMultipathRelax: true}
	applyBGPOptions(&config.BGPConfig{MultipathRelax: new(false)}, merged)
	if merged.BGPMultipathRelax {
		t.Error("native multipathRelax false did not override the frr.conf value")
	}

	if _, err := netplan.ParseFRRConfigBytes([]byte("router bgp 65000\n bgp bestpath as-path multipath-relax as-set\n")); err == nil {
		t.Error("frr.conf: multipath-relax as-set parsed without error")
	}
	if _, err := caprf.ParseVars(strings.NewReader("BGP_MULTIPATH_RELAX=as-set\n")); err == nil {
		t.Error("vars: BGP_MULTIPATH_RELAX=as-set parsed without error")
	}
}

// The relax flag goes from frr.conf through the netplan merge (the
// sessions branch) into the GoBGP config.
func TestMultipathRelaxPlumbing(t *testing.T) {
	parsed, err := netplan.ParseFRRConfigBytes([]byte("router bgp 65000\n bgp bestpath as-path multipath-relax\n neighbor 192.0.2.1 remote-as 65001\n"))
	if err != nil {
		t.Fatal(err)
	}
	src := netplan.ToNetworkConfig(&netplan.Config{}, parsed)
	dst := &network.Config{}
	mergeNetplanConfig(dst, src)
	if len(dst.BGPSessions) != 1 || !dst.BGPMultipathRelax {
		t.Fatalf("merged sessions %d, relax %v; want the frr.conf session and relax", len(dst.BGPSessions), dst.BGPMultipathRelax)
	}
}

// "[no] bgp fast-external-failover" and network.bgp.fastExternalFailover
// give the same session flag; the native value overrides frr.conf. An
// unsupported value fails in both forms.
func TestFastExternalFailoverBothForms(t *testing.T) {
	parsed, err := netplan.ParseFRRConfigBytes([]byte("router bgp 65000\n no bgp fast-external-failover\n neighbor eth0 interface remote-as external\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Sessions) != 1 || parsed.Sessions[0].FastExternalFailover {
		t.Fatalf("frr.conf sessions = %+v, want fast-external-failover off", parsed.Sessions)
	}
	vars, err := caprf.ParseVars(strings.NewReader("BGP_FAST_EXTERNAL_FAILOVER=false\nBGP_SESSIONS=[{\"name\":\"eth0\",\"interface\":true,\"remoteAS\":\"external\",\"families\":[\"ipv4-unicast\"]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	native := &network.Config{ASN: 65000}
	if err := applyNativeSessions(&vars.Network.BGP, native); err != nil {
		t.Fatal(err)
	}
	if len(native.BGPSessions) != 1 || native.BGPSessions[0].FastExternalFailover != parsed.Sessions[0].FastExternalFailover {
		t.Fatalf("native sessions = %+v, want the frr.conf flag", native.BGPSessions)
	}
	on := &network.Config{BGPSessions: []network.BGPSession{{Name: "eth0", FastExternalFailover: false}}}
	if err := applyNativeSessions(&config.BGPConfig{FastExternalFailover: new(true)}, on); err != nil {
		t.Fatal(err)
	}
	if !on.BGPSessions[0].FastExternalFailover {
		t.Error("native fastExternalFailover true did not override frr.conf")
	}
	// Native sessions without their own setting keep a frr.conf "no".
	replaced := &network.Config{ASN: 65000, BGPSessions: parsed.Sessions}
	if err := applyNativeSessions(&config.BGPConfig{Sessions: []config.BGPSessionConfig{{Name: "eth0", Interface: true, RemoteAS: "external", Families: []string{"ipv4-unicast"}}}}, replaced); err != nil {
		t.Fatal(err)
	}
	if replaced.BGPSessions[0].FastExternalFailover {
		t.Error("native sessions lost the frr.conf no bgp fast-external-failover")
	}
	if _, err := netplan.ParseFRRConfigBytes([]byte("router bgp 65000\n bgp fast-external-failover now\n")); err == nil {
		t.Error("frr.conf: bgp fast-external-failover now parsed")
	}
	if _, err := caprf.ParseVars(strings.NewReader("BGP_FAST_EXTERNAL_FAILOVER=sometimes\n")); err == nil {
		t.Error("vars: BGP_FAST_EXTERNAL_FAILOVER=sometimes parsed")
	}
}

// The inference path of TestGoBGPRouteModeType5Only with the real inputs
// of the numbered lab node (test/e2e/clab/data/*-gobgp-numbered): its
// provision VNI is an FRR L3VNI. With its provision_gateway the mode stays
// as configured (#662); without it, mergeInferredEVPNMode selects Type-5
// only and logs the choice once.
func TestMergeInferredEVPNModeNumberedLab(t *testing.T) {
	data := filepath.Join("test", "e2e", "clab", "data")
	f, err := os.Open(filepath.Join(data, "vars-gobgp-numbered"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := caprf.ParseVars(f)
	if err != nil {
		t.Fatal(err)
	}
	fs := filepath.Join(data, "fs-gobgp-numbered", "etc")
	npCfg, err := detectNetplanConfig(filepath.Join(fs, "netplan"), filepath.Join(fs, "frr", "frr.conf"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	merged := func(gateway string) (*network.Config, string) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer slog.SetDefault(prev)
		c := *cfg
		c.Network.EVPN.ProvisionGateway = gateway
		n := networkConfigFromMachineConfig(&c)
		mergeNetplanConfig(n, npCfg)
		return n, buf.String()
	}
	if cfg.Network.EVPN.ProvisionGateway == "" {
		t.Fatal("the lab vars no longer set provision_gateway; update this test")
	}
	n, logs := merged(cfg.Network.EVPN.ProvisionGateway)
	if n.EVPNType5Only || n.EVPNL2Enabled || strings.Contains(logs, "provision VNI is an FRR L3VNI") {
		t.Errorf("with provision_gateway: Type-5 only %v, L2 %v, log %q; want the configured mode", n.EVPNType5Only, n.EVPNL2Enabled, logs)
	}
	n, logs = merged("")
	if !n.EVPNType5Only || n.EVPNL2Enabled || strings.Count(logs, "provision VNI is an FRR L3VNI") != 1 {
		t.Errorf("without provision_gateway: Type-5 only %v, L2 %v, log %q; want Type-5 only, logged once", n.EVPNType5Only, n.EVPNL2Enabled, logs)
	}
}

// network.bgp.bfdProfiles replaces the frr.conf profiles: the frr.conf BFD
// sessions take the native timers, an unknown profile the FRR defaults.
func TestApplyNativeBFDProfiles(t *testing.T) {
	tx := uint32(150)
	netCfg := &network.Config{BGPSessions: []network.BGPSession{
		{Name: "a", BFD: true, BFDProfile: "fabric", BFDTransmitMS: 300, BFDReceiveMS: 300, BFDDetectMult: 3},
		{Name: "b", BFD: true, BFDProfile: "other", BFDTransmitMS: 50, BFDReceiveMS: 50, BFDDetectMult: 5},
		{Name: "c"},
	}}
	bgp := &config.BGPConfig{BFDProfiles: []config.BFDProfileConfig{{Name: "fabric", TransmitInterval: &tx, DetectMultiplier: new(uint32(4))}}}
	if err := applyNativeSessions(bgp, netCfg); err != nil {
		t.Fatal(err)
	}
	got := netCfg.BGPSessions
	if got[0].BFDTransmitMS != 150 || got[0].BFDReceiveMS != 300 || got[0].BFDDetectMult != 4 ||
		got[1].BFDTransmitMS != 300 || got[1].BFDDetectMult != 3 || got[2].BFDTransmitMS != 0 {
		t.Fatalf("sessions = %+v", got)
	}
	bad := &config.BGPConfig{BFDProfiles: []config.BFDProfileConfig{{Name: "fabric", DetectMultiplier: new(uint32(1))}}}
	if err := applyNativeSessions(bad, netCfg); err == nil || !strings.Contains(err.Error(), "detect-multiplier 1 out of range 2-255") {
		t.Fatalf("applyNativeSessions() = %v, want the range error", err)
	}
}

// The bm4x route-maps and community-list from frr.conf and from the native
// vars are the same; a native list replaces the frr.conf list as a whole.
// "set metric" fails in both forms.
func TestNativeRouteMapsEqualFRRConf(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("pkg", "network", "frrconf", "testdata", "bm4x", "two-uplinks", "frr.conf"))
	if err != nil {
		t.Fatal(err)
	}
	params, err := netplan.ParseFRRConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	parsed := netplan.ToNetworkConfig(&netplan.Config{}, params)
	vars, err := caprf.ParseVars(strings.NewReader(`BGP_ROUTE_MAPS=[{"name":"TAG-FABRIC-IN","entries":[{"seq":10,"setLocalPreference":90,"setCommunities":["65169:200"],"setCommunityAdditive":true}]},{"name":"DENY-TAG-FABRIC-OUT","entries":[{"seq":10,"deny":true,"matchCommunity":"cm-received-fabric"},{"seq":20}]}]
BGP_COMMUNITY_LISTS=[{"name":"cm-received-fabric","entries":[{"communities":["65169:200"]}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	native := networkConfigFromMachineConfig(vars)
	if !reflect.DeepEqual(native.BGPRouteMaps, parsed.BGPRouteMaps) || !reflect.DeepEqual(native.BGPCommunityLists, parsed.BGPCommunityLists) {
		t.Fatalf("native %+v %+v\nfrr.conf %+v %+v", native.BGPRouteMaps, native.BGPCommunityLists, parsed.BGPRouteMaps, parsed.BGPCommunityLists)
	}

	merged := &network.Config{BGPRouteMaps: []network.RouteMap{{Name: "ONLY"}}}
	mergeBGPPolicy(merged, parsed)
	if len(merged.BGPRouteMaps) != 1 || merged.BGPRouteMaps[0].Name != "ONLY" || !reflect.DeepEqual(merged.BGPCommunityLists, parsed.BGPCommunityLists) {
		t.Fatalf("merged route-maps %+v, community-lists %+v", merged.BGPRouteMaps, merged.BGPCommunityLists)
	}

	if _, err := netplan.ParseFRRConfigBytes([]byte("route-map M permit 10\n set metric 10\nexit\n")); err == nil {
		t.Error("frr.conf: set metric parsed without error")
	}
	if _, err := caprf.ParseVars(strings.NewReader(`BGP_ROUTE_MAPS=[{"name":"M","entries":[{"seq":10,"setMetric":10}]}]` + "\n")); err == nil {
		t.Error("vars: setMetric parsed without error")
	}
}

// The BM4X lab failure (#662): the rendered bm4x templates without
// provision_gateway must pass the GoBGP config check after the netplan
// merge, because the L3VNI selects Type-5 only. Chains the real parse,
// the merge and gobgp.Config.Validate.
func TestBM4XTemplatesWithoutGatewayValidate(t *testing.T) {
	fixture := filepath.Join("pkg", "network", "frrconf", "testdata", "bm4x", "two-uplinks")
	data, err := os.ReadFile(filepath.Join(fixture, "netplan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "50-bm4x.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	npCfg, err := detectNetplanConfig(dir, filepath.Join(fixture, "frr.conf"), true, true)
	if err != nil {
		t.Fatalf("detectNetplanConfig() = %v", err)
	}
	netCfg := networkConfigFromMachineConfig(&config.MachineConfig{})
	mergeNetplanConfig(netCfg, npCfg)
	if !netCfg.EVPNType5Only || netCfg.ProvisionGateway != "" {
		t.Fatalf("Type-5 only = %v, gateway %q, want Type-5 only without a gateway", netCfg.EVPNType5Only, netCfg.ProvisionGateway)
	}
	cfg, err := gobgp.NewConfig(netCfg)
	if err != nil {
		t.Fatalf("gobgp.NewConfig() = %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want the bm4x templates to pass without provision_gateway", err)
	}
}

// The bm4x templates and the native vars give the same Type-5 settings.
func TestType5KeysEqualBM4XTemplates(t *testing.T) {
	dir := filepath.Join("pkg", "network", "frrconf", "testdata", "bm4x", "two-uplinks")
	parsed, err := detectNetplanConfig(dir, filepath.Join(dir, "frr.conf"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	merged := networkConfigFromMachineConfig(&config.MachineConfig{})
	mergeNetplanConfig(merged, parsed)
	vars, err := caprf.ParseVars(strings.NewReader(strings.Join([]string{
		"LOOPBACK_ADDRESSES=" + strings.Join(merged.LoopbackAddrs, ","),
		"BLACKHOLE_ROUTES=" + strings.Join(merged.BlackholeRoutes, ","),
		"EVPN_TYPE5_NETWORKS=" + strings.Join(merged.Type5Networks, ","),
		"VXLAN_NAME=" + merged.VXLANName,
		"EVPN_TYPE5_ONLY=true",
	}, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	native := networkConfigFromMachineConfig(vars)
	want := []string{"2001:db8:40a:5481::17/128", "2001:db8:40a:5481::/64"}
	if !slices.Equal(merged.LoopbackAddrs, want[:1]) || !slices.Equal(merged.BlackholeRoutes, want[1:]) || !slices.Equal(merged.Type5Networks, want) ||
		merged.VXLANName != "vx.3000" || !merged.EVPNType5Only {
		t.Fatalf("templates = lo %v blackholes %v networks %v vxlan %q type5only %v", merged.LoopbackAddrs, merged.BlackholeRoutes, merged.Type5Networks, merged.VXLANName, merged.EVPNType5Only)
	}
	if !slices.Equal(native.LoopbackAddrs, merged.LoopbackAddrs) || !slices.Equal(native.BlackholeRoutes, merged.BlackholeRoutes) ||
		!slices.Equal(native.Type5Networks, merged.Type5Networks) || native.VXLANName != merged.VXLANName || native.EVPNType5Only != merged.EVPNType5Only {
		t.Fatalf("native = lo %v blackholes %v networks %v vxlan %q type5only %v", native.LoopbackAddrs, native.BlackholeRoutes, native.Type5Networks, native.VXLANName, native.EVPNType5Only)
	}
}

func TestApplyBGPOptions(t *testing.T) {
	frrSessions := func() []network.BGPSession {
		return []network.BGPSession{
			{Name: "eth1", Interface: true, LocalASN: 65501, AllowASIn: 3, ConnectRetry: 120},
			{Name: "198.51.100.1", Internal: true, VRF: "Vrf_underlay", AllowASIn: 3, ConnectRetry: 5, ConnectRetrySet: true},
			{Name: "198.51.100.9", RemoteASN: 65009, ConnectRetry: 120},
		}
	}
	t.Run("frr defaults", func(t *testing.T) {
		n := &network.Config{BGPSessions: frrSessions(), BGPMultipathRelax: true}
		applyBGPOptions(&config.BGPConfig{}, n)
		if !n.BGPInstallRoutes || !n.BGPMultipathRelax {
			t.Fatalf("frr.conf defaults = %+v", n)
		}
		if !reflect.DeepEqual(n.BGPSessions, frrSessions()) {
			t.Fatalf("sessions changed without explicit options: %+v", n.BGPSessions)
		}
	})
	t.Run("native defaults off", func(t *testing.T) {
		n := &network.Config{}
		applyBGPOptions(&config.BGPConfig{}, n)
		if n.BGPInstallRoutes || n.BGPMultipathRelax || n.BGPAllowASIn != 0 {
			t.Fatalf("native defaults = %+v", n)
		}
	})
	// The peer options are defaults: a session value wins.
	t.Run("peer options are session defaults", func(t *testing.T) {
		n := &network.Config{BGPSessions: frrSessions(), BGPMultipathRelax: true}
		applyBGPOptions(&config.BGPConfig{
			InstallRoutes: new(false), FastExternalFailover: new(false), MultipathRelax: new(false),
			AllowASIn: new(uint32(2)), ConnectRetry: 10, LocalAS: 65999, NeighborVRF: "Vrf_x",
		}, n)
		if n.BGPInstallRoutes || n.BGPMultipathRelax {
			t.Fatalf("explicit false lost: %+v", n)
		}
		want := frrSessions()
		want[2].AllowASIn = 2
		want[0].ConnectRetry, want[0].ConnectRetrySet = 10, true
		want[2].ConnectRetry, want[2].ConnectRetrySet, want[2].LocalASN, want[2].VRF = 10, true, 65999, "Vrf_x"
		if !reflect.DeepEqual(n.BGPSessions, want) {
			t.Fatalf("sessions = %+v, want %+v", n.BGPSessions, want)
		}
	})
	// The BFD keys are native wins: set values replace the frr.conf
	// profile values of the sessions with bfd.
	t.Run("BFD timers win", func(t *testing.T) {
		n := &network.Config{BGPSessions: []network.BGPSession{
			{Name: "eth1", Interface: true, BFD: true, BFDTransmitMS: 300, BFDReceiveMS: 300, BFDDetectMult: 3},
			{Name: "eth2", Interface: true},
		}}
		applyBGPOptions(&config.BGPConfig{BFDTransmitMS: 150, BFDReceiveMS: 200, BFDDetectMultiplier: 5}, n)
		want := []network.BGPSession{
			{Name: "eth1", Interface: true, BFD: true, BFDTransmitMS: 150, BFDReceiveMS: 200, BFDDetectMult: 5},
			{Name: "eth2", Interface: true},
		}
		if !reflect.DeepEqual(n.BGPSessions, want) {
			t.Fatalf("sessions = %+v, want %+v", n.BGPSessions, want)
		}
	})
}

// BGP_ROUTE_MAP_IN alone replaces only the in route-maps; the frr.conf out
// route-map of each family stays.
func TestApplyBGPOptionsRouteMapInKeepsOut(t *testing.T) {
	n := &network.Config{BGPSessions: []network.BGPSession{{Name: "eth1", Interface: true, IPv4Unicast: true,
		RouteMaps: []network.SessionRouteMaps{{Family: "ipv4-unicast", In: "TAG-FABRIC-IN", Out: "DENY-TAG-FABRIC-OUT"}}}}}
	applyBGPOptions(&config.BGPConfig{RouteMapIn: "NATIVE-IN"}, n)
	want := []network.SessionRouteMaps{{Family: "ipv4-unicast", In: "NATIVE-IN", Out: "DENY-TAG-FABRIC-OUT"}}
	if !reflect.DeepEqual(n.BGPSessions[0].RouteMaps, want) {
		t.Fatalf("route-maps = %+v, want %+v", n.BGPSessions[0].RouteMaps, want)
	}
}

// The frr.conf route targets win over the legacy vpn_rt key, as in FRR;
// native route target lists win over both; vpn_rt stays for configs
// without frr.conf route targets.
func TestMergeBGPPolicyRouteTargets(t *testing.T) {
	frr := &network.Config{EVPNImportRTs: []string{"65169:3000"}, EVPNExportRTs: []string{"65169:3000"}}
	dst := &network.Config{VPNRT: "65000:100"}
	mergeBGPPolicy(dst, frr)
	if strings.Join(dst.EVPNImportRTs, ",") != "65169:3000" || strings.Join(dst.EVPNExportRTs, ",") != "65169:3000" {
		t.Fatalf("with vpn_rt: route targets %v / %v, want the frr.conf ones", dst.EVPNImportRTs, dst.EVPNExportRTs)
	}
	dst = &network.Config{VPNRT: "65000:100", EVPNImportRTs: []string{"1:1"}}
	mergeBGPPolicy(dst, frr)
	if strings.Join(dst.EVPNImportRTs, ",") != "1:1" || len(dst.EVPNExportRTs) != 0 {
		t.Fatalf("native lists: %v / %v, want the native import list only", dst.EVPNImportRTs, dst.EVPNExportRTs)
	}
	dst = &network.Config{VPNRT: "65000:100"}
	mergeBGPPolicy(dst, &network.Config{})
	if dst.VPNRT != "65000:100" || len(dst.EVPNImportRTs) != 0 {
		t.Fatalf("without frr.conf route targets: %+v", dst)
	}
}

// The native BFD timers reach the network config.
func TestNetworkConfigFromMachineConfigBFD(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Network.BGP.BFDTransmitMS, cfg.Network.BGP.BFDReceiveMS, cfg.Network.BGP.BFDDetectMultiplier = 150, 200, 5
	n := networkConfigFromMachineConfig(cfg)
	if n.BFDTransmitMS != 150 || n.BFDReceiveMS != 200 || n.BFDDetectMult != 5 {
		t.Fatalf("BFD = %d/%d x%d, want 150/200 x5", n.BFDTransmitMS, n.BFDReceiveMS, n.BFDDetectMult)
	}
}

// The native device options reach the network config.
func TestNetworkConfigFromMachineConfigDevices(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Network.Overlay = network.OverlayDevices{VXLANMTU: 9000, BridgeMTU: 8900, BridgeSTP: new(false), VXLANLinkLocal: new(false)}
	cfg.Network.NoAcceptRA = "ens1f0np0, ens1f1np1"
	cfg.Network.EmitLLDP = "ens1f0np0"
	cfg.Network.IPForwarding, cfg.Network.IPv6Forwarding = new(false), new(true)
	n := networkConfigFromMachineConfig(cfg)
	if !reflect.DeepEqual(n.OverlayDevices, cfg.Network.Overlay) {
		t.Errorf("OverlayDevices = %+v, want %+v", n.OverlayDevices, cfg.Network.Overlay)
	}
	if !slices.Equal(n.NoAcceptRA, []string{"ens1f0np0", "ens1f1np1"}) || !slices.Equal(n.EmitLLDP, []string{"ens1f0np0"}) {
		t.Errorf("NoAcceptRA %v EmitLLDP %v", n.NoAcceptRA, n.EmitLLDP)
	}
	if n.IPForwarding == nil || *n.IPForwarding || n.IPv6Forwarding == nil || !*n.IPv6Forwarding {
		t.Errorf("forwarding = %v %v, want false and true", n.IPForwarding, n.IPv6Forwarding)
	}
}

func TestNetworkConfigFromMachineConfigOverlayVRF(t *testing.T) {
	for _, tt := range []struct{ name, wantName string }{{"default", ""}, {"Vrf_cust", "Vrf_cust"}} {
		cfg := &config.MachineConfig{}
		cfg.Network.VRF.Name = "Vrf_underlay"
		cfg.Network.VRF.OverlayName = tt.name
		n := networkConfigFromMachineConfig(cfg)
		if !n.OverlayVRFSet || n.OverlayVRFName != tt.wantName {
			t.Errorf("OVERLAY_VRF_NAME=%s: set %v name %q, want %q", tt.name, n.OverlayVRFSet, n.OverlayVRFName, tt.wantName)
		}
	}
}

// BRIDGE_MAC is a key of this release: a set native MAC wins over the
// netplan macaddress of the same bridge.
func TestMergeNetplanConfigNativeBridgeMAC(t *testing.T) {
	dst := &network.Config{BridgeMAC: "02:00:00:00:aa:01"}
	src := &network.Config{BridgeMAC: "02:00:00:00:10:01"}
	mergeNetplanConfig(dst, src)
	if dst.BridgeMAC != "02:00:00:00:aa:01" {
		t.Fatalf("BridgeMAC = %q, want the native 02:00:00:00:aa:01", dst.BridgeMAC)
	}
}

// A set native device setting wins over netplan; frr.conf forwarding is
// only a default.
func TestMergeNetplanConfigOverlayDevices(t *testing.T) {
	dst := &network.Config{
		OverlayDevices: network.OverlayDevices{VXLANMTU: 8000, VXLANMAC: "02:00:00:00:00:01", PortHairpin: new(false)},
		IPForwarding:   new(true),
	}
	src := &network.Config{
		OverlayDevices: network.OverlayDevices{VXLANMTU: 9000, VXLANLinkLocal: new(false)},
		NoAcceptRA:     []string{"ens1f0np0"},
		EmitLLDP:       []string{"ens1f0np0"},
		IPForwarding:   new(false),
		IPv6Forwarding: new(false),
	}

	mergeNetplanConfig(dst, src)

	d := dst.OverlayDevices
	// A set native value wins; netplan fills the unset ones.
	if d.VXLANMTU != 8000 || d.VXLANMAC != "02:00:00:00:00:01" || d.PortHairpin == nil || *d.PortHairpin ||
		d.VXLANLinkLocal == nil || *d.VXLANLinkLocal {
		t.Errorf("overlay devices = %+v", d)
	}
	if !*dst.IPForwarding || dst.IPv6Forwarding == nil || *dst.IPv6Forwarding {
		t.Errorf("forwarding = %v %v, want explicit true and frr.conf false", *dst.IPForwarding, dst.IPv6Forwarding)
	}
	if !slices.Equal(dst.NoAcceptRA, []string{"ens1f0np0"}) || !slices.Equal(dst.EmitLLDP, []string{"ens1f0np0"}) {
		t.Errorf("NoAcceptRA = %v, EmitLLDP = %v", dst.NoAcceptRA, dst.EmitLLDP)
	}
}

// Every overlay device setting follows the native rule on its own: a set
// native value wins, an unset one takes the netplan value. The native
// NoAcceptRA and EmitLLDP lists replace the netplan lists.
func TestMergeOverlayDevicesEveryField(t *testing.T) {
	native, parsed := network.OverlayDevices{}, network.OverlayDevices{}
	nv, pv := reflect.ValueOf(&native).Elem(), reflect.ValueOf(&parsed).Elem()
	for i := range nv.NumField() {
		switch f := nv.Field(i); f.Kind() {
		case reflect.Int:
			f.SetInt(1000)
			pv.Field(i).SetInt(2000)
		case reflect.String:
			f.SetString("02:00:00:00:00:01")
			pv.Field(i).SetString("02:00:00:00:00:02")
		case reflect.Pointer:
			f.Set(reflect.ValueOf(new(true)))
			pv.Field(i).Set(reflect.ValueOf(new(false)))
		default:
			t.Fatalf("field %s of kind %s not covered", nv.Type().Field(i).Name, f.Kind())
		}
	}
	got := native
	mergeOverlayDevices(&got, &parsed)
	if !reflect.DeepEqual(got, native) {
		t.Errorf("native set: got %+v, want the native values %+v", got, native)
	}
	got = network.OverlayDevices{}
	mergeOverlayDevices(&got, &parsed)
	if !reflect.DeepEqual(got, parsed) {
		t.Errorf("native unset: got %+v, want the netplan values %+v", got, parsed)
	}

	dst := &network.Config{NoAcceptRA: []string{"eth9"}, EmitLLDP: []string{"eth9"}}
	mergeNetplanConfig(dst, &network.Config{NoAcceptRA: []string{"ens1f0np0"}, EmitLLDP: []string{"ens1f0np0"}})
	if !slices.Equal(dst.NoAcceptRA, []string{"eth9"}) || !slices.Equal(dst.EmitLLDP, []string{"eth9"}) {
		t.Errorf("native lists: NoAcceptRA %v EmitLLDP %v, want [eth9]", dst.NoAcceptRA, dst.EmitLLDP)
	}
}

// network.devices (YAML) and NETWORK_DEVICES (vars) are the native form of
// the provisioner netplan: the bm4x templates give the same network config
// from the netplan file and from the native YAML. An unknown key and a
// route key BOOTy does not apply fail in every form.
// nativeDevicesYAML writes a BOOTy YAML config with the "network:" section
// of a netplan file as network.devices and returns its path.
func nativeDevicesYAML(t *testing.T, netplanFile []byte) string {
	t.Helper()
	var np map[string]any
	if err := yaml.Unmarshal(netplanFile, &np); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(map[string]any{"network": map[string]any{"devices": np["network"]}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "booty.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeDevicesEqualNetplan(t *testing.T) {
	for _, fixture := range []string{"two-uplinks", "one-uplink", "two-uplinks-layer2"} {
		t.Run(fixture, func(t *testing.T) {
			dir := filepath.Join("pkg", "network", "frrconf", "testdata", "bm4x", fixture)
			data, err := os.ReadFile(filepath.Join(dir, "netplan.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			fromFile, err := detectNetplanConfig(dir, filepath.Join(dir, "frr.conf"), true, true)
			if err != nil {
				t.Fatal(err)
			}
			// The native YAML: the "network:" section under network.devices,
			// loaded by the production loader.
			cfg, err := config.Load(nativeDevicesYAML(t, data))
			if err != nil {
				t.Fatal(err)
			}
			native, err := detectNetplanConfigWith(t.TempDir(), filepath.Join(dir, "frr.conf"), true, true, cfg.Network.Devices.Section())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(native, fromFile) {
				t.Fatalf("native devices differ from netplan:\n native  %+v\n netplan %+v", native, fromFile)
			}
		})
	}

	bad := "network:\n  ethernets:\n    eth0:\n      wakeonlan: true\n"
	if _, err := netplan.ParseBytesWith("file", []byte(bad), true); err == nil {
		t.Error("netplan file: unknown key parsed")
	}
	// config.Load is not strict, but network.devices always is.
	if _, err := config.Load(nativeDevicesYAML(t, []byte(bad))); err == nil || !strings.Contains(err.Error(), "wakeonlan") {
		t.Errorf("network.devices via config.Load: unknown key error = %v", err)
	}
	jsonPath := filepath.Join(t.TempDir(), "booty.json")
	if err := os.WriteFile(jsonPath, []byte(`{"network":{"devices":{"ethernets":{"eth0":{"wakeonlan":true}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(jsonPath); err == nil {
		t.Error("network.devices in JSON: unknown key parsed")
	}
	if _, err := caprf.ParseVars(strings.NewReader(`NETWORK_DEVICES={"ethernets":{"eth0":{"wakeonlan":true}}}` + "\n")); err == nil {
		t.Error("NETWORK_DEVICES: unknown key parsed")
	}
	route := &netplan.NetworkSection{Ethernets: map[string]netplan.EthernetConfig{"eth0": {Routes: []netplan.RouteConfig{{To: "default", Via: "192.0.2.1", Metric: 50}}}}}
	if _, err := detectNetplanConfigWith(t.TempDir(), "/nonexistent", true, true, route); err == nil {
		t.Error("network.devices: route metric accepted")
	}
	if _, err := caprf.ParseVars(strings.NewReader(`NETWORK_DEVICES={"ethernets":{"eth0":{"routes":[{"to":"default","via":"192.0.2.1","metric":50}]}}}` + "\n")); err == nil {
		t.Error("NETWORK_DEVICES: route metric accepted")
	}
}
