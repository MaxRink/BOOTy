//go:build linux

package gobgp

import (
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// This file applies the netplan device settings (systemd-networkd
// semantics) of the provision VXLAN device and bridge, the NIC accept-ra
// settings and the forwarding sysctls.

// addrGenModeNone is IN6_ADDR_GEN_MODE_NONE: no IPv6 link-local address,
// like networkd LinkLocalAddressing=no (netplan "link-local: []").
const addrGenModeNone = 1

// vxlanDeviceMTU is the netplan tunnel MTU, else the NIC MTU minus the
// VXLAN overhead.
func (o *OverlayTier) vxlanDeviceMTU() int {
	if o.cfg.OverlayDevices.VXLANMTU > 0 {
		return o.cfg.OverlayDevices.VXLANMTU
	}
	return o.vxlanMTU()
}

// applyVXLANSettings sets the netplan MAC, learning and ARP proxy of a
// VXLAN device that is not created yet.
func (o *OverlayTier) applyVXLANSettings(v *netlink.Vxlan) error {
	d := &o.cfg.OverlayDevices
	if d.VXLANMAC != "" {
		mac, err := net.ParseMAC(d.VXLANMAC)
		if err != nil {
			return fmt.Errorf("parse VXLAN MAC %s: %w", d.VXLANMAC, err)
		}
		v.HardwareAddr = mac
	}
	v.Learning = boolValue(d.VXLANLearning)
	v.Proxy = boolValue(d.VXLANARPProxy)
	return nil
}

// applyLinkSettings sets the IPv6 link-local mode and accept_ra of a link
// before it goes up.
func applyLinkSettings(link netlink.Link, linkLocal, acceptRA *bool) error {
	if linkLocal != nil && !*linkLocal {
		if err := netlink.LinkSetIP6AddrGenMode(link, addrGenModeNone); err != nil {
			return fmt.Errorf("disable IPv6 link-local on %s: %w", link.Attrs().Name, err)
		}
	}
	if acceptRA != nil {
		return setAcceptRA(link.Attrs().Name, *acceptRA)
	}
	return nil
}

// applyPortSettings sets the netplan hairpin and port-mac-learning of the
// VXLAN device as bridge port.
func (o *OverlayTier) applyPortSettings(port netlink.Link) error {
	d := &o.cfg.OverlayDevices
	if d.PortHairpin != nil {
		if err := netlink.LinkSetHairpin(port, *d.PortHairpin); err != nil {
			return fmt.Errorf("set hairpin on %s: %w", port.Attrs().Name, err)
		}
	}
	if d.PortLearning != nil {
		if err := netlink.LinkSetLearning(port, *d.PortLearning); err != nil {
			return fmt.Errorf("set learning on %s: %w", port.Attrs().Name, err)
		}
	}
	return nil
}

// applyBridgeSettings sets the netplan MTU and STP of the bridge.
func (o *OverlayTier) applyBridgeSettings(br netlink.Link) error {
	if mtu := o.cfg.OverlayDevices.BridgeMTU; mtu > 0 {
		if err := netlink.LinkSetMTU(br, mtu); err != nil {
			return fmt.Errorf("set bridge MTU: %w", err)
		}
	}
	if stp := o.cfg.OverlayDevices.BridgeSTP; stp != nil {
		return setBridgeSTP(br, *stp)
	}
	return nil
}

// setBridgeSTP sets the spanning tree of a bridge (IFLA_BR_STP_STATE);
// netlink.Bridge has no field for it.
func setBridgeSTP(link netlink.Link, on bool) error {
	state := uint32(0)
	if on {
		state = 1
	}
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(link.Attrs().Index) //nolint:gosec // an interface index
	req.AddData(msg)
	info := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	info.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated("bridge"))
	data := info.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	data.AddRtAttr(nl.IFLA_BR_STP_STATE, nl.Uint32Attr(state))
	req.AddData(info)
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("set STP on %s: %w", link.Attrs().Name, err)
	}
	return nil
}

func setAcceptRA(dev string, on bool) error {
	val := "0"
	if on {
		val = "1"
	}
	p := procSysNet + "/ipv6/conf/" + dev + "/accept_ra"
	if err := os.WriteFile(p, []byte(val), 0o644); err != nil { //nolint:gosec // sysctl path
		return fmt.Errorf("set %s: %w", p, err)
	}
	return nil
}

func boolValue(b *bool) bool { return b != nil && *b }

// sysctlBool is the sysctl value of b, or of def when b is unset.
func sysctlBool(b *bool, def bool) string {
	if b == nil {
		b = &def
	}
	if *b {
		return "1"
	}
	return "0"
}
