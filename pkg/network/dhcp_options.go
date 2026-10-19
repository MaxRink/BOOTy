//go:build linux

package network

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net"
	"slices"
	"strings"
	"sync"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/telekom/BOOTy/pkg/network/dhcp4"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The DHCPv4 options of systemd-networkd (systemd 255) with the legacy
// ramdisk's netplan, as captured in kindmetal (CAPRF !309, job 407787205).

// networkdParams is the parameter request list (option 55) of networkd:
// subnet mask, router, DNS, host name, domain name, MTU, static routes,
// NTP, captive portal, domain search, SIP servers, classless routes.
var networkdParams = []dhcpv4.OptionCode{
	dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter, dhcpv4.OptionDomainNameServer, dhcpv4.OptionHostName,
	dhcpv4.OptionDomainName, dhcpv4.OptionInterfaceMTU, dhcpv4.OptionStaticRoutingTable, dhcpv4.OptionNTPServers,
	dhcpv4.OptionURL, dhcpv4.OptionDNSDomainSearchList, dhcpv4.OptionSIPServers, dhcpv4.OptionClasslessStaticRoute,
}

// systemdEnterprise is the IANA enterprise number of systemd in its
// DUID-EN (sd-dhcp-duid.h).
const systemdEnterprise = 43793

// dhcpClientID is the client identifier (option 61) of networkd: type 255,
// a 4 byte IAID of the interface and a DUID-EN (type 2) with the systemd
// enterprise number and 8 bytes derived from the machine ID (RFC 4361).
// The machine ID of the ramdisk is new at every boot, as in the legacy
// ramdisk, so the identifier is too; the hash differs from networkd's
// siphash, its form does not.
func dhcpClientID(ifname, machineID string) []byte {
	if machineID == "" {
		machineID = fallbackMachineID()
	}
	iaid := sha256.Sum256([]byte(ifname))
	id := sha256.Sum256([]byte(machineID))
	b := make([]byte, 0, 19)
	b = append(b, 255)
	b = append(b, iaid[:4]...)
	b = binary.BigEndian.AppendUint16(b, 2)
	b = binary.BigEndian.AppendUint32(b, systemdEnterprise)
	return append(b, id[:8]...)
}

// fallbackMachineID is a random ID for the run when the ramdisk has no
// machine ID, so two hosts do not send the same client identifier.
var fallbackMachineID = sync.OnceValue(func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
})

// dhcpRoute is one route of a lease: a destination and a gateway, or no
// gateway for an on-link destination.
type dhcpRoute struct {
	dst *net.IPNet
	gw  net.IP
}

// staticRoutes parses option 33 (RFC 2132 5.8): pairs of a destination,
// with its classful mask, and a router. 0.0.0.0 and class D and E are no
// valid destinations; an option whose length is not a multiple of 8 is
// ignored as a whole, like networkd.
func staticRoutes(data []byte) []dhcpRoute {
	var ips dhcpv4.IPs
	if ips.FromBytes(data) != nil || len(ips)%2 != 0 {
		return nil
	}
	var out []dhcpRoute
	for ; len(ips) > 0; ips = ips[2:] {
		dst := ips[0]
		if dst.IsUnspecified() || dst[0] >= 224 {
			continue // no classful mask for class D and E
		}
		mask := dst.DefaultMask()
		out = append(out, dhcpRoute{dst: &net.IPNet{IP: dst.Mask(mask), Mask: mask}, gw: ips[1]})
	}
	return out
}

// classlessRoutes decodes option 121 (RFC 3442), with the destinations
// masked. A malformed option is ignored as a whole, like networkd does.
func classlessRoutes(data []byte) ([]dhcpRoute, bool) {
	var routes dhcpv4.Routes
	if routes.FromBytes(data) != nil {
		return nil, false
	}
	out := make([]dhcpRoute, 0, len(routes))
	for _, r := range routes {
		out = append(out, dhcpRoute{dst: &net.IPNet{IP: r.Dest.IP.Mask(r.Dest.Mask), Mask: r.Dest.Mask}, gw: r.Router})
	}
	return out, len(out) > 0
}

// domainSearch decodes option 119 (dhcp4.SearchDomains).
var domainSearch = dhcp4.SearchDomains

// leaseRouteSet returns the routes of a lease: option 121, or without it
// option 33 and a default route through the first router that is not
// 0.0.0.0 (none when all are), like networkd-dhcp4.c.
func leaseRouteSet(lease *dhcp4.Lease) []dhcpRoute {
	if routes, ok := classlessRoutes(lease.Options.Get(dhcpv4.OptionClasslessStaticRoute)); ok {
		return routes
	}
	routes := staticRoutes(lease.Options.Get(dhcpv4.OptionStaticRoutingTable))
	if i := slices.IndexFunc(lease.Router, func(r net.IP) bool { return r.To4() != nil && !r.IsUnspecified() }); i >= 0 {
		routes = append(routes, dhcpRoute{dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, gw: lease.Router[i]})
	}
	return routes
}

