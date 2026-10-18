//go:build linux

// Package ethtool talks to the ethtool generic netlink family of the
// kernel (the interface of "ethtool" since Linux 5.6) with the netlink
// helpers of vishvananda/netlink, so BOOTy needs no ethtool binary.
package ethtool

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// family caches the ID of the "ethtool" generic netlink family.
var family struct {
	once sync.Once
	id   uint16
	err  error
}

func familyID() (uint16, error) {
	family.once.Do(func() {
		f, err := netlink.GenlFamilyGet(unix.ETHTOOL_GENL_NAME)
		if err != nil {
			family.err = fmt.Errorf("ethtool netlink family: %w", err)
			return
		}
		family.id = f.ID
	})
	return family.id, family.err
}

// header is the request header attribute (index 1 of every ethtool
// message) that names the device.
func header(ifname string, flags uint32) *nl.RtAttr {
	h := nl.NewRtAttr(1|unix.NLA_F_NESTED, nil)
	h.AddRtAttr(unix.ETHTOOL_A_HEADER_DEV_NAME, nl.ZeroTerminated(ifname))
	if flags != 0 {
		h.AddRtAttr(unix.ETHTOOL_A_HEADER_FLAGS, nl.Uint32Attr(flags))
	}
	return h
}

// request sends one ethtool command and returns the attributes of the
// reply messages (after the generic netlink header).
func request(cmd uint8, flags int, attrs ...*nl.RtAttr) ([][]byte, error) {
	id, err := familyID()
	if err != nil {
		return nil, err
	}
	req := nl.NewNetlinkRequest(int(id), flags)
	req.AddData(&nl.Genlmsg{Command: cmd, Version: unix.ETHTOOL_GENL_VERSION})
	for _, a := range attrs {
		req.AddData(a)
	}
	msgs, err := req.Execute(unix.NETLINK_GENERIC, 0)
	if err != nil {
		return nil, fmt.Errorf("ethtool netlink command %d: %w", cmd, err)
	}
	out := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		if len(m) >= nl.SizeofGenlmsg {
			out = append(out, m[nl.SizeofGenlmsg:])
		}
	}
	return out, nil
}

// attr is one netlink attribute with the flag bits removed from its type.
type attr struct {
	typ   uint16
	value []byte
}

// parse splits netlink attributes.
func parse(b []byte) ([]attr, error) {
	raw, err := nl.ParseRouteAttr(b)
	if err != nil {
		return nil, fmt.Errorf("parse netlink attributes: %w", err)
	}
	out := make([]attr, len(raw))
	for i, a := range raw {
		out[i] = attr{typ: a.Attr.Type & nl.NLA_TYPE_MASK, value: a.Value}
	}
	return out, nil
}

// IsNotSupported reports whether err means that the driver does not
// support the ethtool request.
func IsNotSupported(err error) bool {
	return errors.Is(err, syscall.EOPNOTSUPP)
}
