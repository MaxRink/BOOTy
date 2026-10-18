//go:build linux

package main

import (
	"context"
	"reflect"
	"testing"

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
