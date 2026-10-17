//go:build linux

package sriov

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Devlink is the devlink access that the inspector needs. Tests replace it
// with a fake.
type Devlink interface {
	// Eswitch returns the eSwitch state of a device. It returns an error
	// that wraps ErrNotSupported when the driver has no eSwitch support or
	// the device no devlink instance.
	Eswitch(bus, device string) (Eswitch, error)
	// Info returns the devlink dev info of a device.
	Info(bus, device string) (*DevlinkInfo, error)
}

// NetlinkDevlink is the Devlink of the kernel, through generic netlink.
type NetlinkDevlink struct{}

// Eswitch implements Devlink. It sends DEVLINK_CMD_ESWITCH_GET itself:
// netlink.DevLinkGetDeviceByName drops the error, so it cannot tell a
// driver without eSwitch support from a failed query.
func (NetlinkDevlink) Eswitch(bus, device string) (Eswitch, error) {
	msgs, err := devlinkRequest(nl.DEVLINK_CMD_ESWITCH_GET, bus, device)
	if err != nil {
		return Eswitch{}, err
	}
	if len(msgs) == 0 {
		return Eswitch{}, errors.New("devlink eswitch get: empty answer")
	}
	return parseEswitch(msgs[0])
}

// Info implements Devlink. It keeps the running and the stored versions
// apart, which netlink.DevlinkGetDeviceInfoByNameAsMap merges.
func (NetlinkDevlink) Info(bus, device string) (*DevlinkInfo, error) {
	msgs, err := devlinkRequest(nl.DEVLINK_CMD_INFO_GET, bus, device)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, errors.New("devlink info get: empty answer")
	}
	info, err := parseInfo(msgs[0])
	if err != nil {
		return nil, err
	}
	info.Handle = bus + "/" + device
	return info, nil
}

// devlinkRequest sends one devlink command for a device and maps the
// errors that mean "not available" to ErrNotSupported.
func devlinkRequest(cmd uint8, bus, device string, attrs ...*nl.RtAttr) ([][]byte, error) {
	family, err := netlink.GenlFamilyGet(nl.GENL_DEVLINK_NAME)
	if err != nil {
		return nil, fmt.Errorf("devlink family: %w: %w", ErrNotSupported, err)
	}
	req := nl.NewNetlinkRequest(int(family.ID), unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	req.AddData(&nl.Genlmsg{Command: cmd, Version: nl.GENL_DEVLINK_VERSION})
	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_BUS_NAME, nl.ZeroTerminated(bus)))
	req.AddData(nl.NewRtAttr(nl.DEVLINK_ATTR_DEV_NAME, nl.ZeroTerminated(device)))
	for _, a := range attrs {
		req.AddData(a)
	}
	msgs, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, devlinkError(cmd, bus, device, err)
	}
	return msgs, nil
}

func devlinkError(cmd uint8, bus, device string, err error) error {
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("devlink cmd %d on %s/%s: %w: %w", cmd, bus, device, ErrNotSupported, err)
	}
	return fmt.Errorf("devlink cmd %d on %s/%s: %w", cmd, bus, device, err)
}

func genlAttrs(msg []byte) ([]syscall.NetlinkRouteAttr, error) {
	if len(msg) < nl.SizeofGenlmsg {
		return nil, errors.New("devlink answer shorter than the genl header")
	}
	attrs, err := nl.ParseRouteAttr(msg[nl.SizeofGenlmsg:])
	if err != nil {
		return nil, fmt.Errorf("parse devlink attributes: %w", err)
	}
	return attrs, nil
}

func parseEswitch(msg []byte) (Eswitch, error) {
	attrs, err := genlAttrs(msg)
	if err != nil {
		return Eswitch{}, err
	}
	e := Eswitch{Capability: CapSupported}
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.DEVLINK_ATTR_ESWITCH_MODE:
			if len(a.Value) >= 2 {
				e.Mode = eswitchModeName(native16(a.Value))
			}
		case nl.DEVLINK_ATTR_ESWITCH_INLINE_MODE:
			if len(a.Value) >= 1 {
				e.InlineMode = inlineModeName(a.Value[0])
			}
		case nl.DEVLINK_ATTR_ESWITCH_ENCAP_MODE:
			if len(a.Value) >= 1 {
				e.EncapMode = encapModeName(a.Value[0])
			}
		}
	}
	if e.Mode == "" {
		return Eswitch{}, errors.New("devlink eswitch answer without a mode")
	}
	return e, nil
}

func native16(b []byte) uint16 { return nl.NativeEndian().Uint16(b) }

func eswitchModeName(m uint16) string {
	switch m {
	case nl.DEVLINK_ESWITCH_MODE_LEGACY:
		return EswitchLegacy
	case nl.DEVLINK_ESWITCH_MODE_SWITCHDEV:
		return EswitchSwitchdev
	}
	return fmt.Sprintf("mode-%d", m)
}

func inlineModeName(m uint8) string {
	switch m {
	case nl.DEVLINK_ESWITCH_INLINE_MODE_NONE:
		return "none"
	case nl.DEVLINK_ESWITCH_INLINE_MODE_LINK:
		return "link"
	case nl.DEVLINK_ESWITCH_INLINE_MODE_NETWORK:
		return "network"
	case nl.DEVLINK_ESWITCH_INLINE_MODE_TRANSPORT:
		return "transport"
	}
	return fmt.Sprintf("inline-%d", m)
}

func encapModeName(m uint8) string {
	switch m {
	case nl.DEVLINK_ESWITCH_ENCAP_MODE_NONE:
		return "none"
	case nl.DEVLINK_ESWITCH_ENCAP_MODE_BASIC:
		return "basic"
	}
	return fmt.Sprintf("encap-%d", m)
}

func parseInfo(msg []byte) (*DevlinkInfo, error) {
	attrs, err := genlAttrs(msg)
	if err != nil {
		return nil, err
	}
	info := &DevlinkInfo{}
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.DEVLINK_ATTR_INFO_DRIVER_NAME:
			info.Driver = nl.BytesToString(a.Value)
		case nl.DEVLINK_ATTR_INFO_SERIAL_NUMBER:
			info.SerialNumber = nl.BytesToString(a.Value)
		case nl.DEVLINK_ATTR_INFO_VERSION_FIXED:
			info.Fixed = appendVersion(info.Fixed, a.Value)
		case nl.DEVLINK_ATTR_INFO_VERSION_RUNNING:
			info.Running = appendVersion(info.Running, a.Value)
		case nl.DEVLINK_ATTR_INFO_VERSION_STORED:
			info.Stored = appendVersion(info.Stored, a.Value)
		}
	}
	return info, nil
}

// appendVersion appends a nested name/value version. A malformed nest is
// skipped: one bad version must not hide the others.
func appendVersion(list []Version, nested []byte) []Version {
	attrs, err := nl.ParseRouteAttr(nested)
	if err != nil {
		return list
	}
	var v Version
	for _, a := range attrs {
		switch a.Attr.Type {
		case nl.DEVLINK_ATTR_INFO_VERSION_NAME:
			v.Name = nl.BytesToString(a.Value)
		case nl.DEVLINK_ATTR_INFO_VERSION_VALUE:
			v.Value = nl.BytesToString(a.Value)
		}
	}
	if v.Name == "" {
		return list
	}
	return append(list, v)
}
