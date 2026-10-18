//go:build linux

package gobgp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"syscall"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// This file holds what an FRR L3VNI host needs besides the bridge and the
// VXLAN device: its addresses on lo, the static blackhole routes, the
// Type-5 origination of its "network" prefixes, and the kernel messages
// for IPv6 routes over an IPv4 VTEP.

// addLoopbackAddrs adds the netplan addresses of lo.
func (o *OverlayTier) addLoopbackAddrs() error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find loopback: %w", err)
	}
	for _, a := range o.cfg.LoopbackAddrs {
		addr, err := netlink.ParseAddr(a)
		if err != nil {
			return fmt.Errorf("parse lo address %s: %w", a, err)
		}
		if err := netlink.AddrAdd(lo, addr); err != nil {
			if errors.Is(err, syscall.EEXIST) {
				continue
			}
			return fmt.Errorf("add lo address %s: %w", a, err)
		}
		o.addedLoopbackAddrs = append(o.addedLoopbackAddrs, addr)
		o.log.Info("added lo address", "ip", a)
	}
	return nil
}

// zebraRouteMetric is the metric zebra gives every route it installs.
const zebraRouteMetric = 20

// rtprotZebraStatic is the kernel protocol of FRR static routes (zebra
// RTPROT_ZSTATIC).
const rtprotZebraStatic = 196

// installBlackholes installs the FRR static blackhole routes in the main
// table, like zebra: protocol 196, metric 20.
func (o *OverlayTier) installBlackholes() error {
	for _, p := range o.cfg.BlackholeRoutes {
		_, dst, err := net.ParseCIDR(p)
		if err != nil {
			return fmt.Errorf("parse blackhole route %s: %w", p, err)
		}
		route := &netlink.Route{Dst: dst, Type: unix.RTN_BLACKHOLE, Protocol: rtprotZebraStatic, Priority: zebraRouteMetric}
		if err := o.netlinkOps.RouteReplace(route); err != nil {
			return fmt.Errorf("install blackhole route %s: %w", p, err)
		}
		o.blackholes = append(o.blackholes, route)
		o.log.Info("installed blackhole route", "dst", p)
	}
	return nil
}

func (o *OverlayTier) removeLocalState() {
	if len(o.addedLoopbackAddrs) > 0 {
		if lo, err := netlink.LinkByName("lo"); err == nil {
			for _, addr := range o.addedLoopbackAddrs {
				if err := netlink.AddrDel(lo, addr); err != nil {
					o.log.Warn("failed to remove lo address", "ip", addr, "error", err)
				}
			}
		}
		o.addedLoopbackAddrs = nil
	}
	for _, route := range o.blackholes {
		if err := o.netlinkOps.RouteDel(route); err != nil {
			o.log.Warn("failed to remove blackhole route", "dst", route.Dst, "error", err)
		}
	}
	o.blackholes = nil
}

// isBlackhole reports whether prefix is a local static blackhole route. A
// received route for it is not installed: zebra prefers the static route
// (distance 1) over BGP.
func (o *OverlayTier) isBlackhole(prefix string) bool {
	return slices.ContainsFunc(o.cfg.BlackholeRoutes, func(p string) bool {
		pp, err := netip.ParsePrefix(p)
		return err == nil && pp.Masked().String() == prefix
	})
}

