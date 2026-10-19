package netplan

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/telekom/BOOTy/pkg/network"
)

// ParseDir reads all .yaml files from dir (sorted lexicographically) and
// returns a merged Config.  Later files override earlier ones at the
// top-level map key level (ethernets, tunnels, bridges, etc.). A key that
// BOOTy does not know is an error.
func ParseDir(dir string) (*Config, error) {
	return ParseDirWith(dir, true)
}

// ParseDirWith is ParseDir. With strict false (NETPLAN_STRICT=false) an
// unknown key is logged with its line and skipped.
func ParseDirWith(dir string, strict bool) (*Config, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read netplan dir %s: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no netplan YAML files in %s", dir)
	}

	merged := &Config{Network: NetworkSection{Version: 2}}
	for _, f := range files {
		cfg, err := parseFile(f, strict)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		mergeConfig(merged, cfg)
	}
	return merged, nil
}

// checkRoutes handles route keys that BOOTy parses but does not apply:
// table, metric, scope and from. A default route with "table: 100" would
// otherwise become the main-table gateway. Strict mode fails; lenient
// mode logs and drops the route.
func checkRoutes(np *Config, strict bool) error {
	check := func(dev string, routes []RouteConfig) ([]RouteConfig, error) {
		kept := routes[:0:0]
		for _, r := range routes {
			if r.Table == 0 && r.Metric == 0 && r.Scope == "" && r.From == "" {
				kept = append(kept, r)
				continue
			}
			if strict {
				return nil, fmt.Errorf("%s route to %s: table, metric, scope and from are not supported by BOOTy", dev, r.To)
			}
			slog.Warn("netplan route skipped (NETPLAN_STRICT=false): table, metric, scope and from are not supported", "device", dev, "to", r.To)
		}
		return kept, nil
	}
	for name := range np.Network.Ethernets {
		eth := np.Network.Ethernets[name]
		routes, err := check(name, eth.Routes)
		if err != nil {
			return err
		}
		eth.Routes = routes
		np.Network.Ethernets[name] = eth
	}
	for name := range np.Network.VLANs {
		vlan := np.Network.VLANs[name]
		routes, err := check(name, vlan.Routes)
		if err != nil {
			return err
		}
		vlan.Routes = routes
		np.Network.VLANs[name] = vlan
	}
	return nil
}

// ParseFile reads a single netplan YAML file.
func ParseFile(path string) (*Config, error) {
	return parseFile(path, true)
}

func parseFile(path string, strict bool) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	cfg, err := parseBytes(path, data, strict)
	if err != nil {
		return nil, err
	}
	if err := checkRoutes(cfg, strict); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ParseBytesWith decodes one netplan document with the checks of a
// netplan file. strict is NETPLAN_STRICT.
func ParseBytesWith(name string, data []byte, strict bool) (*Config, error) {
	cfg, err := parseBytes(name, data, strict)
	if err != nil {
		return nil, err
	}
	if err := checkRoutes(cfg, strict); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return cfg, nil
}

// Check applies the strict route checks of a netplan file to cfg.
func Check(cfg *Config) error { return checkRoutes(cfg, true) }

// parseBytes decodes netplan YAML. Unknown keys are an error, like netplan
// itself, so a setting BOOTy does not implement is never dropped silently.
// Without strict, each unknown key is logged with its line and skipped.
func parseBytes(name string, data []byte, strict bool) (*Config, error) {
	cfg, err := decode(data, true)
	if err == nil {
		return cfg, nil
	}
	var typeErr *yaml.TypeError
	if strict || !errors.As(err, &typeErr) || !onlyUnknownKeys(typeErr) {
		return nil, fmt.Errorf("unmarshal %s: %w", name, err)
	}
	for _, e := range typeErr.Errors {
		slog.Warn("netplan key ignored (NETPLAN_STRICT=false)", "file", name, "error", e)
	}
	cfg, err = decode(data, false)
	if err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", name, err)
	}
	return cfg, nil
}

func decode(data []byte, known bool) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(known)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err //nolint:wrapcheck // the caller names the file
	}
	return &cfg, nil
}

// onlyUnknownKeys reports whether every decode error is an unknown key that
// a lenient decode may drop. A wrong value type stays an error, and so does
// an unknown key under "match" (it would select other NICs) or in a route
// (on-link, type and the like change the route).
func onlyUnknownKeys(e *yaml.TypeError) bool {
	for _, msg := range e.Errors {
		if !strings.Contains(msg, "not found in type") ||
			strings.HasSuffix(msg, "netplan.MatchConfig") || strings.HasSuffix(msg, "netplan.RouteConfig") {
			return false
		}
	}
	return true
}

