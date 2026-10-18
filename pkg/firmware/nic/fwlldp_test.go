//go:build linux

package nic

import (
	"fmt"
	"testing"

	"github.com/telekom/BOOTy/pkg/network/ethtool"
)

// fakeNIC is a NIC with private flags for the flag seams.
type fakeNIC struct {
	flags map[string]bool
	sets  []string
}

func fakeNICs(t *testing.T, nics map[string]*fakeNIC) {
	t.Helper()
	og, os := getFlags, setFlag
	t.Cleanup(func() { getFlags, setFlag = og, os })
	getFlags = func(name string) ([]ethtool.Flag, error) {
		out := make([]ethtool.Flag, 0, len(nics[name].flags))
		for k, v := range nics[name].flags {
			out = append(out, ethtool.Flag{Name: k, On: v})
		}
		return out, nil
	}
	setFlag = func(name, flag string, on bool) error {
		nic := nics[name]
		nic.flags[flag] = on
		nic.sets = append(nic.sets, fmt.Sprintf("%s=%v", flag, on))
		return nil
	}
}

// BOOTy stops the agent with the driver's flag and keeps it off (there is
// no restore); a flag that is already off is not written, so the driver
// does not reset the port.
func TestDisableFirmwareLLDP(t *testing.T) {
	ice := &fakeNIC{flags: map[string]bool{"fw-lldp-agent": true, "link-down-on-close": false}}
	i40e := &fakeNIC{flags: map[string]bool{"disable-fw-lldp": false}}
	off := &fakeNIC{flags: map[string]bool{"fw-lldp-agent": false}}
	fakeNICs(t, map[string]*fakeNIC{"ice0": ice, "i40e0": i40e, "off0": off})
	for _, tt := range []struct{ nic, driver, flag string }{
		{"ice0", "ice", "fw-lldp-agent"}, {"i40e0", "i40e", "disable-fw-lldp"}, {"off0", "ice", "fw-lldp-agent"}, {"mlx0", "mlx5_core", ""},
	} {
		if flag, err := disableFirmwareLLDP(tt.nic, tt.driver); err != nil || flag != tt.flag {
			t.Fatalf("%s: flag %q, %v", tt.nic, flag, err)
		}
	}
	if ice.flags["fw-lldp-agent"] || !i40e.flags["disable-fw-lldp"] || len(off.sets) != 0 {
		t.Fatalf("after disable: ice %v, i40e %v, already off set %v", ice.flags, i40e.flags, off.sets)
	}
	if len(ice.sets) != 1 || len(i40e.sets) != 1 {
		t.Fatalf("writes: ice %v, i40e %v, want one each", ice.sets, i40e.sets)
	}
	// A driver without the flag (too old) fails and changes nothing.
	old := &fakeNIC{flags: map[string]bool{}}
	fakeNICs(t, map[string]*fakeNIC{"old0": old})
	if _, err := disableFirmwareLLDP("old0", "ice"); err == nil || len(old.sets) != 0 {
		t.Fatalf("missing flag: %v, sets %v", err, old.sets)
	}
}