// localPrefix reports whether a route in the local RIB covers prefix: the
// connected prefix of a lo or provision bridge address, or a blackhole
// route, equal to or less specific than prefix. FRR advertises a "network"
// only then (import check, doc/user/bgp.rst "bgp network import-check";
// an exact match is needed only with "import-check exact").
func (o *OverlayTier) localPrefix(prefix netip.Prefix) bool {
	for _, p := range slices.Concat(o.cfg.LoopbackAddrs, o.cfg.BlackholeRoutes, []string{o.cfg.ProvisionIP}) {
		if pp, err := netip.ParsePrefix(p); err == nil && pp.Bits() <= prefix.Bits() && pp.Masked().Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

// advertiseType5Networks originates the FRR "network" prefixes of the L3VNI
// VRF as EVPN Type-5 routes, like "advertise ipv4|ipv6 unicast".
func (o *OverlayTier) advertiseType5Networks(ctx context.Context) error {
	vni := uint32(o.cfg.ProvisionVNI)
	for _, n := range o.cfg.Type5Networks {
		prefix, err := netip.ParsePrefix(n)
		if err != nil {
			return fmt.Errorf("parse network %s: %w", n, err)
		}
		prefix = prefix.Masked()
		if !o.localPrefix(prefix) {
			o.log.Warn("network is not in the local RIB, not advertised (import check)", "prefix", prefix)
			continue
		}
		rd := buildRouteDistinguisher(o.cfg.ASN, vni)
		gw := type5DirectGateway
		if prefix.Addr().Is6() {
			gw = "::"
		}
		nlri, err := buildEVPNType5NLRI(rd, prefix.String(), gw, vni)
		if err != nil {
			return fmt.Errorf("build EVPN NLRI for %s: %w", prefix, err)
		}
		pattrs, err := buildType5PathAttrsRTs(nlri, o.cfg.RouterID, o.cfg.ASN, vni, o.exportRTSpecs(), o.cfg.BridgeMAC)
		if err != nil {
			return fmt.Errorf("build path attributes for %s: %w", prefix, err)
		}
		if err := addPath(o.bgp, &apipb.Path{
			Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
			Nlri:   nlri,
			Pattrs: pattrs,
		}); err != nil {
			return fmt.Errorf("add EVPN Type-5 path for %s: %w", prefix, err)
		}
		o.log.Info("advertised EVPN type-5 network", "prefix", prefix, "vni", vni)
	}
	return nil
}

// type5Gateway returns the kernel gateway of a Type-5 route: IPv4 for an
// IPv4 prefix, and for an IPv6 prefix over an IPv4 VTEP the IPv4-mapped
// address ::ffff:<VTEP>, like zebra (FRR bgp_evpn_rt5 topotest).
func type5Gateway(dst *net.IPNet, gw net.IP) net.IP {
	if dst.IP.To4() != nil {
		return gw.To4()
	}
	return gw.To16()
}

// gatewayKey names a Type-5 gateway. An IPv4-mapped gateway of IPv6 routes
// is "::ffff:a.b.c.d", so it does not share state with the IPv4 gateway.
func gatewayKey(gw net.IP) string {
	if a, ok := netip.AddrFromSlice(gw); ok {
		return a.String()
	}
	return ""
}

func parseGatewayKey(key string) net.IP {
	a, err := netip.ParseAddr(key)
	if err != nil {
		return nil
	}
	return net.IP(a.AsSlice())
}

// isMappedGateway reports an IPv4-mapped gateway: 16 bytes with the
// ::ffff:0:0/96 prefix.
func isMappedGateway(ip net.IP) bool {
	return len(ip) == net.IPv6len && ip.To4() != nil
}

// The netlink package sends an IPv4-mapped address as an IPv4 address, so
// the kernel rejects it in an IPv6 route or neighbor. These requests send
// the 16 bytes.

func mappedGatewayRoute(route *netlink.Route, del bool) error {
	req := nl.NewNetlinkRequest(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE|unix.NLM_F_ACK)
	msg := nl.NewRtMsg()
	if del {
		req = nl.NewNetlinkRequest(unix.RTM_DELROUTE, unix.NLM_F_ACK)
		msg = nl.NewRtDelMsg()
	}
	ones, _ := route.Dst.Mask.Size()
	msg.Family = unix.AF_INET6
	msg.Dst_len = uint8(ones)
	msg.Flags = uint32(route.Flags)
	if !del {
		msg.Protocol = uint8(route.Protocol)
	}
	table := uint32(unix.RT_TABLE_MAIN)
	if route.Table > 0 {
		table = uint32(route.Table)
	}
	msg.Table = unix.RT_TABLE_UNSPEC
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.RTA_DST, route.Dst.IP.To16()))
	req.AddData(nl.NewRtAttr(unix.RTA_GATEWAY, route.Gw.To16()))
	req.AddData(nl.NewRtAttr(unix.RTA_OIF, nl.Uint32Attr(uint32(route.LinkIndex))))
	req.AddData(nl.NewRtAttr(unix.RTA_TABLE, nl.Uint32Attr(table)))
	if route.Priority > 0 {
		req.AddData(nl.NewRtAttr(unix.RTA_PRIORITY, nl.Uint32Attr(uint32(route.Priority))))
	}
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("route %s via %s: %w", route.Dst, gatewayKey(route.Gw), err)
	}
	return nil
}

func mappedGatewayNeigh(n *netlink.Neigh, del bool) error {
	req := nl.NewNetlinkRequest(unix.RTM_NEWNEIGH, unix.NLM_F_CREATE|unix.NLM_F_REPLACE|unix.NLM_F_ACK)
	if del {
		req = nl.NewNetlinkRequest(unix.RTM_DELNEIGH, unix.NLM_F_ACK)
	}
	req.AddData(&netlink.Ndmsg{
		Family: unix.AF_INET6,
		Index:  uint32(n.LinkIndex),
		State:  uint16(n.State),
		Flags:  uint8(n.Flags),
		Type:   uint8(n.Type),
	})
	req.AddData(nl.NewRtAttr(unix.NDA_DST, n.IP.To16()))
	if len(n.HardwareAddr) > 0 {
		req.AddData(nl.NewRtAttr(unix.NDA_LLADDR, n.HardwareAddr))
	}
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("neighbor %s: %w", gatewayKey(n.IP), err)
	}
	return nil
}
