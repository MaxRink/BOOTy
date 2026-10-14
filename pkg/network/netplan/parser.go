package netplan

import (
	"errors"
	"fmt"
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
// top-level map key level (ethernets, tunnels, bridges, etc.).
func ParseDir(dir string) (*Config, error) {
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
		cfg, err := ParseFile(f)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		mergeConfig(merged, cfg)
	}
	return merged, nil
}

// ParseFile reads a single netplan YAML file.
func ParseFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return &cfg, nil
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
	found := false
	for _, name := range names {
		t := np.Network.Tunnels[name]
		if !strings.EqualFold(t.Mode, "vxlan") || t.ID <= 0 {
			continue
		}
		provision = t
		found = true
		break
	}

	if found {
		if cfg.ProvisionVNI == 0 {
			cfg.ProvisionVNI = uint32(provision.ID)
		}
		if provision.Local != "" && cfg.UnderlayIP == "" {
			cfg.UnderlayIP = provision.Local
		}
		if provision.Link != "" && cfg.VXLANLink == "" {
			cfg.VXLANLink = ethernetName(np, provision.Link)
		}
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
		if len(br.Addresses) == 0 {
			continue
		}
		if hasVXLAN && cfg.ProvisionIP == "" {
			cfg.BridgeName = name
			cfg.ProvisionIP = br.Addresses[0]
			break
		}
	}

	// The router MAC must belong to the bridge that BOOTy creates.
	if cfg.BridgeMAC == "" {
		cfg.BridgeMAC = np.Network.Bridges[cfg.BridgeNameOrDefault()].MACAddress
	}
}

func extractEthernets(np *Config, cfg *network.Config) (dhcpCount int, dnsList []string) {
	for _, name := range slices.Sorted(maps.Keys(np.Network.Ethernets)) {
		eth := np.Network.Ethernets[name]
		if eth.DHCP4 != nil && *eth.DHCP4 {
			dhcpCount++
		}
		for _, ll := range eth.LinkLocal {
			if strings.EqualFold(ll, "ipv6") {
				cfg.Interfaces = append(cfg.Interfaces, "auto-detect")
				break
			}
		}
		if eth.Nameservers != nil {
			dnsList = append(dnsList, eth.Nameservers.Addresses...)
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
	if frr.AdvertiseAllVNI {
		// advertise-all-vni only yields Type-2/Type-3 routes for L2VNIs. When
		// the provisioning VNI is an L3VNI the fabric is pure Type-5, as with
		// the FRR-based legacy deployer, so BOOTy must not originate MAC/IP or
		// IMET routes for it.
		if cfg.ProvisionVNI != 0 && slices.Contains(frr.L3VNIs, cfg.ProvisionVNI) {
			slog.Info("netplan: provision VNI is an FRR L3VNI, using Type-5 only", "vni", cfg.ProvisionVNI)
		} else {
			cfg.EVPNL2Enabled = true
		}
	}
	cfg.InterfaceNames = append(cfg.InterfaceNames, frr.UnnumberedPeers...)
	hasUnnumbered := len(frr.UnnumberedPeers) > 0
	hasNumbered := len(frr.NumberedPeers) > 0

	switch {
	case hasUnnumbered && hasNumbered:
		cfg.BGPPeerMode = network.PeerModeDual
		cfg.BGPNeighbors = strings.Join(frr.NumberedPeers, ",")
		cfg.BGPRemoteASN = frr.RemoteASN
	case hasUnnumbered:
		cfg.BGPPeerMode = network.PeerModeUnnumbered
	case hasNumbered:
		cfg.BGPPeerMode = network.PeerModeNumbered
		cfg.BGPNeighbors = strings.Join(frr.NumberedPeers, ",")
		cfg.BGPRemoteASN = frr.RemoteASN
	}
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
