//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/payload"
	"github.com/telekom/BOOTy/pkg/provision"
)

// splitRamdisk makes a payload directory of a split build that left out
// split and loaded the archives in loaded, all of build b1.
func splitRamdisk(t *testing.T, split string, loaded ...string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "payloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"split": split}
	for _, l := range loaded {
		files[l] = "build=b1\n"
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	origDir, origBuild := payload.Dir, Build
	t.Cleanup(func() { payload.Dir, Build = origDir, origBuild })
	payload.Dir, Build = dir, "b1"
}

// checkPayloads refuses a payload archive of another build.
func TestCheckPayloads(t *testing.T) {
	for _, tt := range []struct {
		name  string
		build string
		code  string
	}{
		{name: "payload of this build"},
		{name: "payload of another build", build: "b2", code: "BOOTY-PAY-002"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			splitRamdisk(t, "frr", "frr")
			if tt.build != "" {
				Build = tt.build
			}
			err := checkPayloads()
			if got := provision.CodeOf(provision.WithCode("check-payloads", err)); got != tt.code {
				t.Fatalf("code = %q (%v), want %q", got, err, tt.code)
			}
		})
	}
}

// FRR mode on a split ramdisk without the FRR archive fails with the
// payload code instead of falling back to DHCP.
func TestSetupNetworkModeFRRWithoutPayload(t *testing.T) {
	splitRamdisk(t, "frr")
	cfg := &config.MachineConfig{}
	cfg.Network.BGP.ASN = 65000
	cfg.Network.EVPN.UnderlayIP = "10.0.0.1"
	cfg.Network.PredictableNames = new(false)
	_, err := setupNetworkMode(context.Background(), cfg)
	if got := provision.CodeOf(provision.WithCode("network-setup", err)); got != provision.CodePayloadMissing {
		t.Fatalf("code = %q (%v), want %s", got, err, provision.CodePayloadMissing)
	}
}

// The network setup error keeps the payload code instead of the generic
// network setup code (provision.WithCode in setupNetworkAndTokenFlow).
func TestSetupNetworkAndTokenFlowKeepsThePayloadCode(t *testing.T) {
	splitRamdisk(t, "frr")
	cfg := &config.MachineConfig{}
	cfg.Network.BGP.ASN = 65000
	cfg.Network.EVPN.UnderlayIP = "10.0.0.1"
	cfg.Network.PredictableNames = new(false)
	_, _, err := setupNetworkAndTokenFlow(context.Background(), cfg, nil, func() {})
	if got := provision.CodeOf(err); got != provision.CodePayloadMissing {
		t.Fatalf("code = %q (%v), want %s", got, err, provision.CodePayloadMissing)
	}
}
