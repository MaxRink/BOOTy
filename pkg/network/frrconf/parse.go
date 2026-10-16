package frrconf

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// node is the FRR CLI node a statement belongs to.
type node int

const (
	nodeConfig node = iota
	nodeBGP
	nodeAF
	nodeRouteMap
	nodeBFD
	nodeBFDProfile
	nodeVRF
	nodeLineVTY
	nodeL2VNI
)

// errNoMatch means the statement is not valid in the tried node.
var errNoMatch = errors.New("no match")

// errUnsupported marks a statement that BOOTy does not implement. Only such
// statements are skipped in lenient mode; an invalid value stays an error.
var errUnsupported = errors.New("not supported by BOOTy")

// unsupported wraps a reason as errUnsupported.
func unsupported(reason string) error {
	return fmt.Errorf("%s: %w", reason, errUnsupported)
}

// blockOpeners are the first words of statements that open a node in FRR.
// An unsupported one stays an error in lenient mode: skipping the opener
// would apply its body to the enclosing node.
//
// The list covers the vtysh node table of the current FRR (10.3): router (bgp, ospf, ospf6,
// rip, ripng, isis, openfabric, eigrp, babel, pim), vrf, interface,
// address-family, vni, route-map, bfd with its profile and peer nodes, line,
// key chain and key, segment-routing, srv6, locators and locator, mpls ldp,
// l2vpn and pseudowire, pbr-map, nexthop-group, rpki, bmp targets, vnc,
// link-params, affinity-map and evpn mh.
var blockOpeners = []string{
	"router", "vrf", "interface", "address-family", "vni", "route-map", "bfd",
	"profile", "peer", "line", "key", "segment-routing", "srv6", "locators",
	"locator", "mpls", "l2vpn", "pseudowire", "pbr-map", "nexthop-group",
	"rpki", "bmp", "vnc", "vrf-policy", "link-params", "affinity-map", "evpn",
}

// ParseError is a statement BOOTy does not accept.
type ParseError struct {
	Line int
	Text string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d %q: %v", e.Line, e.Text, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// parser keeps the node stack. Each level remembers the object that the
// block statement opened, so nested statements can fill it.
type parser struct {
	cfg   *Config
	stack []node
	// Open objects, valid while the matching node is on the stack.
	router  int // index into cfg.Routers
	af      int // index into cfg.Routers[router].AddressFamilies
	rmIndex int // index into cfg.RouteMaps
	rmEntry int // index into cfg.RouteMaps[routeMap].Entries
	profile int // index into cfg.BFDProfiles
	vrf     string
	line    int
	text    string
	warn    func(*ParseError)
	// unsure is the stack depth of a skipped statement that FRR does not
	// know in any node of the stack. The statements after it in the same
	// block may land in another node in FRR; each gets a warning.
	unsure int
}

// Parse reads an FRR configuration. Leading blanks do not matter and lines
// that start with "!" or "#" are comments, like FRR. A statement that is not
// valid in the current node is tried in the parent nodes, and the parser
// stays in the node where it matched (lib/command.c,
// command_config_read_one_line). A statement that no node accepts is an
// error: BOOTy implements only the statements listed in this package.
func Parse(data []byte) (*Config, error) {
	return ParseWith(data, Options{})
}

// Options change how Parse treats a statement that BOOTy does not accept.
type Options struct {
	// Lenient skips such a statement instead of failing (FRR_CONFIG_STRICT
	// =false). Warn gets each skipped statement. An invalid value and an
	// unsupported statement that opens a block stay errors.
	Lenient bool
	// Warn gets each skipped statement; nil logs it with slog.
	Warn func(*ParseError)
}

// ParseWith is Parse with options.
func ParseWith(data []byte, o Options) (*Config, error) {
	warn := o.Warn
	if warn == nil {
		warn = func(e *ParseError) {
			slog.Warn("frr.conf statement skipped", "line", e.Line, "statement", e.Text, "error", e.Err)
		}
	}
	p := &parser{cfg: &Config{}, stack: []node{nodeConfig}, warn: warn}
	for i, raw := range strings.Split(string(data), "\n") {
		p.line = i + 1
		p.text = strings.TrimSpace(raw)
		fields := strings.Fields(raw)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "!") || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if err := p.statement(fields); err != nil {
			pe := &ParseError{Line: p.line, Text: p.text, Err: err}
			if !o.Lenient || !errors.Is(err, errUnsupported) || slices.Contains(blockOpeners, unNo(fields)[0]) {
				return nil, pe
			}
			p.warn(pe)
		}
	}
	return p.cfg, nil
}

