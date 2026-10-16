package frrconf

import "slices"

// The tables below list, per CLI node, the first words of the commands of
// the current FRR (checked against FRR 10.3.1: lib/command.c node fallback,
// bgpd/bgp_vty.c and bgpd/bgp_evpn_vty.c install_element, bfdd/bfdd_cli.c,
// lib/routemap_cli.c), which the legacy ramdisk runs. Where FRR 8.2.2
// installed a command in another node, the current FRR wins. They decide in which node FRR would apply a
// statement that BOOTy does not implement, so a skipped statement moves the
// parser to the same node as FRR.

var configWords = []string{
	"frr", "hostname", "domainname", "log", "service", "password", "enable",
	"banner", "debug", "ip", "ipv6", "vni", "router", "bfd", "route-map", "bgp",
	"vrf", "interface", "line", "key", "access-list", "agentx", "zebra", "table",
	"allow-external-route-update", "fpm", "mpls", "segment-routing", "pbr-map",
	"nexthop-group", "l2vpn", "rpki", "username", "evpn", "router-id",
	"ptm-enable", "dump", "pseudowire", "end", "exit",
}

var bgpWords = []string{
	"bgp", "neighbor", "address-family", "network", "aggregate-address",
	"redistribute", "maximum-paths", "distance", "timers", "coalesce-time",
	"read-quanta", "write-quanta", "update-delay", "table-map", "vnc", "bmp",
	"segment-routing", "sid", "exit",
}

// afWords are the commands of the unicast address-families.
var afWords = []string{
	"network", "aggregate-address", "redistribute", "maximum-paths", "neighbor",
	"table-map", "distance", "bgp", "import", "export", "rd", "rt", "label",
	"nexthop", "route-map", "sid", "route-target", "route-target6", "rt6",
	"exit-address-family", "exit",
}

// evpnWords are the commands of BGP_EVPN_NODE (address-family l2vpn evpn).
var evpnWords = []string{
	"advertise", "advertise-all-vni", "advertise-default-gw",
	"advertise-svi-ip", "advertise-pip", "autort",
	"default-originate", "dup-addr-detection", "exit-address-family",
	"flooding", "disable-ead-evi-rx", "disable-ead-evi-tx", "use-es-l3nhg",
	"neighbor", "network", "rd", "route-target", "vni", "mac-vrf",
	"enable-resolve-overlay-index", "ead-es-route-target", "ead-es-frag",
	"test", "exit",
}

// evpnNeighborWords are the neighbor attributes of BGP_EVPN_NODE.
var evpnNeighborWords = []string{
	"activate", "allowas-in", "attribute-unchanged", "next-hop-self",
	"route-map", "route-reflector-client", "route-server-client",
	"soft-reconfiguration", "maximum-prefix", "soo",
}

// afNeighborWords are the neighbor attributes of an address-family. FRR
// also accepts them in the router node (for ipv4 unicast).
var afNeighborWords = []string{
	"activate", "addpath-tx-all-paths", "addpath-tx-bestpath-per-AS",
	"allowas-in", "as-override", "attribute-unchanged", "default-originate",
	"distribute-list", "filter-list", "maximum-prefix", "maximum-prefix-out",
	"next-hop-self", "prefix-list", "remove-private-AS", "route-map",
	"route-reflector-client", "route-server-client", "send-community",
	"soft-reconfiguration", "unsuppress-map", "weight", "advertise-map",
	"disable-addpath-rx", "nexthop-local", "soo", "dampening", "addpath-rx-paths-limit",
}

var l2vniWords = []string{"rd", "route-target", "advertise-default-gw", "advertise-svi-ip", "advertise-subnet", "exit-vni", "exit"}

var routeMapWords = []string{"match", "set", "call", "on-match", "continue", "description", "exit"}

var bfdWords = []string{"profile", "peer", "sbfd", "exit"}

var bfdProfileWords = []string{
	"transmit-interval", "receive-interval", "detect-multiplier", "echo-mode",
	"echo-interval", "echo", "minimum-ttl", "passive-mode", "shutdown", "exit",
}

var vrfWords = []string{"vni", "ip", "ipv6", "netns", "exit-vrf", "exit"}

var lineVTYWords = []string{"exec-timeout", "login", "access-class", "ipv6", "exit"}

// nodeWords are the command tables of the nodes other than the
// address-families.
var nodeWords = map[node][]string{
	nodeConfig:     configWords,
	nodeBGP:        bgpWords,
	nodeL2VNI:      l2vniWords,
	nodeRouteMap:   routeMapWords,
	nodeBFD:        bfdWords,
	nodeBFDProfile: bfdProfileWords,
	nodeVRF:        vrfWords,
	nodeLineVTY:    lineVTYWords,
}

// knownIn reports whether FRR has a command in node n that starts like f.
// evpn selects the l2vpn evpn address-family for nodeAF.
func knownIn(n node, evpn bool, f []string) bool {
	f = unNo(f)
	if n == nodeAF {
		return knownInAF(evpn, f)
	}
	return slices.Contains(nodeWords[n], f[0])
}

func knownInAF(evpn bool, f []string) bool {
	switch {
	case evpn && f[0] == "neighbor":
		return len(f) > 2 && slices.Contains(evpnNeighborWords, f[2])
	case evpn:
		return slices.Contains(evpnWords, f[0])
	case f[0] == "neighbor":
		return isAFNeighborAttr(f)
	}
	return slices.Contains(afWords, f[0])
}

// isAFNeighborAttr reports whether "neighbor X ATTR ..." is an
// address-family command. "capability orf" is one, the other capabilities
// belong to the router node.
func isAFNeighborAttr(f []string) bool {
	if len(f) < 3 {
		return false
	}
	if f[2] == "capability" {
		return len(f) > 3 && f[3] == "orf"
	}
	return slices.Contains(afNeighborWords, f[2])
}
