//go:build linux

package main

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/network"
)

func TestMergeNetplanConfigPreservesProvisionPrefixForSameHostMask(t *testing.T) {
	dst := &network.Config{ProvisionIP: "10.200.0.10/24"}
	src := &network.Config{ProvisionIP: "10.200.0.10/32"}

	mergeNetplanConfig(dst, src)

	if dst.ProvisionIP != "10.200.0.10/24" {
		t.Fatalf("ProvisionIP = %q, want existing /24 prefix", dst.ProvisionIP)
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

func TestPrepareLinkLayersCreatesBondBeforeVLAN(t *testing.T) {
	previousBond := setupBondLayer
	previousVLAN := setupVLANLayer
	var calls []string
	setupBondLayer = func(_ context.Context, _ *network.Config) error {
		calls = append(calls, "bond")
		return nil
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
	if err := prepareLinkLayers(context.Background(), cfg); err != nil {
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
	setupBondLayer = func(_ context.Context, _ *network.Config) error {
		return nil
	}
	t.Cleanup(func() {
		setupBondLayer = previousBond
	})

	cfg := &network.Config{BondInterfaces: "eth0,eth1"}
	if err := prepareLinkLayers(context.Background(), cfg); err != nil {
		t.Fatalf("prepareLinkLayers: %v", err)
	}
	if cfg.StaticIface != "bond0" {
		t.Fatalf("StaticIface = %q, want bond0", cfg.StaticIface)
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

	if tryKexec(cfg, false) {
		t.Fatal("tryKexec returned true when secure boot re-enable requires hard reboot")
	}
}

func TestSetupNetworkModeExplicitGoBGPFailsClosed(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Network.Mode = "gobgp"
	cfg.Network.BGP.ASN = 65000
	cfg.Network.BGP.UnderlayAF = "invalid"
	cfg.Network.EVPN.UnderlayIP = "10.0.0.1"
	cfg.Network.EVPN.ProvisionVNI = 4000

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

func TestReapExitedChildrenWithDrainsUntilNoChildren(t *testing.T) {
	calls := 0
	reaped := []int{}
	statuses := []syscall.WaitStatus{0, 1}
	waitChild := func() (int, syscall.WaitStatus, error) {
		calls++
		switch calls {
		case 1, 2:
			reaped = append(reaped, calls)
			return calls, statuses[calls-1], nil
		default:
			return -1, 0, syscall.ECHILD
		}
	}

	reapExitedChildrenWith(waitChild)

	if len(reaped) != 2 {
		t.Fatalf("reaped = %v, want two children", reaped)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestReapExitedChildrenWithStopsWhenNoProcessReady(t *testing.T) {
	calls := 0

	reapExitedChildrenWith(func() (int, syscall.WaitStatus, error) {
		calls++
		return 0, 0, nil
	})

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestReapExitedChildrenWithStopsOnUnexpectedError(t *testing.T) {
	calls := 0
	wantErr := errors.New("wait failure")

	reapExitedChildrenWith(func() (int, syscall.WaitStatus, error) {
		calls++
		return -1, 0, wantErr
	})

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestReapExitedChildrenWithRetriesInterruptedWait(t *testing.T) {
	calls := 0

	reapExitedChildrenWith(func() (int, syscall.WaitStatus, error) {
		calls++
		switch calls {
		case 1:
			return -1, 0, syscall.EINTR
		case 2:
			return 42, 0, nil
		default:
			return -1, 0, syscall.ECHILD
		}
	})

	if calls != 3 {
		t.Fatalf("calls = %d, want EINTR retry, child reap, and ECHILD stop", calls)
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