// errAfterSkipped marks a statement that follows a skipped one in the same
// block when FRR's node for the skipped one is not known.
var errAfterSkipped = errors.New("applied after a skipped statement that FRR does not know; FRR may apply it in another node")

// unNo drops a leading "no".
func unNo(f []string) []string {
	if len(f) > 1 && f[0] == "no" {
		return f[1:]
	}
	return f
}

// statement applies one statement in the deepest node that accepts it. If
// no node accepts it, the parser stays in its node and the error names it.
func (p *parser) statement(f []string) error {
	saved := slices.Clone(p.stack)
	for depth := len(saved) - 1; depth >= 0; depth-- {
		p.stack = slices.Clone(saved[:depth+1])
		err := p.apply(p.stack[depth], f)
		if errors.Is(err, errNoMatch) {
			continue
		}
		if err != nil {
			p.stack = saved
			return fmt.Errorf("%w (in %s)", err, p.context(saved))
		}
		p.afterApply(depth)
		return nil
	}
	// No node of BOOTy accepts it. FRR applies a command it knows in the
	// deepest node of the stack that has it, and stays there; move there
	// too, so the next statements land where FRR puts them.
	for depth := len(saved) - 1; depth >= 0; depth-- {
		if knownIn(saved[depth], p.inEVPN(saved[depth]), f) {
			p.stack = slices.Clone(saved[:depth+1])
			if depth+1 < p.unsure {
				p.unsure = 0
			}
			return fmt.Errorf("statement %w (in %s)", errUnsupported, p.context(p.stack))
		}
	}
	p.stack = saved
	// At the top level there is no parent node to drift to.
	if len(saved) > 1 && (p.unsure == 0 || len(saved) < p.unsure) {
		p.unsure = len(saved)
	}
	return fmt.Errorf("statement %w (in %s)", errUnsupported, p.context(saved))
}

// inEVPN reports whether n is the l2vpn evpn address-family node.
func (p *parser) inEVPN(n node) bool {
	return n == nodeAF && p.currentRouter().AddressFamilies[p.af].SAFI == "evpn"
}

// afterApply warns about a statement that was applied in the block of a
// skipped statement whose FRR node is not known, and ends that block when a
// statement is applied in a parent node.
func (p *parser) afterApply(depth int) {
	if p.unsure == 0 {
		return
	}
	if depth+1 < p.unsure {
		p.unsure = 0
		return
	}
	p.warn(&ParseError{Line: p.line, Text: p.text, Err: errAfterSkipped})
}

// context names the nodes of a stack, like "router bgp 65000 >
// address-family l2vpn evpn".
func (p *parser) context(stack []node) string {
	if len(stack) == 1 {
		return "the top level"
	}
	parts := make([]string, 0, len(stack)-1)
	for _, n := range stack[1:] {
		parts = append(parts, p.nodeName(n))
	}
	return strings.Join(parts, " > ")
}

func (p *parser) nodeName(n node) string {
	switch n {
	case nodeBGP:
		r := p.currentRouter()
		if r.VRF != "" {
			return fmt.Sprintf("router bgp %d vrf %s", r.ASN, r.VRF)
		}
		return fmt.Sprintf("router bgp %d", r.ASN)
	case nodeAF:
		af := p.currentRouter().AddressFamilies[p.af]
		return "address-family " + af.AFI + " " + af.SAFI
	case nodeRouteMap:
		rm := p.cfg.RouteMaps[p.rmIndex]
		e := rm.Entries[p.rmEntry]
		return fmt.Sprintf("route-map %s %s %d", rm.Name, e.Action, e.Seq)
	case nodeBFD:
		return "bfd"
	case nodeBFDProfile:
		return "profile " + p.cfg.BFDProfiles[p.profile].Name
	case nodeVRF:
		return "vrf " + p.vrf
	case nodeLineVTY:
		return "line vty"
	case nodeL2VNI:
		af := p.currentRouter().AddressFamilies[p.af]
		return fmt.Sprintf("vni %d", af.L2VNIs[len(af.L2VNIs)-1].VNI)
	}
	return "config"
}

func (p *parser) push(n node) { p.stack = append(p.stack, n) }

func (p *parser) pop() {
	if len(p.stack) > 1 {
		p.stack = p.stack[:len(p.stack)-1]
	}
}

func (p *parser) apply(n node, f []string) error {
	switch n {
	case nodeConfig:
		return p.configStatement(f)
	case nodeBGP:
		return p.bgpStatement(f)
	case nodeAF:
		return p.afStatement(f)
	case nodeRouteMap:
		return p.routeMapStatement(f)
	case nodeBFD:
		return p.bfdStatement(f)
	case nodeBFDProfile:
		return p.bfdProfileStatement(f)
	case nodeVRF:
		return p.vrfStatement(f)
	case nodeL2VNI:
		return p.l2vniStatement(f)
	case nodeLineVTY:
		if is(f, "exit") {
			p.pop()
			return nil
		}
	}
	return errNoMatch
}

