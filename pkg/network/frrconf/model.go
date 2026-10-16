// Package frrconf parses the FRR configuration that CAPRF provisioners hand
// to the ramdisk into a typed model. It follows FRR's own reading rules
// (leading blanks do not matter, "!" starts a comment, a statement that does
// not fit the current node is retried in the parent nodes) and accepts only
// the statements that BOOTy implements. Any other statement is an error, so
// a configuration is never applied partly.
//
// Model names follow das-schiff-network-operator (pkg/config/baseconfig.go
// Neighbor, api/v1alpha1 Filter/Matcher/Action) where the concepts match.
package frrconf

// Profile is the "frr defaults" profile. It selects default timers and
// whether eBGP needs a policy (FRR bgpd/bgp_vty.c, doc/user/bgp.rst).
type Profile string

// FRR default profiles.
const (
	ProfileTraditional Profile = "traditional"
	ProfileDatacenter  Profile = "datacenter"
)

// Config is a parsed FRR configuration.
type Config struct {
	// Version is the "frr version" line. FRR selects version dependent
	// defaults by it.
	Version string
	// Defaults is the "frr defaults" profile. Empty means the line is
	// missing; then the daemon profile applies.
	Defaults Profile
	Hostname string
	// IPForwarding and IPv6Forwarding are nil unless the config sets them.
	IPForwarding   *bool
	IPv6Forwarding *bool

	// VRFVNIs maps VRFs to L3VNIs: a top-level "vni N" maps the default
	// VRF, a "vrf NAME" block maps NAME.
	VRFVNIs []VRFVNI

	StaticRoutes   []StaticRoute
	Routers        []Router
	RouteMaps      []RouteMap
	CommunityLists []CommunityList
	BFDProfiles    []BFDProfile
}

// VRFVNI maps a VRF to its L3VNI.
type VRFVNI struct {
	VRF              string // "" for the default VRF
	VNI              uint32
	PrefixRoutesOnly bool
	Line             int
}

// StaticRoute is an "ip route" or "ipv6 route" statement. Only blackhole
// routes are supported.
type StaticRoute struct {
	Prefix    string
	Blackhole bool
	Line      int
}

// Router is a "router bgp ASN [vrf NAME]" block.
type Router struct {
	ASN uint32
	// VRF is "" for "router bgp ASN" and "default" for an explicit
	// "vrf default". Both mean the default VRF.
	VRF      string
	RouterID string
	// The flags are nil unless the config sets them, so the profile
	// default applies.
	EBGPRequiresPolicy *bool
	SuppressDuplicates *bool
	DefaultIPv4Unicast *bool
	MultipathRelax     bool
	// FastExternalFailover is "[no] bgp fast-external-failover"; nil is
	// the FRR default (on).
	FastExternalFailover *bool

	// Neighbors holds peer-groups and peers in config order.
	Neighbors       []Neighbor
	AddressFamilies []AddressFamily
	Line            int
}

// RemoteASKind says how the remote AS was given.
type RemoteASKind string

// Remote AS kinds.
const (
	RemoteASNumber   RemoteASKind = "number"
	RemoteASInternal RemoteASKind = "internal"
	RemoteASExternal RemoteASKind = "external"
)

// RemoteAS is the "remote-as" of a neighbor or peer-group.
type RemoteAS struct {
	Kind   RemoteASKind
	Number uint32
}

// LocalAS is "local-as N [no-prepend [replace-as]]".
type LocalAS struct {
	ASN       uint32
	NoPrepend bool
	ReplaceAS bool
}

// Neighbor is a peer-group ("neighbor NAME peer-group") or a peer: an
// address or, with Interface, an unnumbered interface peer.
type Neighbor struct {
	Name      string
	PeerGroup bool   // this entry defines a peer-group
	Interface bool   // "neighbor IFACE interface ..."
	Group     string // the peer-group this peer is a member of

	// Attributes set on this entry. nil or zero means "not set here"; the
	// peer-group value or the profile default applies.
	RemoteAS      *RemoteAS
	LocalAS       *LocalAS
	BFD           bool
	BFDProfile    string
	KeepaliveTime *uint32 // seconds
	HoldTime      *uint32 // seconds
	ConnectRetry  *uint32 // seconds
	Line          int
}

// AddressFamily is an "address-family" block.
type AddressFamily struct {
	AFI  string // "ipv4", "ipv6" or "l2vpn"
	SAFI string // "unicast" or "evpn"

	Networks              []string
	RedistributeConnected bool
	Neighbors             []AFNeighbor

	// l2vpn evpn only.
	AdvertiseAllVNI      bool
	AdvertiseDefaultGW   bool
	AdvertiseIPv4Unicast bool
	AdvertiseIPv6Unicast bool
	RouteTargetImport    []string
	RouteTargetExport    []string
	// L2VNIs are "vni N" blocks with an explicit RD or route targets.
	L2VNIs []L2VNI
	Line   int
}

// L2VNI is a "vni N" block in "address-family l2vpn evpn".
type L2VNI struct {
	VNI               uint32
	RD                string
	RouteTargetImport []string
	RouteTargetExport []string
	// AdvertiseDefaultGW is "advertise-default-gw" in the vni block.
	AdvertiseDefaultGW bool
	Line               int
}

// AFNeighbor holds the per address family settings of a neighbor or
// peer-group.
type AFNeighbor struct {
	Name                string
	Activate            bool
	SoftReconfigInbound bool
	// AllowASIn is the "allowas-in" occurrence count, 0 when unset. FRR uses
	// 3 when the statement has no number (bgpd/bgp_vty.c).
	AllowASIn   int
	RouteMapIn  string
	RouteMapOut string
}

// RouteMapAction is permit or deny.
type RouteMapAction string

// Route map actions.
const (
	Permit RouteMapAction = "permit"
	Deny   RouteMapAction = "deny"
)

// RouteMap is the ordered list of entries of one route-map.
type RouteMap struct {
	Name    string
	Entries []RouteMapEntry
}

// RouteMapEntry is one "route-map NAME permit|deny SEQ" block.
type RouteMapEntry struct {
	Seq    int
	Action RouteMapAction
	// MatchCommunity names a community-list; empty matches every route.
	MatchCommunity string
	// SetLocalPreference is nil unless the entry sets it.
	SetLocalPreference *uint32
	// SetCommunities are the communities of "set community"; with
	// SetCommunityAdditive they are added, otherwise they replace.
	SetCommunities       []string
	SetCommunityAdditive bool
	Line                 int
}

// CommunityList is a standard community-list.
type CommunityList struct {
	Name    string
	Entries []CommunityListEntry
}

// CommunityListEntry is one "bgp community-list standard" line.
type CommunityListEntry struct {
	Action      RouteMapAction
	Communities []string
	Line        int
}

// BFDProfile is a "bfd / profile NAME" block. nil values are not set.
type BFDProfile struct {
	Name             string
	TransmitInterval *uint32 // milliseconds
	ReceiveInterval  *uint32 // milliseconds
	DetectMultiplier *uint32
	Line             int
}
