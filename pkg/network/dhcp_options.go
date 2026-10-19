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

	"github.com/google/gopacket/layers"
	"github.com/telekom/BOOTy/pkg/network/dhcp4"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The DHCPv4 options of systemd-networkd (systemd 255) with the legacy
// ramdisk's netplan, as captured in kindmetal (CAPRF !309, job 407787205).

// networkdParams is the parameter request list (option 55) of networkd:
// subnet mask, router, DNS, host name, domain name, MTU, static routes,
// NTP, captive portal, domain search, SIP servers, classless routes.
var networkdParams = []layers.DHCPOpt{1, 3, 6, 12, 15, 26, 33, 42, 114, 119, 120, 121}

const (
	dhcpOptStaticRoutes    layers.DHCPOpt = 33
	dhcpOptDomainSearch    layers.DHCPOpt = 119
	dhcpOptClasslessRoutes layers.DHCPOpt = 121
	// systemdEnterprise is the IANA enterprise number of systemd in its
	// DUID-EN (sd-dhcp-duid.h).
	systemdEnterprise = 43793
)

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

// leaseOption returns the payload of a lease option that the client keeps
// in OtherOptions.
func leaseOption(lease *dhcp4.Lease, opt layers.DHCPOpt) ([]byte, bool) {
	for _, o := range lease.OtherOptions {
		if o.Type == opt {
			return o.Data, true
		}
	}
	return nil, false
}

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
	if len(data)%8 != 0 {
		return nil
	}
	var out []dhcpRoute
	for b := data; len(b) >= 8; b = b[8:] {
		dst := net.IP(append([]byte(nil), b[:4]...))
		if dst.IsUnspecified() || dst[0] >= 224 {
			continue // no classful mask for class D and E
		}
		mask := dst.DefaultMask()
		out = append(out, dhcpRoute{dst: &net.IPNet{IP: dst.Mask(mask), Mask: mask}, gw: net.IP(append([]byte(nil), b[4:8]...))})
	}
	return out
}

// classlessRoutes decodes option 121 (RFC 3442). A malformed option is
// ignored as a whole, like networkd does.
func classlessRoutes(data []byte) ([]dhcpRoute, bool) {
	var out []dhcpRoute
	for b := data; len(b) > 0; {
		width := int(b[0])
		n := (width + 7) / 8
		if width > 32 || len(b) < 1+n+4 {
			return nil, false
		}
		dst := make(net.IP, net.IPv4len)
		copy(dst, b[1:1+n])
		mask := net.CIDRMask(width, 32)
		gw := net.IP(append([]byte(nil), b[1+n:5+n]...))
		out = append(out, dhcpRoute{dst: &net.IPNet{IP: dst.Mask(mask), Mask: mask}, gw: gw})
		b = b[5+n:]
	}
	return out, len(out) > 0
}

// domainSearch decodes option 119 (RFC 3397): DNS names in wire format,
// with compression pointers into the option. A malformed option (a
// truncated name, a pointer that does not point back, a reserved label
// type) is ignored as a whole, like networkd (-EBADMSG).
func domainSearch(data []byte) []string {
	var out []string
	for off := 0; off < len(data); {
		name, next, ok := dnsName(data, off)
		if !ok {
			return nil
		}
		if name != "" {
			out = append(out, name)
		}
		off = next
	}
	return out
}

// dnsName reads the name at off and returns it and the offset after it.
func dnsName(data []byte, off int) (name string, next int, ok bool) {
	var labels []string
	next, wire := -1, 0
	for jumps := 0; off < len(data) && jumps < 64; {
		l := int(data[off])
		switch {
		case l == 0:
			if next < 0 {
				next = off + 1
			}
			return strings.Join(labels, "."), next, true
		case l&0xc0 == 0xc0:
			if off+1 >= len(data) {
				return "", 0, false
			}
			if next < 0 {
				next = off + 2
			}
			ptr := (l&0x3f)<<8 | int(data[off+1])
			if ptr >= off {
				return "", 0, false // a pointer must point back
			}
			off = ptr
			jumps++
		case l&0xc0 != 0:
			return "", 0, false // label types 01 and 10 are reserved
		case off+1+l <= len(data):
			labels = append(labels, string(data[off+1:off+1+l]))
			if wire += 1 + l; wire > 254 {
				return "", 0, false // over 255 octets (RFC 1035 2.3.4)
			}
			off += 1 + l
		default:
			return "", 0, false
		}
	}
	return "", 0, false
}

// leaseRouteSet returns the routes of a lease: option 121, or without it
// option 33 and a default route through the first router that is not
// 0.0.0.0 (none when all are), like networkd-dhcp4.c.
func leaseRouteSet(lease *dhcp4.Lease) []dhcpRoute {
	if data, ok := leaseOption(lease, dhcpOptClasslessRoutes); ok {
		if routes, ok := classlessRoutes(data); ok {
			return routes
		}
	}
	var routes []dhcpRoute
	if data, ok := leaseOption(lease, dhcpOptStaticRoutes); ok {
		routes = staticRoutes(data)
	}
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
	if data, ok := leaseOption(lease, dhcpOptDomainSearch); ok {
		for _, d := range domainSearch(data) {
			add(d)
		}
	}
	return out
}