// mergeConfig merges src into dst at the map-key level.
func mergeConfig(dst, src *Config) {
	if src.Network.Version != 0 {
		dst.Network.Version = src.Network.Version
	}
	dst.Network.Ethernets = mergeMaps(dst.Network.Ethernets, src.Network.Ethernets)
	dst.Network.Bonds = mergeMaps(dst.Network.Bonds, src.Network.Bonds)
	dst.Network.Tunnels = mergeMaps(dst.Network.Tunnels, src.Network.Tunnels)
	dst.Network.Bridges = mergeMaps(dst.Network.Bridges, src.Network.Bridges)
	dst.Network.VLANs = mergeMaps(dst.Network.VLANs, src.Network.VLANs)
	dst.Network.DummyDevices = mergeMaps(dst.Network.DummyDevices, src.Network.DummyDevices)
	dst.Network.VRFs = mergeMaps(dst.Network.VRFs, src.Network.VRFs)
}

// mergeMaps copies all entries from src into dst, creating dst if nil.
func mergeMaps[V any](dst, src map[string]V) map[string]V {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		return maps.Clone(src)
	}
	maps.Copy(dst, src)
	return dst
}

// ToNetworkConfig converts a parsed netplan Config (and optional FRR params)
// into the BOOTy network.Config used by the networking stack.
func ToNetworkConfig(np *Config, frr *FRRParams) *network.Config {
	cfg := &network.Config{}

	extractTunnels(np, cfg)
	extractDummyDevices(np, cfg)
	extractBridges(np, cfg)
	dhcpCount, dnsList := extractEthernets(np, cfg)
	dnsList = extractVLANs(np, cfg, dnsList)
	cfg.DNSResolvers = joinUnique(dnsList)
	extractBonds(np, cfg)
	extractVRFs(np, cfg)
	applyFRRParams(frr, cfg)

	if cfg.ASN > 0 && cfg.ProvisionVNI > 0 {
		cfg.NetworkMode = "gobgp"
	}
	if dhcpCount > 0 && cfg.ASN == 0 && cfg.ProvisionVNI == 0 {
		slog.Info("netplan: DHCP interface(s) found, no EVPN config, using DHCP mode", "dhcpCount", dhcpCount)
	}
	return cfg
}

func extractTunnels(np *Config, cfg *network.Config) {
	names := slices.Sorted(maps.Keys(np.Network.Tunnels))

	// The provision tunnel is the first VXLAN tunnel that carries a VNI.
	// VNI, local underlay address and underlay link all come from that one
	// tunnel, so the VXLAN device never binds a link of a different tunnel.
	var provision TunnelConfig
	provisionName := ""
	found := false
	for _, name := range names {
		t := np.Network.Tunnels[name]
		if !strings.EqualFold(t.Mode, "vxlan") || t.ID <= 0 {
			continue
		}
		provision, provisionName = t, name
		found = true
		break
	}

	if found {
		if cfg.VXLANName == "" {
			cfg.VXLANName = provisionName
		}
		if cfg.ProvisionVNI == 0 {
			cfg.ProvisionVNI = uint32(provision.ID)
		}
		if provision.Local != "" && cfg.UnderlayIP == "" {
			cfg.UnderlayIP = provision.Local
		}
		if provision.Link != "" && cfg.VXLANLink == "" {
			cfg.VXLANLink = ethernetName(np, provision.Link)
		}
		applyTunnelDevice(&provision, &cfg.OverlayDevices)
		return
	}

	// No tunnel carries a VNI, so no provision VXLAN device exists.
	// Keep the underlay address hint only.
	for _, name := range names {
		t := np.Network.Tunnels[name]
		if !strings.EqualFold(t.Mode, "vxlan") {
			continue
		}
		if t.Local != "" && cfg.UnderlayIP == "" {
			cfg.UnderlayIP = t.Local
		}
	}
}

func extractDummyDevices(np *Config, cfg *network.Config) {
	for _, name := range slices.Sorted(maps.Keys(np.Network.DummyDevices)) {
		d := np.Network.DummyDevices[name]
		for _, addr := range d.Addresses {
			if strings.HasSuffix(addr, "/32") {
				ip := strings.TrimSuffix(addr, "/32")
				if cfg.UnderlayIP == "" {
					cfg.UnderlayIP = ip
				}
				if cfg.UnderlayDummyName == "" && cfg.UnderlayIP == ip {
					cfg.UnderlayDummyName = name
				}
			}
		}
	}
}