// is reports whether f is exactly the given words.
func is(f []string, words ...string) bool {
	if len(f) != len(words) {
		return false
	}
	for i, w := range words {
		if f[i] != w {
			return false
		}
	}
	return true
}

// has reports whether f starts with the given words.
func has(f []string, words ...string) bool {
	return len(f) >= len(words) && is(f[:len(words)], words...)
}

//nolint:cyclop // one case per accepted top-level statement
func (p *parser) configStatement(f []string) error {
	switch {
	case has(f, "frr", "version") && len(f) == 3:
		p.cfg.Version = f[2]
	case has(f, "frr", "defaults") && len(f) == 3:
		switch Profile(f[2]) {
		case ProfileTraditional, ProfileDatacenter:
			p.cfg.Defaults = Profile(f[2])
		default:
			return fmt.Errorf("unknown frr defaults profile %q", f[2])
		}
	case has(f, "hostname") && len(f) == 2:
		p.cfg.Hostname = f[1]
	case isLogStatement(f), has(f, "debug") && len(f) >= 2:
		// No effect on routing: log and debug output only.
	case is(f, "service", "integrated-vtysh-config"):
	case is(f, "ip", "forwarding"), is(f, "no", "ip", "forwarding"):
		v := f[0] != "no"
		p.cfg.IPForwarding = &v
	case is(f, "ipv6", "forwarding"), is(f, "no", "ipv6", "forwarding"):
		v := f[0] != "no"
		p.cfg.IPv6Forwarding = &v
	case has(f, "vni"):
		return p.vni(f, "")
	case has(f, "ip", "route"), has(f, "ipv6", "route"):
		return p.staticRoute(f)
	case has(f, "router", "bgp"):
		return p.routerBGP(f)
	case is(f, "bfd"):
		p.push(nodeBFD)
	case has(f, "route-map"):
		return p.routeMap(f)
	case has(f, "bgp", "community-list"):
		return p.communityList(f)
	case has(f, "vrf") && len(f) == 2:
		p.vrf = f[1]
		p.push(nodeVRF)
	case is(f, "line", "vty"):
		p.push(nodeLineVTY)
	case is(f, "exit"), is(f, "end"):
	default:
		return errNoMatch
	}
	return nil
}

// isLogStatement accepts the logging statements of the production template.
// They only change where FRR logs.
func isLogStatement(f []string) bool {
	switch {
	case has(f, "log", "stdout") && len(f) <= 3,
		has(f, "log", "syslog") && len(f) <= 3,
		has(f, "log", "file") && (len(f) == 3 || len(f) == 4),
		has(f, "log", "timestamp", "precision") && len(f) == 4:
		return true
	}
	return false
}

func (p *parser) vni(f []string, vrf string) error {
	if len(f) != 2 && (len(f) != 3 || f[2] != "prefix-routes-only") {
		return errNoMatch
	}
	vni, err := parseUint32(f[1], "VNI")
	if err != nil {
		return err
	}
	p.cfg.VRFVNIs = append(p.cfg.VRFVNIs, VRFVNI{VRF: vrf, VNI: vni, PrefixRoutesOnly: len(f) == 3, Line: p.line})
	return nil
}

func (p *parser) staticRoute(f []string) error {
	if len(f) != 4 || f[3] != "blackhole" {
		return unsupported("only blackhole static routes are supported")
	}
	prefix, err := netip.ParsePrefix(f[2])
	if err != nil {
		return fmt.Errorf("invalid prefix: %w", err)
	}
	if (f[0] == "ip") != prefix.Addr().Is4() {
		return fmt.Errorf("%s route with a prefix of the other address family", f[0])
	}
	p.cfg.StaticRoutes = append(p.cfg.StaticRoutes, StaticRoute{Prefix: prefix.String(), Blackhole: true, Line: p.line})
	return nil
}

func (p *parser) routerBGP(f []string) error {
	if len(f) != 3 && (len(f) != 5 || f[3] != "vrf") {
		return errNoMatch
	}
	asn, err := parseUint32(f[2], "ASN")
	if err != nil {
		return err
	}
	r := Router{ASN: asn, Line: p.line}
	if len(f) == 5 {
		r.VRF = f[4]
	}
	p.cfg.Routers = append(p.cfg.Routers, r)
	p.router = len(p.cfg.Routers) - 1
	p.push(nodeBGP)
	return nil
}

