//go:build linux

package sriov

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// genlMsg serializes a devlink answer like the kernel sends it: the genl
// header, then the attributes. The fixtures below follow the attribute
// layout of devlink_nl_eswitch_fill and devlink_nl_info_fill.
func genlMsg(cmd uint8, attrs ...*nl.RtAttr) []byte {
	b := (&nl.Genlmsg{Command: cmd, Version: nl.GENL_DEVLINK_VERSION}).Serialize()
	for _, a := range attrs {
		b = append(b, a.Serialize()...)
	}
	return b
}

func handleAttrs(bus, dev string) []*nl.RtAttr {
	return []*nl.RtAttr{
		nl.NewRtAttr(nl.DEVLINK_ATTR_BUS_NAME, nl.ZeroTerminated(bus)),
		nl.NewRtAttr(nl.DEVLINK_ATTR_DEV_NAME, nl.ZeroTerminated(dev)),
	}
}

func version(kind int, name, value string) *nl.RtAttr {
	v := nl.NewRtAttr(kind, nil)
	v.AddRtAttr(nl.DEVLINK_ATTR_INFO_VERSION_NAME, nl.ZeroTerminated(name))
	v.AddRtAttr(nl.DEVLINK_ATTR_INFO_VERSION_VALUE, nl.ZeroTerminated(value))
	return v
}

// mlx5EswitchAnswer is "devlink dev eswitch show pci/0000:3b:00.0" of a
// ConnectX-6 Dx in switchdev mode: mode switchdev inline-mode none
// encap-mode basic.
func mlx5EswitchAnswer(mode uint16) []byte {
	attrs := handleAttrs("pci", "0000:3b:00.0")
	attrs = append(attrs,
		nl.NewRtAttr(nl.DEVLINK_ATTR_ESWITCH_MODE, nl.Uint16Attr(mode)),
		nl.NewRtAttr(nl.DEVLINK_ATTR_ESWITCH_INLINE_MODE, []byte{nl.DEVLINK_ESWITCH_INLINE_MODE_NONE}),
		nl.NewRtAttr(nl.DEVLINK_ATTR_ESWITCH_ENCAP_MODE, []byte{nl.DEVLINK_ESWITCH_ENCAP_MODE_BASIC}))
	return genlMsg(nl.DEVLINK_CMD_ESWITCH_GET, attrs...)
}

func TestParseEswitch(t *testing.T) {
	for mode, want := range map[uint16]string{
		nl.DEVLINK_ESWITCH_MODE_LEGACY:    EswitchLegacy,
		nl.DEVLINK_ESWITCH_MODE_SWITCHDEV: EswitchSwitchdev,
		7:                                 "mode-7",
	} {
		e, err := parseEswitch(mlx5EswitchAnswer(mode))
		if err != nil {
			t.Fatal(err)
		}
		if e.Capability != CapSupported || e.Mode != want || e.InlineMode != "none" || e.EncapMode != "basic" {
			t.Errorf("mode %d: %+v", mode, e)
		}
	}
	// ice in legacy mode sends only the mode.
	e, err := parseEswitch(genlMsg(nl.DEVLINK_CMD_ESWITCH_GET, append(handleAttrs("pci", "0000:5e:00.0"),
		nl.NewRtAttr(nl.DEVLINK_ATTR_ESWITCH_MODE, nl.Uint16Attr(nl.DEVLINK_ESWITCH_MODE_LEGACY)))...))
	if err != nil || e.Mode != EswitchLegacy || e.InlineMode != "" || e.EncapMode != "" {
		t.Fatalf("ice legacy = %+v, %v", e, err)
	}
}

func TestParseEswitchRejectsBrokenAnswers(t *testing.T) {
	for name, msg := range map[string][]byte{
		"short":     {1, 2},
		"no mode":   genlMsg(nl.DEVLINK_CMD_ESWITCH_GET, handleAttrs("pci", "0000:3b:00.0")...),
		"truncated": append(genlMsg(nl.DEVLINK_CMD_ESWITCH_GET), 8, 0, 25, 0, 1),
	} {
		if e, err := parseEswitch(msg); err == nil {
			t.Errorf("%s: parsed %+v without error", name, e)
		}
	}
}

