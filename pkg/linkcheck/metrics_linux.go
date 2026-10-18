//go:build linux

package linkcheck

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"

	"github.com/telekom/BOOTy/pkg/network/ethtool"
	"github.com/telekom/BOOTy/pkg/network/sff"
)

// Test seams for the ethtool and netlink reads.
var (
	getLinkModes = ethtool.GetLinkModes
	getLinkState = ethtool.GetLinkState
	getFEC       = ethtool.GetFEC
	readModule   = ethtool.ReadModuleEEPROM
	getCounters  = func(nic string) *Counters {
		link, err := netlink.LinkByName(nic)
		if err != nil || link.Attrs().Statistics == nil {
			return nil
		}
		s := link.Attrs().Statistics
		return &Counters{RxErrors: s.RxErrors, RxCRC: s.RxCrcErrors, RxFrame: s.RxFrameErrors,
			RxMissed: s.RxMissedErrors, RxDropped: s.RxDropped, TxErrors: s.TxErrors}
	}
)

// linkState reads the link layer state of a NIC. Values that the driver
// does not report stay empty.
func linkState(nic string) *LinkState {
	s := &LinkState{Flaps: sysInt(filepath.Join(sysClassNet, nic, "carrier_down_count")), Errors: getCounters(nic)}
	if m, err := getLinkModes(nic); err == nil {
		s.SpeedMbps, s.Lanes, s.Duplex, s.Autoneg = m.SpeedMbps, m.Lanes, m.Duplex, m.Autoneg
	}
	if st, err := getLinkState(nic); err == nil {
		s.ExtState = st.ExtState
	}
	if f, err := getFEC(nic); err == nil {
		s.FEC = &FEC{Active: f.Active, Corrected: f.Corrected, Uncorrectable: f.Uncorrectable, Stats: f.Stats}
	}
	return s
}

// module decodes the transceiver of a NIC; nil without one (no cage,
// empty cage, virtual NIC) or for an unknown type.
func module(nic string) *sff.Module {
	m, err := sff.Decode(func(addr, page, offset, length int) ([]byte, error) {
		return readModule(nic, addr, page, offset, length)
	})
	if err != nil {
		return nil
	}
	return m
}

// pcie reads the PCIe link of the NIC function from sysfs.
func pcie(nic string) *PCIe {
	dev := filepath.Join(sysClassNet, nic, "device")
	p := &PCIe{
		Speed: sysString(filepath.Join(dev, "current_link_speed")), MaxSpeed: sysString(filepath.Join(dev, "max_link_speed")),
		Width: sysInt(filepath.Join(dev, "current_link_width")), MaxWidth: sysInt(filepath.Join(dev, "max_link_width")),
	}
	if p.Speed == "" || p.MaxSpeed == "" {
		return nil // not a PCIe function
	}
	if resolved, err := filepath.EvalSymlinks(dev); err == nil {
		slot := filepath.Dir(resolved) // the upstream port: root port or switch port
		p.SlotMaxSpeed, p.SlotMaxWidth = sysString(filepath.Join(slot, "max_link_speed")), sysInt(filepath.Join(slot, "max_link_width"))
	}
	return p
}

func sysString(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // sysfs path
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func sysInt(path string) int {
	n, _ := strconv.Atoi(sysString(path))
	return n
}