func (p *parser) currentRouter() *Router { return &p.cfg.Routers[p.router] }

func (p *parser) vrfStatement(f []string) error {
	switch {
	case has(f, "vni"):
		return p.vni(f, p.vrf)
	case has(f, "ip", "route"), has(f, "ipv6", "route"):
		// A route of the VRF table; BOOTy installs static routes only in
		// the main table.
		return unsupported("static routes inside vrf " + p.vrf)
	case is(f, "exit-vrf"), is(f, "exit"):
		p.pop()
		return nil
	}
	return errNoMatch
}

func boolPtr(v bool) *bool { return &v }

// routerFlag sets a "[no] bgp FLAG" router flag and reports whether f is
// one.
func routerFlag(r *Router, f []string) bool {
	flags := map[string]**bool{
		"ebgp-requires-policy":   &r.EBGPRequiresPolicy,
		"suppress-duplicates":    &r.SuppressDuplicates,
		"fast-external-failover": &r.FastExternalFailover,
	}
	words := f
	if len(words) > 0 && words[0] == "no" {
		words = words[1:]
	}
	switch {
	case is(words, "bgp", "default", "ipv4-unicast"):
		r.DefaultIPv4Unicast = boolPtr(f[0] != "no")
		return true
	case len(words) == 2 && words[0] == "bgp" && flags[words[1]] != nil:
		*flags[words[1]] = boolPtr(f[0] != "no")
		return true
	}
	return false
}

func (p *parser) bgpStatement(f []string) error {
	r := p.currentRouter()
	switch {
	case has(f, "bgp", "router-id") && len(f) == 3:
		addr, err := netip.ParseAddr(f[2])
		if err != nil || !addr.Is4() {
			return fmt.Errorf("invalid router-id %q", f[2])
		}
		r.RouterID = f[2]
	case routerFlag(r, f):
	case is(f, "bgp", "bestpath", "as-path", "multipath-relax"):
		r.MultipathRelax = true
	case has(f, "bgp", "bestpath", "as-path", "multipath-relax"):
		// as-set and no-as-set change the aggregate path, which BOOTy does
		// not build; an error in both modes, not a skippable statement.
		return fmt.Errorf("bgp bestpath as-path multipath-relax %s is not supported", strings.Join(f[4:], " "))
	case is(f, "bgp", "log-neighbor-changes"), is(f, "no", "bgp", "log-neighbor-changes"):
		// Logging only.
	case has(f, "neighbor") && len(f) >= 3:
		return p.neighborStatement(r, f[1], f[2:])
	case has(f, "address-family"):
		return p.addressFamily(r, f)
	case is(f, "exit"):
		p.pop()
	default:
		return errNoMatch
	}
	return nil
}

// neighbor returns the neighbor or peer-group entry NAME, creating it.
func (p *parser) neighbor(r *Router, name string) *Neighbor {
	for i := range r.Neighbors {
		if r.Neighbors[i].Name == name {
			return &r.Neighbors[i]
		}
	}
	r.Neighbors = append(r.Neighbors, Neighbor{Name: name, Line: p.line})
	return &r.Neighbors[len(r.Neighbors)-1]
}

// findNeighbor returns the neighbor or peer-group NAME, or nil.
func findNeighbor(r *Router, name string) *Neighbor {
	for i := range r.Neighbors {
		if r.Neighbors[i].Name == name {
			return &r.Neighbors[i]
		}
	}
	return nil
}

func (p *parser) neighborStatement(r *Router, name string, rest []string) error {
	switch {
	case is(rest, "peer-group"):
		n := p.neighbor(r, name)
		n.PeerGroup = true
		return nil
	case rest[0] == "interface":
		return p.interfaceNeighbor(r, name, rest[1:])
	case rest[0] == "description":
		// Free text without effect on the session. FRR needs a known peer.
		if findNeighbor(r, name) == nil {
			return fmt.Errorf("neighbor %s is not defined", name)
		}
		return nil
	case len(rest) == 2 && rest[0] == "peer-group":
		if n := findNeighbor(r, name); n == nil || !n.PeerGroup && !n.Interface {
			if _, err := netip.ParseAddr(name); err != nil {
				return fmt.Errorf("neighbor %s is not an address", name)
			}
		}
		if err := checkGroup(r, rest[1]); err != nil {
			return err
		}
		p.neighbor(r, name).Group = rest[1]
		return nil
	}
	if n := findNeighbor(r, name); n != nil {
		return p.neighborAttribute(n, rest)
	}
	return p.newNeighbor(r, name, rest)
}

