//go:build linux

package ethtool

import (
	"fmt"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// Flag is one driver private flag ("ethtool --show-priv-flags").
type Flag struct {
	Name string
	On   bool
}

// PrivateFlags returns the private flags of a NIC (ETHTOOL_MSG_PRIVFLAGS_GET).
func PrivateFlags(ifname string) ([]Flag, error) {
	msgs, err := request(unix.ETHTOOL_MSG_PRIVFLAGS_GET, 0, header(ifname, 0))
	if err != nil {
		return nil, fmt.Errorf("private flags of %s: %w", ifname, err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("private flags of %s: no reply", ifname)
	}
	attrs, err := parse(msgs[0])
	if err != nil {
		return nil, err
	}
	for _, a := range attrs {
		if a.typ == unix.ETHTOOL_A_PRIVFLAGS_FLAGS {
			return parseBitset(a.value)
		}
	}
	return nil, nil
}

// SetPrivateFlag sets one private flag of a NIC (ETHTOOL_MSG_PRIVFLAGS_SET,
// "ethtool --set-priv-flags IF NAME on|off"). The other flags stay.
func SetPrivateFlag(ifname, name string, on bool) error {
	_, err := request(unix.ETHTOOL_MSG_PRIVFLAGS_SET, unix.NLM_F_ACK, header(ifname, 0), flagBitset(name, on))
	if err != nil {
		return fmt.Errorf("set private flag %s of %s to %v: %w", name, ifname, on, err)
	}
	return nil
}

// flagBitset is a verbose bitset with one named bit and no mask: the
// kernel changes only that bit.
func flagBitset(name string, on bool) *nl.RtAttr {
	bit := nl.NewRtAttr(unix.ETHTOOL_A_BITSET_BITS_BIT|unix.NLA_F_NESTED, nil)
	bit.AddRtAttr(unix.ETHTOOL_A_BITSET_BIT_NAME, nl.ZeroTerminated(name))
	if on {
		bit.AddRtAttr(unix.ETHTOOL_A_BITSET_BIT_VALUE, nil)
	}
	bits := nl.NewRtAttr(unix.ETHTOOL_A_BITSET_BITS|unix.NLA_F_NESTED, nil)
	bits.AddChild(bit)
	set := nl.NewRtAttr(unix.ETHTOOL_A_PRIVFLAGS_FLAGS|unix.NLA_F_NESTED, nil)
	set.AddChild(bits)
	return set
}

// parseBitset reads a verbose bitset. With ETHTOOL_A_BITSET_NOMASK the
// listed bits are the set ones; else a bit is set when it has
// ETHTOOL_A_BITSET_BIT_VALUE.
func parseBitset(b []byte) ([]Flag, error) {
	attrs, err := parse(b)
	if err != nil {
		return nil, err
	}
	noMask := false
	var bits []byte
	for _, a := range attrs {
		switch a.typ {
		case unix.ETHTOOL_A_BITSET_NOMASK:
			noMask = true
		case unix.ETHTOOL_A_BITSET_BITS:
			bits = a.value
		}
	}
	list, err := parse(bits)
	if err != nil {
		return nil, err
	}
	var flags []Flag
	for _, bit := range list {
		if bit.typ != unix.ETHTOOL_A_BITSET_BITS_BIT {
			continue
		}
		fields, err := parse(bit.value)
		if err != nil {
			return nil, err
		}
		f := Flag{On: noMask}
		for _, a := range fields {
			switch a.typ {
			case unix.ETHTOOL_A_BITSET_BIT_NAME:
				f.Name = unix.ByteSliceToString(a.value)
			case unix.ETHTOOL_A_BITSET_BIT_VALUE:
				f.On = true
			}
		}
		flags = append(flags, f)
	}
	return flags, nil
}
