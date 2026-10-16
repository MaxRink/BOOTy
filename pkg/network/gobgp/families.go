package gobgp

import (
	"cmp"

	apipb "github.com/osrg/gobgp/v3/api"

	"github.com/telekom/BOOTy/pkg/network"
)

// bgpTimers returns the configured BGP keepalive/hold timers.
func bgpTimers(cfg *Config) *apipb.Timers {
	return &apipb.Timers{
		Config: &apipb.TimersConfig{
			ConnectRetry:      cfg.ConnectRetry,
			KeepaliveInterval: cfg.KeepaliveInterval,
			HoldTime:          cfg.HoldTime,
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
		},
		Timers:   bgpTimers(cfg),
		AfiSafis: families,
		Transport: &apipb.Transport{
			MtuDiscovery: true,
		},
	}

	if cfg.AuthPassword != "" {
		peer.Conf.AuthPassword = cfg.AuthPassword
	}

	return peer
}

// buildInterfacePeer constructs a GoBGP peer config for an unnumbered (link-local) session.
func buildInterfacePeer(cfg *Config, iface, addr string, families []*apipb.AfiSafi) *apipb.Peer {
	peer := &apipb.Peer{
		Conf: &apipb.PeerConf{
			NeighborAddress: addr,
			PeerAsn:         0, // External peer, ASN learned via open
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
			ConnectRetry:      uint64(s.ConnectRetry),
			KeepaliveInterval: uint64(s.KeepaliveTime),
			HoldTime:          uint64(s.HoldTime),
		}},
		AfiSafis:  sessionFamilies(s),
		Transport: &apipb.Transport{MtuDiscovery: true},
	}
	if s.Interface {
		peer.Transport.LocalAddress = "::"
		peer.Transport.BindInterface = s.Name
		peer.Transport.RemoteAddress = addr
	} else if vrf := sessionVRF(s); vrf != "" {
		// SO_BINDTODEVICE on the VRF device: the connect uses the VRF
		// table, like FRR bgpd in "router bgp N vrf NAME".
		peer.Transport.BindInterface = vrf
	}
	return peer
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
