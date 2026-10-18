//go:build linux

package ethtool

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// bitset builds a verbose bitset reply like the kernel's
// ethnl_put_bitset32: bits with their names, VALUE on the set ones.
func bitset(noMask bool, bits ...Flag) []byte {
	list := nl.NewRtAttr(unix.ETHTOOL_A_BITSET_BITS|unix.NLA_F_NESTED, nil)
	for i, f := range bits {
		b := nl.NewRtAttr(unix.ETHTOOL_A_BITSET_BITS_BIT|unix.NLA_F_NESTED, nil)
		b.AddRtAttr(unix.ETHTOOL_A_BITSET_BIT_INDEX, nl.Uint32Attr(uint32(i)))
		b.AddRtAttr(unix.ETHTOOL_A_BITSET_BIT_NAME, nl.ZeroTerminated(f.Name))
		if f.On && !noMask {
			b.AddRtAttr(unix.ETHTOOL_A_BITSET_BIT_VALUE, nil)
		}
		list.AddChild(b)
	}
	set := nl.NewRtAttr(unix.ETHTOOL_A_PRIVFLAGS_FLAGS|unix.NLA_F_NESTED, nil)
	if noMask {
		set.AddRtAttr(unix.ETHTOOL_A_BITSET_NOMASK, nil)
	}
	set.AddRtAttr(unix.ETHTOOL_A_BITSET_SIZE, nl.Uint32Attr(uint32(len(bits))))
	set.AddChild(list)
	return set.Serialize()[unix.SizeofRtAttr:]
}

func TestParseBitset(t *testing.T) {
	want := []Flag{{"link-down-on-close", false}, {"fw-lldp-agent", true}, {"channel-inline-flow-director", false}}
	got, err := parseBitset(bitset(false, want...))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parseBitset = %+v, %v; want %+v", got, err, want)
	}
	// Without a mask the kernel lists only the set bits.
	got, err = parseBitset(bitset(true, Flag{"disable-fw-lldp", true}))
	if err != nil || !reflect.DeepEqual(got, []Flag{{"disable-fw-lldp", true}}) {
		t.Fatalf("parseBitset nomask = %+v, %v", got, err)
	}
}

// The set request names one bit, has VALUE only to turn it on and no
// NOMASK, so the kernel leaves the other flags as they are.
func TestFlagBitset(t *testing.T) {
	for _, on := range []bool{true, false} {
		b := flagBitset("fw-lldp-agent", on).Serialize()
		attrs, err := parse(b)
		if err != nil || len(attrs) != 1 || attrs[0].typ != unix.ETHTOOL_A_PRIVFLAGS_FLAGS {
			t.Fatalf("on=%v: attrs %+v, %v", on, attrs, err)
		}
		got, err := parseBitset(attrs[0].value)
		if err != nil || !reflect.DeepEqual(got, []Flag{{"fw-lldp-agent", on}}) {
			t.Fatalf("on=%v: bitset = %+v, %v", on, got, err)
		}
		inner, _ := parse(attrs[0].value)
		for _, a := range inner {
			if a.typ == unix.ETHTOOL_A_BITSET_NOMASK {
				t.Fatalf("on=%v: the request has NOMASK and would clear the other flags", on)
			}
		}
	}
}

func TestDriver(t *testing.T) {
	orig := sysClassNet
	t.Cleanup(func() { sysClassNet = orig })
	sysClassNet = t.TempDir()
	dev := filepath.Join(sysClassNet, "ens1f0", "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../bus/pci/drivers/ice", filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	if got := Driver("ens1f0"); got != "ice" {
		t.Fatalf("Driver = %q, want ice", got)
	}
	if got := Driver("veth0"); got != "" {
		t.Fatalf("Driver of a NIC without device = %q", got)
	}
}

// The kernel accepts the request: lo has no private flags, so a well
// formed request ends with EOPNOTSUPP, a malformed one with EINVAL.
func TestPrivateFlagsKernel(t *testing.T) {
	if _, err := familyID(); err != nil {
		t.Skipf("no ethtool netlink family: %v", err)
	}
	_, err := PrivateFlags("lo")
	if !IsNotSupported(err) {
		t.Fatalf("PrivateFlags(lo) = %v, want EOPNOTSUPP", err)
	}
}
