package gobgp

import (
	"cmp"
	"fmt"

	apipb "github.com/osrg/gobgp/v4/api"

	"github.com/telekom/BOOTy/pkg/network"
)

// idleHoldAfterReset is the time in seconds a peer waits in Idle after
// ResetPeer (BFD down) before it connects again; the GoBGP default is
// 30 s. For a BFD reset of an Established session the peer restarter
// bypasses it: it deletes and adds the peer at once when the session has
// ended (restart.go). FRR waits BGP_INIT_START_TIMER (1 s) after a TCP
// close and 2 s after a NOTIFICATION, where v_start doubles (bgp_fsm.c,
// bgp_packet.c).
//
// BGP_CONNECT_AT_ONCE sets ConnectAtOnce (GoBGP fork) on every peer: FRR
// connects as soon as BGP_Start runs (bgp_fsm.c, bgp_start), and BGP_Start
// comes from the start timer, from a valid next hop (bgp_fsm_nht_update)
// and from BFD Up. Without it GoBGP waits a random 1.5 to 2 s before the
// first connect after Idle (osrg/gobgp#3736); retries wait the connect
// timer either way. It is off by default: after a carrier loss GoBGP and
// the leaf then both have a connect pending until the link works again,
// and the connection collision is not always resolved to one session
// (#811).
const idleHoldAfterReset = 1

// bgpTimers returns the configured BGP keepalive/hold timers.
func bgpTimers(cfg *Config) *apipb.Timers {
	return &apipb.Timers{
		Config: &apipb.TimersConfig{
			ConnectRetry:           cfg.ConnectRetry,
			KeepaliveInterval:      cfg.KeepaliveInterval,
			HoldTime:               cfg.HoldTime,
			IdleHoldTimeAfterReset: idleHoldAfterReset,
			ConnectAtOnce:          cfg.ConnectAtOnce,
		},
	}
}

// buildNumberedPeer constructs a GoBGP peer config for a numbered (IP-based) session.
func buildNumberedPeer(cfg *Config, addr string, families []*apipb.AfiSafi) *apipb.Peer {
	remoteASN := cfg.RemoteASN
	if remoteASN == 0 {
		remoteASN = cfg.ASN // iBGP
	}

	peer := &apipb.Peer{
		Conf: &apipb.PeerConf{
			NeighborAddress: addr,
			PeerAsn:         remoteASN,
			AllowOwnAsn:     cfg.AllowASIn,
		},
		Timers:   bgpTimers(cfg),
		AfiSafis: families,
		Transport: &apipb.Transport{
			MtuDiscovery: true,
			// SO_BINDTODEVICE on the VRF device, like FRR "router bgp N
			// vrf NAME".
			BindInterface: cfg.NeighborVRF,
		},
	}

	if cfg.AuthPassword != "" {
		peer.Conf.AuthPassword = cfg.AuthPassword
	}
	setCapabilities(peer, &cfg.Capabilities)
	return peer
}

// buildInterfacePeer constructs a GoBGP peer config for an unnumbered (link-local) session.
func buildInterfacePeer(cfg *Config, iface, addr string, families []*apipb.AfiSafi) *apipb.Peer {
	peer := &apipb.Peer{
		Conf: &apipb.PeerConf{
			NeighborAddress: addr,
			PeerAsn:         0, // External peer, ASN learned via open
			// GoBGP local-as is FRR "local-as N no-prepend replace-as".
			LocalAsn:    cfg.LocalAS,
			AllowOwnAsn: cfg.AllowASIn,
		},
		Timers:   bgpTimers(cfg),
		AfiSafis: families,
		Transport: &apipb.Transport{
			MtuDiscovery:  true,
			LocalAddress:  "::",
			BindInterface: iface,
			RemoteAddress: addr,
		},
	}

	if cfg.AuthPassword != "" {
		peer.Conf.AuthPassword = cfg.AuthPassword
	}
	setCapabilities(peer, &cfg.Capabilities)
	return peer
}

// allFamilies returns IPv4 unicast + L2VPN-EVPN address families.
func allFamilies() []*apipb.AfiSafi {
	return []*apipb.AfiSafi{
		{Config: &apipb.AfiSafiConfig{
			Family: &apipb.Family{Afi: apipb.Family_AFI_IP, Safi: apipb.Family_SAFI_UNICAST},
		}},
		{Config: &apipb.AfiSafiConfig{
			Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		}},
	}
}

// evpnFamilies returns only the L2VPN-EVPN address family.
func evpnFamilies() []*apipb.AfiSafi {
	return []*apipb.AfiSafi{
		{Config: &apipb.AfiSafiConfig{
			Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		}},
	}
}

// peerModeFamilies returns the families of the unnumbered and of the
// numbered peers of a BGP_PEER_MODE; nil means no such peers. In dual mode
// the numbered peers (the route reflectors) get only L2VPN-EVPN, like the
// bm4x template ("no bgp default ipv4-unicast", only "neighbor dcgw
// activate" in l2vpn evpn); IPv4 unicast runs on the unnumbered peers.
func peerModeFamilies(mode network.PeerMode) (iface, numbered func() []*apipb.AfiSafi, err error) {
	switch mode {
	case network.PeerModeUnnumbered:
		return allFamilies, nil, nil
	case network.PeerModeDual:
		return ipv4Families, evpnFamilies, nil
	case network.PeerModeNumbered:
		return nil, allFamilies, nil
	}
	return nil, nil, fmt.Errorf("unsupported peer mode: %q", mode)
}

