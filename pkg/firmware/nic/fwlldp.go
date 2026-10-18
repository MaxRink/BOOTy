//go:build linux

package nic

import (
	"fmt"
	"slices"

	"github.com/telekom/BOOTy/pkg/network/ethtool"
)

// fwLLDPFlags are the private flags that stop the firmware LLDP agent of
// Intel NICs. While that agent runs, it consumes the LLDP frames of the
// switch and the host frames do not go out reliably. Stopping it also
// stops the firmware DCBX of the port.
var fwLLDPFlags = []fwFlag{
	{"i40e", "disable-fw-lldp", true}, // 700 series
	{"ice", "fw-lldp-agent", false},   // 800 series; the NIC keeps it across boots
}

// fwFlag is the private flag of a driver and the value that stops the
// agent.
type fwFlag struct {
	driver string
	flag   string
	on     bool
}

// Test seams for the flag access.
var (
	getFlags = ethtool.PrivateFlags
	setFlag  = ethtool.SetPrivateFlag
)

// This is the one implementation of the firmware LLDP agent switch
// (LLDP_DISABLE_FW_AGENT); the private flags go over ethtool netlink.

// DisableFirmwareLLDP stops the firmware LLDP agent of a NIC through its
// private flag, like "ethtool --set-priv-flags IF disable-fw-lldp on"
// (i40e) or "fw-lldp-agent off" (ice). It does not put the flag back: ice
// keeps it in the NVM, so the agent stays off for the installed OS too,
// on purpose (docs/networking.md). It returns the flag, "" for a driver
// without such an agent.
func DisableFirmwareLLDP(ifname string) (string, error) {
	return disableFirmwareLLDP(ifname, ethtool.Driver(ifname))
}

func disableFirmwareLLDP(ifname, driver string) (string, error) {
	i := slices.IndexFunc(fwLLDPFlags, func(f fwFlag) bool { return f.driver == driver })
	if i < 0 {
		return "", nil
	}
	f := fwLLDPFlags[i]
	flags, err := getFlags(ifname)
	if err != nil {
		return f.flag, err
	}
	j := slices.IndexFunc(flags, func(fl ethtool.Flag) bool { return fl.Name == f.flag })
	switch {
	case j < 0:
		return f.flag, fmt.Errorf("%s has no private flag %s (driver too old)", ifname, f.flag)
	case flags[j].On == f.on:
		return f.flag, nil // already off: no write, no port reset
	}
	return f.flag, setFlag(ifname, f.flag, f.on)
}
