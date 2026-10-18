//go:build linux

package ethtool

import (
	"fmt"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// The FEC attributes of include/uapi/linux/ethtool_netlink.h (Linux
// 5.13+); x/sys/unix does not have them.
const (
	fecActive        = 4 // ETHTOOL_A_FEC_ACTIVE, u32 link mode bit
	fecStats         = 5 // ETHTOOL_A_FEC_STATS, nest
	fecStatCorrected = 2 // ETHTOOL_A_FEC_STAT_CORRECTED, u64 array
	fecStatUncorr    = 3 // ETHTOOL_A_FEC_STAT_UNCORR, u64 array
)

// LinkModes is the speed, duplex and autonegotiation of a link
// ("ethtool IF").
type LinkModes struct {
	SpeedMbps int    // 0 when unknown
	Duplex    string // full, half or ""
	Autoneg   bool
	Lanes     int // 0 when the driver does not report it
}

// LinkState is the link state with the reason of a link down
// ("ethtool IF": Link detected, Link down reason).
type LinkState struct {
	Up       bool
	ExtState string // for example "no-cable", "link-training-failure"
}

// FEC is the active forward error correction and its block counters
// ("ethtool --show-fec IF", "ethtool -I --show-fec IF").
type FEC struct {
	Active        string // off, rs, baser, llrs or ""
	Corrected     uint64
	Uncorrectable uint64
	Stats         bool // the driver reports the counters
}

// GetLinkModes reads ETHTOOL_MSG_LINKMODES_GET.
func GetLinkModes(ifname string) (LinkModes, error) {
	b, err := get(unix.ETHTOOL_MSG_LINKMODES_GET, ifname, 0)
	if err != nil {
		return LinkModes{}, err
	}
	return parseLinkModes(b)
}

// GetLinkState reads ETHTOOL_MSG_LINKSTATE_GET.
func GetLinkState(ifname string) (LinkState, error) {
	b, err := get(unix.ETHTOOL_MSG_LINKSTATE_GET, ifname, 0)
	if err != nil {
		return LinkState{}, err
	}
	return parseLinkState(b)
}

// GetFEC reads ETHTOOL_MSG_FEC_GET with the statistics.
func GetFEC(ifname string) (FEC, error) {
	b, err := get(unix.ETHTOOL_MSG_FEC_GET, ifname, unix.ETHTOOL_FLAG_STATS)
	if err != nil {
		return FEC{}, err
	}
	return parseFEC(b)
}

// get sends a GET request for one device and returns its reply.
func get(cmd uint8, ifname string, flags uint32) ([]byte, error) {
	msgs, err := request(cmd, 0, header(ifname, flags))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ifname, err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("%s: no reply to ethtool command %d", ifname, cmd)
	}
	return msgs[0], nil
}

func parseLinkModes(b []byte) (LinkModes, error) {
	attrs, err := parse(b)
	if err != nil {
		return LinkModes{}, err
	}
	var m LinkModes
	for _, a := range attrs {
		switch {
		case a.typ == unix.ETHTOOL_A_LINKMODES_SPEED && len(a.value) == 4:
			if s := nl.NativeEndian().Uint32(a.value); s != 0 && s != 0xffffffff { // SPEED_UNKNOWN
				m.SpeedMbps = int(s)
			}
		case a.typ == unix.ETHTOOL_A_LINKMODES_DUPLEX && len(a.value) == 1:
			m.Duplex = map[byte]string{0: "half", 1: "full"}[a.value[0]] // DUPLEX_HALF, DUPLEX_FULL
		case a.typ == unix.ETHTOOL_A_LINKMODES_LANES && len(a.value) == 4:
			m.Lanes = int(nl.NativeEndian().Uint32(a.value))
		case a.typ == unix.ETHTOOL_A_LINKMODES_AUTONEG && len(a.value) == 1:
			m.Autoneg = a.value[0] == 1 // AUTONEG_ENABLE
		}
	}
	return m, nil
}

// extStates are the names of enum ethtool_link_ext_state.
var extStates = []string{"autoneg", "link-training-failure", "link-logical-mismatch", "bad-signal-integrity",
	"no-cable", "cable-issue", "eeprom-issue", "calibration-failure", "power-budget-exceeded", "overheat", "module"}

func parseLinkState(b []byte) (LinkState, error) {
	attrs, err := parse(b)
	if err != nil {
		return LinkState{}, err
	}
	var s LinkState
	for _, a := range attrs {
		switch {
		case a.typ == unix.ETHTOOL_A_LINKSTATE_LINK && len(a.value) == 1:
			s.Up = a.value[0] != 0
		case a.typ == unix.ETHTOOL_A_LINKSTATE_EXT_STATE && len(a.value) == 1:
			s.ExtState = fmt.Sprintf("%d", a.value[0])
			if int(a.value[0]) < len(extStates) {
				s.ExtState = extStates[a.value[0]]
			}
		}
	}
	return s, nil
}

func parseFEC(b []byte) (FEC, error) {
	attrs, err := parse(b)
	if err != nil {
		return FEC{}, err
	}
	var f FEC
	for _, a := range attrs {
		switch {
		case a.typ == fecActive && len(a.value) == 4:
			f.Active = map[uint32]string{unix.ETHTOOL_LINK_MODE_FEC_NONE_BIT: "off", unix.ETHTOOL_LINK_MODE_FEC_RS_BIT: "rs",
				unix.ETHTOOL_LINK_MODE_FEC_BASER_BIT: "baser", unix.ETHTOOL_LINK_MODE_FEC_LLRS_BIT: "llrs"}[nl.NativeEndian().Uint32(a.value)]
		case a.typ == fecStats:
			stats, err := parse(a.value)
			if err != nil {
				return FEC{}, err
			}
			for _, s := range stats {
				// An array of u64: the total, then one per lane.
				if len(s.value) < 8 {
					continue
				}
				switch s.typ {
				case fecStatCorrected:
					f.Corrected, f.Stats = nl.NativeEndian().Uint64(s.value), true
				case fecStatUncorr:
					f.Uncorrectable, f.Stats = nl.NativeEndian().Uint64(s.value), true
				}
			}
		}
	}
	return f, nil
}