// newNeighbor reads "neighbor ADDR remote-as R" for a new peer: FRR accepts
// it for an address, every other attribute needs a known peer or
// peer-group. The statement is checked before the peer exists, so a
// rejected one leaves no peer behind.
func (p *parser) newNeighbor(r *Router, name string, rest []string) error {
	if rest[0] != "remote-as" {
		return fmt.Errorf("neighbor %s is not defined", name)
	}
	if _, err := netip.ParseAddr(name); err != nil {
		return fmt.Errorf("neighbor %s is not an address, an interface or a peer-group", name)
	}
	if err := p.neighborAttribute(&Neighbor{}, rest); err != nil {
		return err
	}
	return p.neighborAttribute(p.neighbor(r, name), rest)
}

// interfaceNeighbor reads "neighbor IFNAME interface [peer-group G |
// remote-as R]". It checks the statement before it creates the neighbor, so
// a rejected statement does not leave a peer behind.
func (p *parser) interfaceNeighbor(r *Router, name string, rest []string) error {
	var group string
	var ras *RemoteAS
	switch {
	case len(rest) == 0:
	case len(rest) == 2 && rest[0] == "peer-group":
		if err := checkGroup(r, rest[1]); err != nil {
			return err
		}
		group = rest[1]
	case len(rest) == 2 && rest[0] == "remote-as":
		v, err := parseRemoteAS(rest[1])
		if err != nil {
			return err
		}
		ras = &v
	default:
		return errNoMatch
	}
	n := p.neighbor(r, name)
	n.Interface = true
	if group != "" {
		n.Group = group
	}
	if ras != nil {
		n.RemoteAS = ras
	}
	return nil
}

// checkGroup reports an error unless group is a defined peer-group.
func checkGroup(r *Router, group string) error {
	if g := findNeighbor(r, group); g == nil || !g.PeerGroup {
		return fmt.Errorf("peer-group %s is not defined", group)
	}
	return nil
}

func (p *parser) neighborAttribute(n *Neighbor, rest []string) error {
	switch {
	case has(rest, "remote-as") && len(rest) == 2:
		ras, err := parseRemoteAS(rest[1])
		if err != nil {
			return err
		}
		n.RemoteAS = &ras
	case has(rest, "local-as"):
		return parseLocalAS(n, rest)
	case is(rest, "bfd"):
		n.BFD = true
	case has(rest, "bfd", "profile") && len(rest) == 3:
		n.BFD = true
		n.BFDProfile = rest[2]
	case has(rest, "timers", "connect") && len(rest) == 3:
		v, err := parseUint32(rest[2], "connect timer")
		if err != nil {
			return err
		}
		n.ConnectRetry = &v
	case has(rest, "timers") && len(rest) == 3:
		return parseTimers(n, rest[1], rest[2])
	default:
		return errNoMatch
	}
	return nil
}

func parseRemoteAS(v string) (RemoteAS, error) {
	switch v {
	case "internal":
		return RemoteAS{Kind: RemoteASInternal}, nil
	case "external":
		return RemoteAS{Kind: RemoteASExternal}, nil
	case "auto":
		// FRR 10 "remote-as auto" learns iBGP or eBGP from the peer.
		return RemoteAS{}, errors.New("remote-as auto is not supported; use a number, internal or external")
	}
	asn, err := parseUint32(v, "remote-as")
	if err != nil {
		return RemoteAS{}, err
	}
	return RemoteAS{Kind: RemoteASNumber, Number: asn}, nil
}

// parseLocalAS reads "local-as N [no-prepend [replace-as]]". FRR accepts
// replace-as only after no-prepend (doc/user/bgp.rst).
func parseLocalAS(n *Neighbor, rest []string) error {
	if len(rest) < 2 || len(rest) > 4 {
		return errNoMatch
	}
	asn, err := parseUint32(rest[1], "local-as")
	if err != nil {
		return err
	}
	la := LocalAS{ASN: asn}
	switch {
	case len(rest) == 2:
	case is(rest[2:], "no-prepend"):
		la.NoPrepend = true
	case is(rest[2:], "no-prepend", "replace-as"):
		la.NoPrepend, la.ReplaceAS = true, true
	default:
		return errNoMatch
	}
	n.LocalAS = &la
	return nil
}

// parseTimers reads "timers KEEPALIVE HOLD". FRR accepts keepalive 0-65535
// and hold 0 or 3-65535.
func parseTimers(n *Neighbor, keepalive, hold string) error {
	k, err := parseUint32(keepalive, "keepalive")
	if err != nil {
		return err
	}
	h, err := parseUint32(hold, "hold time")
	if err != nil {
		return err
	}
	if k > 65535 || h > 65535 || (h != 0 && h < 3) {
		return fmt.Errorf("invalid timers %d %d", k, h)
	}
	n.KeepaliveTime, n.HoldTime = &k, &h
	return nil
}