func extractBridges(np *Config, cfg *network.Config) {
	names := slices.Sorted(maps.Keys(np.Network.Bridges))
	for _, name := range names {
		br := np.Network.Bridges[name]
		hasVXLAN := false
		for _, member := range br.Interfaces {
			if _, ok := np.Network.Tunnels[member]; ok {
				hasVXLAN = true
				break
			}
		}
		if !hasVXLAN {
			continue
		}
		// The provision bridge is the first bridge with a VXLAN port. The
		// bm4x L3VNI bridge has no address; it still names the bridge.
		if cfg.BridgeName == "" {
			cfg.BridgeName = name
		}
		if len(br.Addresses) > 0 && cfg.ProvisionIP == "" {
			cfg.BridgeName = name
			cfg.ProvisionIP = br.Addresses[0]
			break
		}
	}

	// The router MAC must belong to the bridge that BOOTy creates.
	if cfg.BridgeMAC == "" {
		cfg.BridgeMAC = np.Network.Bridges[cfg.BridgeNameOrDefault()].MACAddress
	}
	if br, ok := np.Network.Bridges[cfg.BridgeNameOrDefault()]; ok {
		cfg.OverlayDevices.BridgeMTU = br.MTU
		cfg.OverlayDevices.BridgeLinkLocal = linkLocalIPv6(br.LinkLocal)
		cfg.OverlayDevices.BridgeAcceptRA = br.AcceptRA
		cfg.OverlayDevices.BridgeSTP = bridgeSTP(br.Parameters)
	}
}

// bridgeSTP is the spanning tree of a netplan bridge: like netplan, a
// parameters block turns it on unless it says "stp: false"; without the
// block networkd keeps the kernel default (off), so nil.
func bridgeSTP(p *BridgeParams) *bool {
	if p == nil {
		return nil
	}
	on := p.STP == nil || *p.STP
	return &on
}

func extractEthernets(np *Config, cfg *network.Config) (dhcpCount int, dnsList []string) {
	for _, name := range slices.Sorted(maps.Keys(np.Network.Ethernets)) {
		eth := np.Network.Ethernets[name]
		if eth.DHCP4 != nil && *eth.DHCP4 {
			dhcpCount++
		}
		if slices.ContainsFunc(eth.LinkLocal, isIPv6) {
			cfg.Interfaces = append(cfg.Interfaces, "auto-detect")
		}
		if eth.Nameservers != nil {
			dnsList = append(dnsList, eth.Nameservers.Addresses...)
		}
		applyEthernetFlags(np, name, &eth, cfg)
		if name == "lo" && eth.Match == nil {
			// Addresses on lo are host addresses, not a static uplink.
			cfg.LoopbackAddrs = append(cfg.LoopbackAddrs, eth.Addresses...)
			continue
		}
		applyEthernetMatch(name, &eth, cfg)
		if eth.Match == nil {
			// Without a match block the ethernet ID is the interface name.
			cfg.InterfaceNames = append(cfg.InterfaceNames, name)
		}
		for _, r := range eth.Routes {
			if r.To == "default" && r.Via != "" && cfg.StaticGateway == "" {
				cfg.StaticGateway = r.Via
				slog.Debug("netplan: default gateway from ethernet routes", "iface", name, "gw", r.Via)
			}
		}
		if eth.MTU > cfg.MTU {
			cfg.MTU = eth.MTU
		}
	}
	return dhcpCount, dnsList
}

func extractVLANs(np *Config, cfg *network.Config, dnsList []string) []string {
	for _, vlanName := range slices.Sorted(maps.Keys(np.Network.VLANs)) {
		v := np.Network.VLANs[vlanName]
		vc := network.VLANConfig{ID: v.ID, Parent: ethernetName(np, v.Link)}
		if len(v.Addresses) > 0 {
			vc.Address = v.Addresses[0]
		}
		for _, r := range v.Routes {
			if r.To == "default" && r.Via != "" {
				vc.Gateway = r.Via
			}
		}
		cfg.VLANs = append(cfg.VLANs, vc)
		if v.Nameservers != nil {
			dnsList = append(dnsList, v.Nameservers.Addresses...)
		}
		slog.Debug("netplan: found VLAN", "name", vlanName, "id", v.ID, "link", v.Link)
	}
	return dnsList
}