func TestParseInfoKeepsRunningAndStoredApart(t *testing.T) {
	attrs := handleAttrs("pci", "0000:3b:00.0")
	attrs = append(attrs,
		nl.NewRtAttr(nl.DEVLINK_ATTR_INFO_DRIVER_NAME, nl.ZeroTerminated("mlx5_core")),
		nl.NewRtAttr(nl.DEVLINK_ATTR_INFO_SERIAL_NUMBER, nl.ZeroTerminated("MT2116X00001")),
		version(nl.DEVLINK_ATTR_INFO_VERSION_FIXED, "fw.psid", "MT_0000000359"),
		version(nl.DEVLINK_ATTR_INFO_VERSION_RUNNING, "fw.version", "22.39.1002"),
		version(nl.DEVLINK_ATTR_INFO_VERSION_RUNNING, "fw", "22.39.1002"),
		version(nl.DEVLINK_ATTR_INFO_VERSION_STORED, "fw.version", "22.41.1000"),
		version(nl.DEVLINK_ATTR_INFO_VERSION_STORED, "fw", "22.41.1000"),
	)
	info, err := parseInfo(genlMsg(nl.DEVLINK_CMD_INFO_GET, attrs...))
	if err != nil {
		t.Fatal(err)
	}
	if info.Driver != "mlx5_core" || info.SerialNumber != "MT2116X00001" || len(info.Fixed) != 1 ||
		len(info.Running) != 2 || len(info.Stored) != 2 || info.Stored[0].Value != "22.41.1000" {
		t.Fatalf("info = %+v", info)
	}
	if !info.PendingReset() {
		t.Fatal("stored 22.41.1000 != running 22.39.1002 must be a pending reset")
	}
	if got := runningFirmware(info); got != "22.39.1002" {
		t.Fatalf("runningFirmware = %q", got)
	}
}

func TestParseInfoSkipsMalformedVersions(t *testing.T) {
	noName := nl.NewRtAttr(nl.DEVLINK_ATTR_INFO_VERSION_RUNNING, nil)
	noName.AddRtAttr(nl.DEVLINK_ATTR_INFO_VERSION_VALUE, nl.ZeroTerminated("1.0"))
	info, err := parseInfo(genlMsg(nl.DEVLINK_CMD_INFO_GET,
		noName,
		nl.NewRtAttr(nl.DEVLINK_ATTR_INFO_VERSION_RUNNING, []byte{0xff, 0xff, 0xff}),
		version(nl.DEVLINK_ATTR_INFO_VERSION_RUNNING, "fw.mgmt", "9.20")))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Running) != 1 || info.Running[0].Name != "fw.mgmt" {
		t.Fatalf("running = %+v", info.Running)
	}
	if info.PendingReset() || (*DevlinkInfo)(nil).PendingReset() {
		t.Fatal("no stored versions: no pending reset")
	}
}

func TestDevlinkErrorMapsNotSupported(t *testing.T) {
	for _, errno := range []error{unix.EOPNOTSUPP, unix.ENODEV, unix.ENOENT} {
		if err := devlinkError(1, "pci", "0000:3b:00.0", errno); !errors.Is(err, ErrNotSupported) || !errors.Is(err, errno) {
			t.Errorf("%v: %v does not wrap ErrNotSupported and the errno", errno, err)
		}
	}
	if err := devlinkError(1, "pci", "0000:3b:00.0", fmt.Errorf("x: %w", unix.EBUSY)); errors.Is(err, ErrNotSupported) {
		t.Errorf("EBUSY maps to ErrNotSupported: %v", err)
	}
}

func TestModeNames(t *testing.T) {
	if inlineModeName(nl.DEVLINK_ESWITCH_INLINE_MODE_LINK) != "link" ||
		inlineModeName(nl.DEVLINK_ESWITCH_INLINE_MODE_NETWORK) != "network" ||
		inlineModeName(nl.DEVLINK_ESWITCH_INLINE_MODE_TRANSPORT) != "transport" ||
		inlineModeName(9) != "inline-9" || encapModeName(nl.DEVLINK_ESWITCH_ENCAP_MODE_NONE) != "none" ||
		encapModeName(5) != "encap-5" {
		t.Fatal("mode names")
	}
}

func TestParseHeaderOnlyAnswers(t *testing.T) {
	// A genl header without attributes is a valid, empty answer.
	info, err := parseInfo(genlMsg(nl.DEVLINK_CMD_INFO_GET))
	if err != nil || info.Driver != "" || len(info.Running) != 0 {
		t.Fatalf("header-only info = %+v, %v", info, err)
	}
	if _, err := parseEswitch(genlMsg(nl.DEVLINK_CMD_ESWITCH_GET)); err == nil || !strings.Contains(err.Error(), "without a mode") {
		t.Fatalf("header-only eswitch: %v", err)
	}
}

func TestRunningFirmwarePrefersGenericName(t *testing.T) {
	info := &DevlinkInfo{Running: []Version{{"fw.mgmt", "9.20"}, {"fw.psid", "MT_1"}, {"fw", "22.39"}}}
	if got := runningFirmware(info); got != "22.39" {
		t.Fatalf("runningFirmware = %q, want the generic fw", got)
	}
	if got := runningFirmware(&DevlinkInfo{Running: []Version{{"fw.psid", "MT_1"}, {"fw.mgmt", "9.20"}}}); got != "9.20" {
		t.Fatalf("runningFirmware = %q, want fw.mgmt", got)
	}
	if got := runningFirmware(&DevlinkInfo{}); got != "" {
		t.Fatalf("runningFirmware = %q", got)
	}
}
