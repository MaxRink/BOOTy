//go:build linux

package ethtool

import (
	"testing"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// reply serializes attributes like a kernel reply after the genl header.
func reply(attrs ...*nl.RtAttr) []byte {
	var b []byte
	for _, a := range attrs {
		b = append(b, a.Serialize()...)
	}
	return b
}

func u64s(v ...uint64) []byte {
	b := make([]byte, 0, 8*len(v))
	for _, x := range v {
		b = append(b, nl.Uint64Attr(x)...)
	}
	return b
}

func TestParseLinkModes(t *testing.T) {
	m, err := parseLinkModes(reply(
		nl.NewRtAttr(unix.ETHTOOL_A_LINKMODES_AUTONEG, []byte{1}),
		nl.NewRtAttr(unix.ETHTOOL_A_LINKMODES_SPEED, nl.Uint32Attr(25000)),
		nl.NewRtAttr(unix.ETHTOOL_A_LINKMODES_DUPLEX, []byte{1}),
		nl.NewRtAttr(unix.ETHTOOL_A_LINKMODES_LANES, nl.Uint32Attr(1)),
	))
	if err != nil || m != (LinkModes{SpeedMbps: 25000, Duplex: "full", Autoneg: true, Lanes: 1}) {
		t.Fatalf("link modes = %+v, %v", m, err)
	}
	// No carrier: SPEED_UNKNOWN and DUPLEX_UNKNOWN.
	m, err = parseLinkModes(reply(
		nl.NewRtAttr(unix.ETHTOOL_A_LINKMODES_SPEED, nl.Uint32Attr(0xffffffff)),
		nl.NewRtAttr(unix.ETHTOOL_A_LINKMODES_DUPLEX, []byte{0xff}),
	))
	if err != nil || m != (LinkModes{}) {
		t.Fatalf("unknown link modes = %+v, %v", m, err)
	}
}

func TestParseLinkState(t *testing.T) {
	s, err := parseLinkState(reply(
		nl.NewRtAttr(unix.ETHTOOL_A_LINKSTATE_LINK, []byte{0}),
		nl.NewRtAttr(unix.ETHTOOL_A_LINKSTATE_EXT_STATE, []byte{4}),
	))
	if err != nil || s != (LinkState{ExtState: "no-cable"}) {
		t.Fatalf("link state = %+v, %v", s, err)
	}
}

// The FEC counters are arrays: the total, then one value per lane.
func TestParseFEC(t *testing.T) {
	stats := nl.NewRtAttr(fecStats|unix.NLA_F_NESTED, nil)
	stats.AddRtAttr(fecStatCorrected, u64s(1200, 1000, 200))
	stats.AddRtAttr(fecStatUncorr, u64s(3, 3, 0))
	f, err := parseFEC(reply(nl.NewRtAttr(fecActive, nl.Uint32Attr(unix.ETHTOOL_LINK_MODE_FEC_RS_BIT)), stats))
	if err != nil || f != (FEC{Active: "rs", Corrected: 1200, Uncorrectable: 3, Stats: true}) {
		t.Fatalf("FEC = %+v, %v", f, err)
	}
	f, err = parseFEC(reply(nl.NewRtAttr(fecActive, nl.Uint32Attr(unix.ETHTOOL_LINK_MODE_FEC_NONE_BIT))))
	if err != nil || f != (FEC{Active: "off"}) {
		t.Fatalf("FEC off = %+v, %v", f, err)
	}
}

func TestModuleDataOf(t *testing.T) {
	data := []byte{0x11, 0x00, 0x04}
	got, err := moduleDataOf(reply(nl.NewRtAttr(moduleData, data)))
	if err != nil || string(got) != string(data) {
		t.Fatalf("data = %x, %v", got, err)
	}
	if _, err := moduleDataOf(reply(nl.NewRtAttr(moduleLength, nl.Uint32Attr(3)))); err == nil {
		t.Fatal("reply without data accepted")
	}
}