// leaseRoutes returns the routes of a lease like networkd (leaseRouteSet),
// with first a host route to every gateway. They have metric 100, proto
// dhcp and the lease address as source, except a route to localhost.
func leaseRoutes(link netlink.Link, addr *netlink.Addr, lease *dhcp4.Lease) []*netlink.Route {
	routes := leaseRouteSet(lease)
	route := func(dst *net.IPNet, gw net.IP) *netlink.Route {
		r := &netlink.Route{LinkIndex: link.Attrs().Index, Src: addr.IP, Priority: dhcpRouteMetric, Protocol: unix.RTPROT_DHCP}
		if ones, _ := dst.Mask.Size(); ones > 0 {
			r.Dst = dst
		}
		switch {
		case dst.IP.IsLoopback():
			// localhost: a host route without source, like networkd.
			r.Scope, r.Src = netlink.SCOPE_HOST, nil
		case dst.IP.Equal(addr.IP):
			r.Scope = netlink.SCOPE_HOST // the lease address: a host route
		case gw == nil || gw.IsUnspecified():
			r.Scope = netlink.SCOPE_LINK
		default:
			r.Gw = gw
		}
		return r
	}
	out := make([]*netlink.Route, 0, len(routes))
	var gws []*netlink.Route
	for _, r := range routes {
		host := r.dst.IP.IsLoopback() || r.dst.IP.Equal(addr.IP)
		if !host && r.gw != nil && !r.gw.IsUnspecified() && !containsGateway(gws, r.gw) {
			gws = append(gws, route(&net.IPNet{IP: r.gw, Mask: net.CIDRMask(32, 32)}, nil))
		}
		out = append(out, route(r.dst, r.gw))
	}
	return append(gws, out...)
}

func containsGateway(routes []*netlink.Route, gw net.IP) bool {
	for _, r := range routes {
		if r.Dst != nil && r.Dst.IP.Equal(gw) {
			return true
		}
	}
	return false
}

// sameRoute compares the keys of two lease routes.
func sameRoute(a, b *netlink.Route) bool {
	dst := func(r *netlink.Route) string {
		if r.Dst == nil {
			return "default"
		}
		return r.Dst.String()
	}
	return dst(a) == dst(b) && a.Gw.Equal(b.Gw) && a.Src.Equal(b.Src) && a.Scope == b.Scope && a.Table == b.Table
}

// Source routing: every NIC's lease routes are also in a table of its
// own, and a rule sends what comes from the lease address there. A
// connection bound to the address of one NIC (the image stream) then
// leaves through that NIC, also when the kernel picks the default route of
// another NIC for new lookups (after a carrier flap). networkd does not do
// this; it fixes the stall of the legacy ramdisk after a link flap.
const (
	dhcpTableBase    = 10000 // table dhcpTableBase+ifindex
	dhcpRulePriority = 10000 // before the main table (32766)
)

// dhcpSourceRouting turns source routing on; tests of the main table
// routes turn it off.
var dhcpSourceRouting = true

// sourceTable returns the table of a NIC.
func sourceTable(link netlink.Link) int { return dhcpTableBase + link.Attrs().Index }

// sourceRoutes returns the copies of the main table routes of a lease in
// the table of the NIC, with the subnet route of the lease address first.
// Host routes (scope host) stay in the main table only.
func sourceRoutes(main []*netlink.Route, link netlink.Link, addr *netlink.Addr) []*netlink.Route {
	table := sourceTable(link)
	subnet := &net.IPNet{IP: addr.IP.Mask(addr.Mask), Mask: addr.Mask}
	out := make([]*netlink.Route, 0, len(main)+1)
	if ones, bits := addr.Mask.Size(); ones < bits {
		out = append(out, &netlink.Route{LinkIndex: link.Attrs().Index, Dst: subnet, Src: addr.IP, Scope: netlink.SCOPE_LINK,
			Priority: dhcpRouteMetric, Protocol: unix.RTPROT_DHCP, Table: table})
	}
	for _, r := range main {
		if r.Scope == netlink.SCOPE_HOST {
			continue
		}
		c := *r
		c.Table = table
		out = append(out, &c)
	}
	return out
}

// sourceRule is the rule for the lease address of a NIC.
func sourceRule(link netlink.Link, addr *netlink.Addr) *netlink.Rule {
	r := netlink.NewRule()
	r.Src = &net.IPNet{IP: addr.IP, Mask: net.CIDRMask(32, 32)}
	r.Table = sourceTable(link)
	r.Priority = dhcpRulePriority
	return r
}

// leaseSearchDomains returns the domain (option 15) and the domain search
// list (option 119) of a lease, each once.
func leaseSearchDomains(lease *dhcp4.Lease) []string {
	var out []string
	add := func(d string) {
		d = strings.TrimSuffix(strings.TrimSpace(d), ".")
		if d != "" && validSearchDomain(d) && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	add(leaseDomain(lease.DomainName))
	for _, d := range domainSearch(lease.Options.Get(dhcpv4.OptionDNSDomainSearchList)) {
		add(d)
	}
	return out
}