func extractBonds(np *Config, cfg *network.Config) {
	names := slices.Sorted(maps.Keys(np.Network.Bonds))
	for _, name := range names {
		bond := np.Network.Bonds[name]
		if len(bond.Interfaces) == 0 {
			continue
		}
		members := make([]string, len(bond.Interfaces))
		for i, m := range bond.Interfaces {
			members[i] = ethernetName(np, m)
		}
		cfg.BondInterfaces = strings.Join(members, ",")
		if bond.Parameters != nil && bond.Parameters.Mode != "" {
			cfg.BondMode = bond.Parameters.Mode
		}
		if addr := firstAddress(bond.Addresses); addr != "" {
			cfg.StaticIP = addr
			cfg.StaticIface = "bond0"
		}
		if bond.MTU > cfg.MTU {
			cfg.MTU = bond.MTU
		}
		slog.Debug("netplan: found bond", "name", name, "members", bond.Interfaces)
		break
	}
}

func extractVRFs(np *Config, cfg *network.Config) {
	if cfg.BridgeName != "" && len(np.Network.VRFs) > 0 {
		cfg.OverlayVRFSet = true
	}

	for _, name := range slices.Sorted(maps.Keys(np.Network.VRFs)) {
		vrf := np.Network.VRFs[name]
		if cfg.VRFTableID == 0 && vrf.Table > 0 {
			cfg.VRFTableID = uint32(vrf.Table)
			cfg.VRFName = name
		}
		if cfg.BridgeName != "" && slices.Contains(vrf.Interfaces, cfg.BridgeName) {
			cfg.OverlayVRFName = name
			if vrf.Table > 0 {
				cfg.OverlayVRFTableID = uint32(vrf.Table)
			}
		}
	}
}