func (p *parser) addressFamily(r *Router, f []string) error {
	var afi, safi string
	switch {
	case is(f, "address-family", "ipv4", "unicast"), is(f, "address-family", "ipv4"):
		afi, safi = "ipv4", "unicast"
	case is(f, "address-family", "ipv6", "unicast"), is(f, "address-family", "ipv6"):
		afi, safi = "ipv6", "unicast"
	case is(f, "address-family", "l2vpn", "evpn"):
		afi, safi = "l2vpn", "evpn"
	default:
		return unsupported("address-family " + strings.Join(f[1:], " "))
	}
	for i := range r.AddressFamilies {
		if r.AddressFamilies[i].AFI == afi && r.AddressFamilies[i].SAFI == safi {
			p.af = i
			p.push(nodeAF)
			return nil
		}
	}
	r.AddressFamilies = append(r.AddressFamilies, AddressFamily{AFI: afi, SAFI: safi, Line: p.line})
	p.af = len(r.AddressFamilies) - 1
	p.push(nodeAF)
	return nil
}

//nolint:cyclop // one case per accepted address-family statement
func (p *parser) afStatement(f []string) error {
	r := p.currentRouter()
	af := &r.AddressFamilies[p.af]
	evpn := af.SAFI == "evpn"
	switch {
	case has(f, "network") && len(f) == 2 && !evpn:
		prefix, err := netip.ParsePrefix(f[1])
		if err != nil {
			return fmt.Errorf("invalid network: %w", err)
		}
		if (af.AFI == "ipv4") != prefix.Addr().Is4() {
			return fmt.Errorf("network %s in address-family %s", f[1], af.AFI)
		}
		af.Networks = append(af.Networks, prefix.String())
	case is(f, "redistribute", "connected") && !evpn:
		af.RedistributeConnected = true
	case has(f, "neighbor") && len(f) >= 3:
		return p.afNeighbor(r, af, f[1], f[2:])
	case is(f, "advertise-all-vni") && evpn:
		af.AdvertiseAllVNI = true
	case is(f, "advertise-default-gw") && evpn:
		af.AdvertiseDefaultGW = true
	case has(f, "vni") && len(f) == 2 && evpn:
		vni, err := parseUint32(f[1], "VNI")
		if err != nil {
			return err
		}
		af.L2VNIs = append(af.L2VNIs, L2VNI{VNI: vni, Line: p.line})
		p.push(nodeL2VNI)
	case is(f, "advertise", "ipv4", "unicast") && evpn:
		af.AdvertiseIPv4Unicast = true
	case is(f, "advertise", "ipv6", "unicast") && evpn:
		af.AdvertiseIPv6Unicast = true
	case has(f, "route-target") && len(f) == 3 && evpn:
		return p.routeTarget(af, f[1], f[2])
	case is(f, "exit-address-family"), is(f, "exit"):
		p.pop()
	default:
		return errNoMatch
	}
	return nil
}

func (p *parser) routeTarget(af *AddressFamily, direction, rt string) error {
	return addRouteTarget(&af.RouteTargetImport, &af.RouteTargetExport, direction, rt)
}

func addRouteTarget(imp, exp *[]string, direction, rt string) error {
	if err := validateExtCommunity(rt); err != nil {
		return fmt.Errorf("invalid route-target %q: %w", rt, err)
	}
	switch direction {
	case "import":
		*imp = append(*imp, rt)
	case "export":
		*exp = append(*exp, rt)
	case "both":
		*imp = append(*imp, rt)
		*exp = append(*exp, rt)
	default:
		return errNoMatch
	}
	return nil
}

// l2vniStatement reads the RD and route targets of an L2VNI block.
func (p *parser) l2vniStatement(f []string) error {
	af := &p.currentRouter().AddressFamilies[p.af]
	v := &af.L2VNIs[len(af.L2VNIs)-1]
	switch {
	case has(f, "rd") && len(f) == 2:
		if err := validateExtCommunity(f[1]); err != nil {
			return fmt.Errorf("invalid rd %q: %w", f[1], err)
		}
		v.RD = f[1]
	case has(f, "route-target") && len(f) == 3:
		return addRouteTarget(&v.RouteTargetImport, &v.RouteTargetExport, f[1], f[2])
	case is(f, "advertise-default-gw"):
		// FRR has it per VNI too (bgp_evpn_vty.c, bgp_evpn_advertise_default_gw_vni).
		v.AdvertiseDefaultGW = true
	case is(f, "exit-vni"), is(f, "exit"):
		p.pop()
	default:
		return errNoMatch
	}
	return nil
}

