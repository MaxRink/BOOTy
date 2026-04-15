//go:build linux

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/network"
)

type failNetworkMode struct {
	connectErr error
}

func (f *failNetworkMode) Setup(context.Context, *network.Config) error { return nil }
func (f *failNetworkMode) Teardown(context.Context) error               { return nil }
func (f *failNetworkMode) WaitForConnectivity(_ context.Context, _ string, _ time.Duration) error {
	return f.connectErr
}

func TestSetupNetworkMode_VLANParseError(t *testing.T) {
	cfg := &config.MachineConfig{
		VLANs: "not-valid-vlan-json!!!",
	}
	_, err := setupNetworkMode(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error from invalid VLAN config, got nil")
	}
	if !strings.Contains(err.Error(), "VLAN") {
		t.Fatalf("expected VLAN error, got: %v", err)
	}
}

func TestEnsureNetworkConnectivity_RetrySetupFailureIsFatal(t *testing.T) {
	mode := &failNetworkMode{connectErr: errors.New("no route to host")}
	cfg := &config.MachineConfig{
		VLANs: "bad-vlan-json",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := ensureNetworkConnectivity(ctx, cfg, mode, "http://192.0.2.1")
	if err == nil {
		t.Fatal("expected fatal error from setupNetworkMode failure during retry, got nil")
	}
	if !strings.Contains(err.Error(), "network retry setup") {
		t.Fatalf("expected retry-setup error, got: %v", err)
	}
}

func TestSetupNetworkMode_BondModeNoInterfaces_ReturnsError(t *testing.T) {
	cfg := &config.MachineConfig{
		NetworkMode:    "bond",
		BondInterfaces: "",
	}
	_, err := setupNetworkMode(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when bond mode has no interfaces configured")
	}
	if !strings.Contains(err.Error(), "bond") {
		t.Fatalf("expected bond-related error, got: %v", err)
	}
}
