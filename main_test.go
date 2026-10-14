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
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/network"
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

func TestMergeNetplanConfigBridgeMAC(t *testing.T) {
	tests := []struct {
		name      string
		srcBridge string
		want      string
	}{
		{"default_bridge", "", "02:00:00:00:10:01"},
		{"same_bridge", "br.provision", "02:00:00:00:10:01"},
		{"other_bridge", "br_cluster", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := &network.Config{}
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
	cfg.Network.EVPN.Type5Only = true

	netCfg := networkConfigFromMachineConfig(cfg)

	if !netCfg.EVPNType5Only {
		t.Fatal("EVPN Type-5-only mode was not wired into the network stack config")
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
		// bm4x: L3VNI without advertise-all-vni and without provision_gateway.
		{name: "L3VNI without advertise-all-vni", dstVNI: 3000, srcL3VNIs: []uint32{3000}, wantType5: true},
		// CAPRF sets provision_gateway: the mode stays, the BUM FDB entry too.
		{name: "L3VNI with provision gateway", dstVNI: 1000, dstGateway: "10.0.0.1", srcL2: true, srcL3VNIs: []uint32{1000}},
		{name: "explicit L2 wins over L3VNI", dstL2: true, dstVNI: 1000, srcL3VNIs: []uint32{1000}, wantL2: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := &network.Config{EVPNL2Enabled: tt.dstL2, EVPNType5Only: tt.dstType5, ProvisionVNI: tt.dstVNI, ProvisionGateway: tt.dstGateway}
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

func TestTokenRenewalFatalFailureActionWaitStopsTheTask(t *testing.T) {
	rebooted, stopped := false, false
	tokenRenewalFatal(&config.MachineConfig{FailureAction: config.FailureActionWait}, func() { rebooted = true }, func() { stopped = true })
	if rebooted || !stopped {
		t.Fatalf("FAILURE_ACTION=wait: rebooted=%v stopped=%v, want the task stopped without reboot", rebooted, stopped)
	}
	rebooted, stopped = false, false
	tokenRenewalFatal(&config.MachineConfig{}, func() { rebooted = true }, func() { stopped = true })
	if !rebooted || stopped {
		t.Fatalf("default: rebooted=%v stopped=%v, want a reboot", rebooted, stopped)
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
		cfg, err := detectNetplanConfig(t.TempDir(), "/nonexistent/frr.conf")
		if cfg != nil || err != nil {
			t.Fatalf("detectNetplanConfig() = %v, %v, want nil, nil", cfg, err)
		}
	})
	t.Run("unreadable netplan dir", func(t *testing.T) {
		notDir := filepath.Join(t.TempDir(), "netplan")
		writeFile(t, notDir, "x")
		if _, err := detectNetplanConfig(notDir, "/nonexistent/frr.conf"); err == nil {
			t.Fatal("detectNetplanConfig() = nil error, want error for a netplan path that is not a dir")
		}
	})
	t.Run("valid netplan without frr.conf", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "50-booty.yaml"), validNetplan)
		cfg, err := detectNetplanConfig(dir, filepath.Join(dir, "missing-frr.conf"))
		if err != nil || cfg == nil {
			t.Fatalf("detectNetplanConfig() = %v, %v, want a config", cfg, err)
		}
	})
	t.Run("malformed netplan", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "50-booty.yaml"), "network: [unclosed\n")
		if _, err := detectNetplanConfig(dir, filepath.Join(dir, "frr.conf")); err == nil {
			t.Fatal("detectNetplanConfig() = nil error, want parse error")
		}
	})
	t.Run("malformed frr.conf", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "50-booty.yaml"), validNetplan)
		frr := filepath.Join(dir, "frr.conf")
		writeFile(t, frr, "router bgp 99999999999\n")
		if _, err := detectNetplanConfig(dir, frr); err == nil {
			t.Fatal("detectNetplanConfig() = nil error, want FRR parse error")
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