//nolint:cyclop // one case per accepted address-family neighbor statement
func (p *parser) afNeighbor(r *Router, af *AddressFamily, name string, rest []string) error {
	var set func(*AFNeighbor)
	switch {
	case is(rest, "activate"):
		set = func(an *AFNeighbor) { an.Activate = true }
	case is(rest, "soft-reconfiguration", "inbound"):
		set = func(an *AFNeighbor) { an.SoftReconfigInbound = true }
	case is(rest, "allowas-in"):
		set = func(an *AFNeighbor) { an.AllowASIn = 3 }
	case has(rest, "allowas-in") && len(rest) == 2:
		n, err := strconv.Atoi(rest[1])
		if err != nil || n < 1 || n > 10 {
			return fmt.Errorf("invalid allowas-in count %q", rest[1])
		}
		set = func(an *AFNeighbor) { an.AllowASIn = n }
	case has(rest, "route-map") && len(rest) == 3 && rest[2] == "in":
		set = func(an *AFNeighbor) { an.RouteMapIn = rest[1] }
	case has(rest, "route-map") && len(rest) == 3 && rest[2] == "out":
		set = func(an *AFNeighbor) { an.RouteMapOut = rest[1] }
	default:
		// Not an address-family statement; FRR then tries the router node.
		return errNoMatch
	}
	if findNeighbor(r, name) == nil {
		return fmt.Errorf("neighbor %s is not defined", name)
	}
	for i := range af.Neighbors {
		if af.Neighbors[i].Name == name {
			set(&af.Neighbors[i])
			return nil
		}
	}
	af.Neighbors = append(af.Neighbors, AFNeighbor{Name: name})
	set(&af.Neighbors[len(af.Neighbors)-1])
	return nil
}

func (p *parser) routeMap(f []string) error {
	if len(f) != 4 || (f[2] != string(Permit) && f[2] != string(Deny)) {
		return errNoMatch
	}
	seq, err := strconv.Atoi(f[3])
	if err != nil || seq < 1 || seq > 65535 {
		return fmt.Errorf("invalid route-map sequence %q", f[3])
	}
	idx := -1
	for i := range p.cfg.RouteMaps {
		if p.cfg.RouteMaps[i].Name == f[1] {
			idx = i
		}
	}
	if idx < 0 {
		p.cfg.RouteMaps = append(p.cfg.RouteMaps, RouteMap{Name: f[1]})
		idx = len(p.cfg.RouteMaps) - 1
	}
	rm := &p.cfg.RouteMaps[idx]
	for _, e := range rm.Entries {
		if e.Seq == seq {
			return fmt.Errorf("route-map %s sequence %d is defined twice", f[1], seq)
		}
	}
	// Keep the entries sorted by sequence number, like FRR evaluates them.
	pos := len(rm.Entries)
	for i, e := range rm.Entries {
		if e.Seq > seq {
			pos = i
			break
		}
	}
	rm.Entries = append(rm.Entries, RouteMapEntry{})
	copy(rm.Entries[pos+1:], rm.Entries[pos:])
	rm.Entries[pos] = RouteMapEntry{Seq: seq, Action: RouteMapAction(f[2]), Line: p.line}
	p.rmIndex, p.rmEntry = idx, pos
	p.push(nodeRouteMap)
	return nil
}

func (p *parser) routeMapStatement(f []string) error {
	e := &p.cfg.RouteMaps[p.rmIndex].Entries[p.rmEntry]
	switch {
	case has(f, "set", "local-preference") && len(f) == 3:
		v, err := parseUint32(f[2], "local-preference")
		if err != nil {
			return err
		}
		e.SetLocalPreference = &v
	case has(f, "set", "community") && len(f) >= 3:
		comms := f[2:]
		additive := comms[len(comms)-1] == "additive"
		if additive {
			comms = comms[:len(comms)-1]
		}
		if len(comms) == 0 {
			return errNoMatch
		}
		for _, c := range comms {
			if err := validateCommunity(c); err != nil {
				return err
			}
		}
		e.SetCommunities, e.SetCommunityAdditive = append([]string(nil), comms...), additive
	case has(f, "match", "community") && len(f) == 3:
		e.MatchCommunity = f[2]
	case is(f, "exit"):
		p.pop()
	default:
		return errNoMatch
	}
	return nil
}