// ipv4Families returns only IPv4 unicast address family.
func ipv4Families() []*apipb.AfiSafi {
	return []*apipb.AfiSafi{
		{Config: &apipb.AfiSafiConfig{
			Family: &apipb.Family{Afi: apipb.Family_AFI_IP, Safi: apipb.Family_SAFI_UNICAST},
		}},
	}
}

// buildSessionPeer constructs a GoBGP peer from an FRR session. addr is
// the peer address; for an unnumbered session it is the discovered
// link-local address.
func buildSessionPeer(cfg *Config, s *network.BGPSession, addr string) *apipb.Peer {
	// iBGP peers are in the effective local AS: the local-as when set,
	// else the router AS.
	peerASN := s.RemoteASN
	if s.Internal && peerASN == 0 {
		peerASN = cmp.Or(s.LocalASN, cfg.ASN)
	}
	peer := &apipb.Peer{
		Conf: &apipb.PeerConf{
			NeighborAddress: addr,
			PeerAsn:         peerASN,
			// GoBGP local-as is FRR "local-as N no-prepend replace-as":
			// only N is sent and N is not added to received paths.
			LocalAsn:     s.LocalASN,
			AuthPassword: cfg.AuthPassword,
		},
		Timers: &apipb.Timers{Config: &apipb.TimersConfig{
			ConnectRetry:           uint64(s.ConnectRetry),
			KeepaliveInterval:      uint64(s.KeepaliveTime),
			HoldTime:               uint64(s.HoldTime),
			IdleHoldTimeAfterReset: idleHoldAfterReset,
			ConnectAtOnce:          cfg.ConnectAtOnce,
		}},
		AfiSafis:  sessionFamilies(s),
		Transport: &apipb.Transport{MtuDiscovery: true},
	}
	// GoBGP checks the router AS on import, like FRR "allowas-in N" does
	// for bgp->as (bgpd/bgp_route.c, aspath_loop_check). FRR allows 1-10.
	peer.Conf.AllowOwnAsn = uint32(s.AllowASIn) //nolint:gosec // 0-10 from the parser
	if s.Interface {
		peer.Transport.LocalAddress = "::"
		peer.Transport.BindInterface = s.Name
		peer.Transport.RemoteAddress = addr
	} else if vrf := sessionVRF(s); vrf != "" {
		// SO_BINDTODEVICE on the VRF device: the connect uses the VRF
		// table, like FRR bgpd in "router bgp N vrf NAME".
		peer.Transport.BindInterface = vrf
	}
	setCapabilities(peer, &cfg.Capabilities)
	return peer
}

// setCapabilities adds the optional OPEN capabilities. GoBGP always sends
// route refresh, the FQDN (the hostname, set from HOSTNAME), 4-octet AS
// and extended next hop, and the software version to iBGP peers.
func setCapabilities(p *apipb.Peer, c *network.BGPCapabilities) {
	p.Conf.SendSoftwareVersion = c.SoftwareVersion
	if c.GracefulRestart {
		// Helper only, like the FRR default: the capability without
		// address family tuples, so a peer does not keep our routes when
		// the session ends. GoBGP then also sends long-lived graceful
		// restart without tuples (FRR sends them with stale time 0).
		// The N bit (RFC 8538) as FRR sets it by its version: with N
		// exchanged a NOTIFICATION other than Hard Reset keeps the routes
		// of a peer that sends family tuples, and GoBGP sends Hard Reset
		// for an administrative shutdown.
		p.GracefulRestart = &apipb.GracefulRestart{Enabled: true, HelperOnly: true, RestartTime: c.GracefulRestartTime, LonglivedEnabled: true,
			NotificationEnabled: c.GracefulNotification}
	}
	for _, af := range p.AfiSafis {
		if c.GracefulRestart {
			af.MpGracefulRestart = &apipb.MpGracefulRestart{Config: &apipb.MpGracefulRestartConfig{Enabled: true}}
			af.LongLivedGracefulRestart = &apipb.LongLivedGracefulRestart{Config: &apipb.LongLivedGracefulRestartConfig{Enabled: true}}
		}
		if c.AddPathReceive {
			af.AddPaths = &apipb.AddPaths{Config: &apipb.AddPathsConfig{Receive: true}}
		}
	}
}

func sessionFamilies(s *network.BGPSession) []*apipb.AfiSafi {
	var out []*apipb.AfiSafi
	add := func(afi apipb.Family_Afi, safi apipb.Family_Safi) {
		out = append(out, &apipb.AfiSafi{Config: &apipb.AfiSafiConfig{
			Family: &apipb.Family{Afi: afi, Safi: safi},
		}})
	}
	if s.IPv4Unicast {
		add(apipb.Family_AFI_IP, apipb.Family_SAFI_UNICAST)
	}
	if s.IPv6Unicast {
		add(apipb.Family_AFI_IP6, apipb.Family_SAFI_UNICAST)
	}
	if s.L2VPNEVPN {
		add(apipb.Family_AFI_L2VPN, apipb.Family_SAFI_EVPN)
	}
	return out
}