func firstAddress(addresses []string) string {
	for _, addr := range addresses {
		if trimmed := strings.TrimSpace(addr); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ethernetName returns the kernel name of a netplan ethernet ID. Bonds,
// VLANs and tunnels refer to an ethernet by its ID; with set-name the NIC
// is renamed before BOOTy uses it, like netplan does.
func ethernetName(np *Config, id string) string {
	if eth, ok := np.Network.Ethernets[id]; ok && eth.Match != nil && eth.SetName != "" {
		return eth.SetName
	}
	return id
}

// applyEthernetFlags takes accept-ra: false and emit-lldp of an ethernet,
// under the name the NIC has after set-name.
func applyEthernetFlags(np *Config, id string, eth *EthernetConfig, cfg *network.Config) {
	if eth.AcceptRA != nil && !*eth.AcceptRA {
		cfg.NoAcceptRA = append(cfg.NoAcceptRA, ethernetName(np, id))
	}
	if eth.EmitLLDP != nil && *eth.EmitLLDP {
		if eth.Match != nil && eth.SetName == "" {
			// The ID of a match block is no interface name; networkd sends on
			// every NIC the block selects.
			cfg.EmitLLDPMatches = append(cfg.EmitLLDPMatches, network.LinkMatch{Name: eth.Match.Name, MAC: eth.Match.MAC})
		} else {
			cfg.EmitLLDP = append(cfg.EmitLLDP, ethernetName(np, id))
		}
	}
}

// applyTunnelDevice takes the device settings of the provision tunnel.
func applyTunnelDevice(t *TunnelConfig, d *network.OverlayDevices) {
	d.VXLANMTU = t.MTU
	d.VXLANMAC = t.MACAddress
	d.VXLANLearning = t.MACLearning
	d.VXLANARPProxy = t.ARPProxy
	d.PortHairpin = t.Hairpin
	d.PortLearning = t.PortMACLearning
	d.VXLANLinkLocal = linkLocalIPv6(t.LinkLocal)
	d.VXLANAcceptRA = t.AcceptRA
}

// linkLocalIPv6 reports whether a netplan link-local list asks for an IPv6
// link-local address; nil when the key is not set (networkd default).
func linkLocalIPv6(list []string) *bool {
	if list == nil {
		return nil
	}
	v := slices.ContainsFunc(list, isIPv6)
	return &v
}

func isIPv6(linkLocal string) bool { return strings.EqualFold(linkLocal, "ipv6") }

// applyEthernetMatch records netplan set-name renames and the first static
// address with the NIC or match it belongs to.
func applyEthernetMatch(name string, eth *EthernetConfig, cfg *network.Config) {
	if eth.Match != nil && eth.SetName != "" {
		cfg.LinkRenames = append(cfg.LinkRenames, network.LinkMatch{Name: eth.Match.Name, MAC: eth.Match.MAC, SetName: eth.SetName})
	}
	if cfg.StaticIP != "" {
		return
	}
	addr := firstAddress(eth.Addresses)
	if addr == "" {
		return
	}
	cfg.StaticIP = addr
	cfg.StaticIface = ethernetStaticIface(name, eth)
	if cfg.StaticIface == "" && eth.Match != nil {
		cfg.StaticMatch = &network.LinkMatch{Name: eth.Match.Name, MAC: eth.Match.MAC}
	}
}

func ethernetStaticIface(name string, eth *EthernetConfig) string {
	if eth.Match == nil {
		return name
	}
	if eth.SetName != "" {
		return eth.SetName
	}
	// A glob or a MAC must be resolved against the NICs; a literal name
	// alone names the NIC.
	if eth.Match.Name == "" || eth.Match.MAC != "" || strings.ContainsAny(eth.Match.Name, "*?[]") {
		return ""
	}
	return eth.Match.Name
}

func applyFRRParams(frr *FRRParams, cfg *network.Config) {
	if frr == nil {
		return
	}
	if frr.ASN > 0 {
		cfg.ASN = frr.ASN
	}
	if frr.LocalASN > 0 && cfg.LocalASN == 0 {
		cfg.LocalASN = frr.LocalASN
	}
	if frr.RouterID != "" && cfg.UnderlayIP == "" {
		cfg.UnderlayIP = frr.RouterID
	}
	cfg.EVPNL3VNIs = frr.L3VNIs
	cfg.BlackholeRoutes = frr.BlackholeRoutes
	if frr.Model != nil {
		cfg.BGPDefaults = string(frr.Model.Defaults)
		cfg.BGPFRRVersion = frr.Model.Version
		// Only an explicit "[no] ip[v6] forwarding" changes the GoBGP
		// defaults: IPv4 on, IPv6 off like the kernel default that zebra
		// keeps.
		cfg.IPForwarding = frr.Model.IPForwarding
		cfg.IPv6Forwarding = frr.Model.IPv6Forwarding
	}
	cfg.BGPSessions = frr.Sessions
	cfg.BGPMultipathRelax = frr.MultipathRelax
	if frr.Model != nil {
		cfg.BGPRouteMaps, cfg.BGPCommunityLists = policy(frr.Model)
	}
	applyEVPNMode(frr, cfg)
	cfg.InterfaceNames = append(cfg.InterfaceNames, frr.UnnumberedPeers...)
	hasUnnumbered := len(frr.UnnumberedPeers) > 0
	hasNumbered := len(frr.NumberedPeers) > 0

	switch {
	case hasUnnumbered && hasNumbered:
		cfg.BGPPeerMode = network.PeerModeDual
		cfg.BGPNeighbors = strings.Join(frr.NumberedPeers, ",")
	case hasUnnumbered:
		cfg.BGPPeerMode = network.PeerModeUnnumbered
	case hasNumbered:
		cfg.BGPPeerMode = network.PeerModeNumbered
		cfg.BGPNeighbors = strings.Join(frr.NumberedPeers, ",")
	}
}

// applyEVPNMode selects the EVPN mode of the provision VNI. An L3VNI gets
// only Type-5 routes, like FRR (advertise-all-vni yields Type-2/Type-3
// routes for L2VNIs only), and its "network" lines are originated.
func applyEVPNMode(frr *FRRParams, cfg *network.Config) {
	cfg.EVPNResolveOverlayIndex = frr.ResolveOverlayIndex
	if cfg.ProvisionVNI != 0 && slices.Contains(frr.L3VNIs, cfg.ProvisionVNI) {
		cfg.EVPNType5Only = true
		cfg.Type5Networks = type5Networks(frr.Model, cfg.ProvisionVNI)
		if frr.Model != nil {
			cfg.EVPNImportRTs, cfg.EVPNExportRTs = l3vniRouteTargets(frr.Model, cfg.ProvisionVNI)
		}
		return
	}
	cfg.EVPNL2Enabled = frr.AdvertiseAllVNI
}

// joinUnique deduplicates a string slice and joins with commas.
func joinUnique(items []string) string {
	seen := make(map[string]bool, len(items))
	var result []string
	for _, s := range items {
		if s != "" && !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return strings.Join(result, ",")
}

// HasNetplanFiles reports whether dir contains .yaml or .yml files. A
// missing dir has none; any other read error is returned.
func HasNetplanFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read netplan dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			return true, nil
		}
	}
	return false, nil
}