// communityList reads "bgp community-list standard NAME [seq N] permit|deny
// COMMUNITY...".
func (p *parser) communityList(f []string) error {
	if len(f) < 6 || f[2] != "standard" {
		return unsupported("only standard community-lists")
	}
	name, rest := f[3], f[4:]
	if rest[0] == "seq" {
		if len(rest) < 4 {
			return errNoMatch
		}
		rest = rest[2:]
	}
	action := RouteMapAction(rest[0])
	if action != Permit && action != Deny {
		return errNoMatch
	}
	for _, c := range rest[1:] {
		if err := validateCommunity(c); err != nil {
			return err
		}
	}
	idx := -1
	for i := range p.cfg.CommunityLists {
		if p.cfg.CommunityLists[i].Name == name {
			idx = i
		}
	}
	if idx < 0 {
		p.cfg.CommunityLists = append(p.cfg.CommunityLists, CommunityList{Name: name})
		idx = len(p.cfg.CommunityLists) - 1
	}
	cl := &p.cfg.CommunityLists[idx]
	cl.Entries = append(cl.Entries, CommunityListEntry{Action: action, Communities: append([]string(nil), rest[1:]...), Line: p.line})
	return nil
}

func (p *parser) bfdStatement(f []string) error {
	switch {
	case has(f, "profile") && len(f) == 2:
		for i := range p.cfg.BFDProfiles {
			if p.cfg.BFDProfiles[i].Name == f[1] {
				p.profile = i
				p.push(nodeBFDProfile)
				return nil
			}
		}
		p.cfg.BFDProfiles = append(p.cfg.BFDProfiles, BFDProfile{Name: f[1], Line: p.line})
		p.profile = len(p.cfg.BFDProfiles) - 1
		p.push(nodeBFDProfile)
	case is(f, "exit"):
		p.pop()
	default:
		return errNoMatch
	}
	return nil
}

// bfdProfileStatement reads the intervals of a BFD profile. FRR accepts
// 10-60000 ms and a multiplier of 2-255 (doc/user/bfd.rst). Echo mode and
// authentication are not implemented, so they are not accepted.
func (p *parser) bfdProfileStatement(f []string) error {
	prof := &p.cfg.BFDProfiles[p.profile]
	if is(f, "exit") {
		p.pop()
		return nil
	}
	if len(f) != 2 {
		return errNoMatch
	}
	var v uint32
	var err error
	switch f[0] {
	case "transmit-interval", "receive-interval":
		if v, err = parseUint32(f[1], f[0]); err != nil {
			return err
		}
		if v < 10 || v > 60000 {
			return fmt.Errorf("%s %d out of range 10-60000", f[0], v)
		}
		if f[0] == "transmit-interval" {
			prof.TransmitInterval = &v
		} else {
			prof.ReceiveInterval = &v
		}
	case "detect-multiplier":
		if v, err = parseUint32(f[1], f[0]); err != nil {
			return err
		}
		if v < 2 || v > 255 {
			return fmt.Errorf("detect-multiplier %d out of range 2-255", v)
		}
		prof.DetectMultiplier = &v
	default:
		return errNoMatch
	}
	return nil
}

func parseUint32(v, what string) (uint32, error) {
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", what, v)
	}
	return uint32(n), nil
}

// validateCommunity accepts a standard community "AA:NN" with 16-bit parts.
func validateCommunity(c string) error {
	a, b, ok := strings.Cut(c, ":")
	if !ok {
		return fmt.Errorf("invalid community %q", c)
	}
	for _, part := range []string{a, b} {
		if _, err := strconv.ParseUint(part, 10, 16); err != nil {
			return fmt.Errorf("invalid community %q", c)
		}
	}
	return nil
}

// validateExtCommunity accepts a route target "ASN:NN" (2- or 4-byte ASN)
// or "IPv4:NN".
func validateExtCommunity(rt string) error {
	a, b, ok := strings.Cut(rt, ":")
	if !ok {
		return errors.New("want ASN:NN or IPv4:NN")
	}
	if addr, err := netip.ParseAddr(a); err == nil {
		if !addr.Is4() {
			return unsupported("IPv6 route targets")
		}
		if _, err := strconv.ParseUint(b, 10, 16); err != nil {
			return errors.New("want IPv4:NN with a 16-bit NN")
		}
		return nil
	}
	asn, err := strconv.ParseUint(a, 10, 32)
	if err != nil {
		return errors.New("want ASN:NN or IPv4:NN")
	}
	bits := 32
	if asn > 65535 {
		bits = 16
	}
	if _, err := strconv.ParseUint(b, 10, bits); err != nil {
		return fmt.Errorf("want ASN:NN with a %d-bit NN", bits)
	}
	return nil
}
