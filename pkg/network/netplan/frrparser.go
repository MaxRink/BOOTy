package netplan

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// FRRParams holds parameters extracted from an FRR configuration file.
type FRRParams struct {
	ASN             uint32
	LocalASN        uint32 // local-as override (BM4X uses per-neighbor local-as).
	RouterID        string
	UnnumberedPeers []string // Interface names from "neighbor X interface" lines.
	NumberedPeers   []string // IPv4 addresses from "neighbor X remote-as" lines.
	NumberedPeersV6 []string // IPv6 addresses from "neighbor X remote-as" lines.
	EVPN            bool     // True if "address-family l2vpn evpn" found.
	AdvertiseAllVNI bool     // True if "advertise-all-vni" found.
	// L3VNIs lists VNIs mapped to a VRF ("vni N" at top level for the default
	// VRF or inside a "vrf" block). FRR advertises only Type-5 routes for them;
	// advertise-all-vni produces Type-2/Type-3 routes for L2VNIs only.
	L3VNIs []uint32
}

// Regex patterns for FRR config extraction.
var (
	reRouterBGP       = regexp.MustCompile(`(?m)^router\s+bgp\s+(\d+)`)
	reRouterID        = regexp.MustCompile(`(?m)^\s+bgp\s+router-id\s+(\S+)`)
	reNeighborIface   = regexp.MustCompile(`(?m)^\s+neighbor\s+(\S+)\s+interface\s+(?:remote-as|peer-group)`)
	reNeighborIPv4    = regexp.MustCompile(`(?m)^\s+neighbor\s+(\d+\.\d+\.\d+\.\d+)\s+remote-as\s+(\S+)`)
	reNeighborIPv6    = regexp.MustCompile(`(?m)^\s+neighbor\s+([0-9a-fA-F]*:[0-9a-fA-F:]+)\s+remote-as\s+(\S+)`)
	reL2VPNEVPN       = regexp.MustCompile(`(?m)^\s+address-family\s+l2vpn\s+evpn`)
	reAdvertiseAllVNI = regexp.MustCompile(`(?m)^\s+advertise-all-vni`)
	reLocalAS         = regexp.MustCompile(`(?m)^\s+neighbor\s+\S+\s+local-as\s+(\d+)`)
)

// ParseFRRConfig extracts networking parameters from an FRR configuration file.
func ParseFRRConfig(path string) (*FRRParams, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read frr config %s: %w", path, err)
	}
	return ParseFRRConfigBytes(data)
}

// ParseFRRConfigBytes extracts networking parameters from FRR config content.
func ParseFRRConfigBytes(data []byte) (*FRRParams, error) {
	content := string(data)
	params := &FRRParams{}

	if m := reRouterBGP.FindStringSubmatch(content); len(m) > 1 {
		asn, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse ASN %q: %w", m[1], err)
		}
		params.ASN = uint32(asn)
	}
	if m := reRouterID.FindStringSubmatch(content); len(m) > 1 {
		params.RouterID = m[1]
	}

	params.UnnumberedPeers = extractUnnumberedPeers(content)
	params.NumberedPeers = extractNumberedPeers(content, reNeighborIPv4, "169.254.", "127.")
	params.NumberedPeersV6 = extractNumberedPeers(content, reNeighborIPv6, "fd00:7:caa5:", "fe80:")

	if m := reLocalAS.FindStringSubmatch(content); len(m) > 1 {
		if localASN, err := strconv.ParseUint(m[1], 10, 32); err == nil {
			params.LocalASN = uint32(localASN)
		}
	}

	params.EVPN = reL2VPNEVPN.MatchString(content)
	params.AdvertiseAllVNI = reAdvertiseAllVNI.MatchString(content)
	params.L3VNIs = extractL3VNIs(content)
	return params, nil
}

// extractL3VNIs returns VNIs that FRR maps to a VRF: a "vni N" statement at
// the top level (default VRF) or inside a "vrf NAME" block, with or without
// "prefix-routes-only". A "vni N" under any other block, such as the L2VNI
// block under "router bgp", is not an L3VNI and is not included.
func extractL3VNIs(content string) []uint32 {
	var vnis []uint32
	block := frrBlockNone
	for line := range strings.Lines(content) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		topLevel := line[0] != ' ' && line[0] != '\t'
		if vni, ok := parseL3VNI(fields); ok && (block == frrBlockVRF || topLevel && block == frrBlockNone) {
			if !slices.Contains(vnis, vni) {
				vnis = append(vnis, vni)
			}
			continue
		}
		if topLevel {
			block = nextFRRBlock(block, fields[0])
		}
	}
	return vnis
}

// frrBlock is the kind of top-level FRR block a config line belongs to.
type frrBlock int

const (
	frrBlockNone frrBlock = iota
	frrBlockVRF
	frrBlockOther
)

// parseL3VNI parses "vni N" or "vni N prefix-routes-only".
func parseL3VNI(fields []string) (uint32, bool) {
	if fields[0] != "vni" || (len(fields) != 2 && (len(fields) != 3 || fields[2] != "prefix-routes-only")) {
		return 0, false
	}
	vni, err := strconv.ParseUint(fields[1], 10, 32)
	return uint32(vni), err == nil
}

// nextFRRBlock returns the block that a top-level line starting with keyword
// opens or closes.
func nextFRRBlock(current frrBlock, keyword string) frrBlock {
	switch {
	case keyword == "vrf":
		return frrBlockVRF
	case keyword == "!" || keyword == "exit" || keyword == "exit-vrf" || keyword == "end":
		return frrBlockNone
	case slices.Contains(frrBlockKeywords, keyword):
		return frrBlockOther
	}
	return current
}

// frrBlockKeywords lists top-level FRR statements that open a block whose
// lines may be written without indentation. A "vni N" inside them is not an
// L3VNI. Single-line top-level statements do not open a block.
var frrBlockKeywords = []string{"router", "interface", "route-map", "line", "segment-routing", "mpls", "bfd", "key", "nexthop-group", "pbr-map"}

// extractUnnumberedPeers returns deduplicated interface names from neighbor lines.
func extractUnnumberedPeers(content string) []string {
	seen := make(map[string]bool)
	var peers []string
	for _, m := range reNeighborIface.FindAllStringSubmatch(content, -1) {
		iface := m[1]
		if !seen[iface] {
			peers = append(peers, iface)
			seen[iface] = true
		}
	}
	return peers
}

// extractNumberedPeers returns deduplicated IPs from neighbor lines, skipping
// addresses that start with any of the given prefixes.
func extractNumberedPeers(content string, re *regexp.Regexp, skipPrefixes ...string) []string {
	seen := make(map[string]bool)
	var peers []string
	for _, m := range re.FindAllStringSubmatch(content, -1) {
		ip := m[1]
		if shouldSkipPeer(ip, skipPrefixes) || seen[ip] {
			continue
		}
		peers = append(peers, ip)
		seen[ip] = true
	}
	return peers
}

func shouldSkipPeer(ip string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(ip, p) {
			return true
		}
	}
	return false
}
