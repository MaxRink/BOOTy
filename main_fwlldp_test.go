//go:build linux

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/sriov"
)

// BOOTy stops the firmware LLDP agent on every NIC except the ports whose
// agent an SR-IOV profile sets: there the profile wins.
func TestDisableNICFirmwareLLDPLeavesSRIOVPorts(t *testing.T) {
	origDetect, origDiscover, origDisable := detectNICs, discoverPFs, disableFirmwareLLDP
	t.Cleanup(func() { detectNICs, discoverPFs, disableFirmwareLLDP = origDetect, origDiscover, origDisable })
	detectNICs = func(context.Context) ([]string, error) { return []string{"ens1f0", "ens1f1", "eno1"}, nil }
	discoverPFs = func() ([]sriov.PF, error) {
		return []sriov.PF{{Interface: "ens1f0", Driver: "ice"}, {Interface: "ens1f1", Driver: "ice"}}, nil
	}
	var disabled []string
	disableFirmwareLLDP = func(nic string) (string, error) {
		disabled = append(disabled, nic)
		return "fw-lldp-agent", nil
	}

	disableNICFirmwareLLDP(context.Background(), &sriov.Config{})
	if want := []string{"ens1f0", "ens1f1", "eno1"}; !reflect.DeepEqual(disabled, want) {
		t.Fatalf("without SR-IOV profiles: disabled %v, want %v", disabled, want)
	}

	agent := true
	disabled = nil
	cfg := &sriov.Config{Profiles: []sriov.Profile{{Name: "dcb", Match: sriov.Match{Interface: "ens1f1"}, Firmware: sriov.FirmwareSettings{FWLLDPAgent: &agent}}}}
	disableNICFirmwareLLDP(context.Background(), cfg)
	if want := []string{"ens1f0", "eno1"}; !reflect.DeepEqual(disabled, want) {
		t.Fatalf("with an fwLLDPAgent profile on ens1f1: disabled %v, want %v", disabled, want)
	}
}

// prepareFirmwareLLDP stops the agent now and wires lldpPrepareNICs, so a
// NIC that appears later gets it stopped too; with the agent kept neither.
func TestPrepareFirmwareLLDPWiresLaterNICs(t *testing.T) {
	origDetect, origDiscover, origDisable, origPrep := detectNICs, discoverPFs, disableFirmwareLLDP, lldpPrepareNICs
	t.Cleanup(func() {
		detectNICs, discoverPFs, disableFirmwareLLDP, lldpPrepareNICs = origDetect, origDiscover, origDisable, origPrep
	})
	detectNICs = func(context.Context) ([]string, error) { return []string{"eno1"}, nil }
	discoverPFs = func() ([]sriov.PF, error) { return nil, nil }
	var disabled []string
	disableFirmwareLLDP = func(nic string) (string, error) {
		disabled = append(disabled, nic)
		return "fw-lldp-agent", nil
	}
	for _, keep := range []bool{false, true} {
		disabled, lldpPrepareNICs = nil, func([]string) {}
		cfg := &config.MachineConfig{}
		if keep {
			off := false
			cfg.Network.LLDP.DisableFirmwareAgent = &off
		}
		prepareFirmwareLLDP(context.Background(), cfg)
		lldpPrepareNICs([]string{"ens2f0"})
		want := []string{"eno1", "ens2f0"}
		if keep {
			want = nil
		}
		if !reflect.DeepEqual(disabled, want) {
			t.Errorf("keep agent %v: disabled %v, want %v", keep, disabled, want)
		}
	}
}

// setupNetworkAndTokenFlow stops the firmware LLDP agent before the network
// setup and wires lldpPrepareNICs for NICs that appear later.
func TestSetupNetworkStopsFirmwareLLDPFirst(t *testing.T) {
	origDetect, origDiscover, origDisable, origPrep, origSetup := detectNICs, discoverPFs, disableFirmwareLLDP, lldpPrepareNICs, setupNetworkModeFn
	t.Cleanup(func() {
		detectNICs, discoverPFs, disableFirmwareLLDP, lldpPrepareNICs, setupNetworkModeFn = origDetect, origDiscover, origDisable, origPrep, origSetup
	})
	var order []string
	detectNICs = func(context.Context) ([]string, error) { return []string{"eno1"}, nil }
	discoverPFs = func() ([]sriov.PF, error) { return nil, nil }
	disableFirmwareLLDP = func(nic string) (string, error) {
		order = append(order, "agent off "+nic)
		return "fw-lldp-agent", nil
	}
	lldpPrepareNICs = func([]string) {}
	setupNetworkModeFn = func(context.Context, *config.MachineConfig) (network.Mode, error) {
		order = append(order, "network")
		lldpPrepareNICs([]string{"ens2f0"}) // a NIC that appears during the setup
		return nil, errors.New("stop here")
	}
	if _, _, err := setupNetworkAndTokenFlow(context.Background(), &config.MachineConfig{}, nil, func() {}); err == nil {
		t.Fatal("setupNetworkAndTokenFlow succeeded with a failing network setup")
	}
	if want := []string{"agent off eno1", "network", "agent off ens2f0"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
}
