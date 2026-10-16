package gobgp

import (
	"testing"

	apipb "github.com/osrg/gobgp/v3/api"

	"github.com/telekom/BOOTy/pkg/network"
)

// The production template peers: an unnumbered leaf uplink with local-as
// and a route reflector bound to the underlay VRF.
func TestBuildSessionPeer(t *testing.T) {
	cfg := &Config{ASN: 4210000017}
	leaf := &network.BGPSession{Name: "ens1f0np0", Interface: true, VRF: "Vrf_underlay", RemoteASN: 65500,
		LocalASN: 65501, KeepaliveTime: 30, HoldTime: 90, ConnectRetry: 120, IPv4Unicast: true}
	p := buildSessionPeer(cfg, leaf, "fe80::1%ens1f0np0")
	if p.Conf.PeerAsn != 65500 || p.Conf.LocalAsn != 65501 {
		t.Errorf("leaf ASNs = %d/%d, want 65500/65501", p.Conf.PeerAsn, p.Conf.LocalAsn)
	}
	if p.Transport.BindInterface != "ens1f0np0" || p.Transport.RemoteAddress != "fe80::1%ens1f0np0" {
		t.Errorf("leaf transport = %+v, want bound to the uplink", p.Transport)
	}
	if tm := p.Timers.Config; tm.KeepaliveInterval != 30 || tm.HoldTime != 90 || tm.ConnectRetry != 120 {
		t.Errorf("leaf timers = %+v, want 30/90/120", tm)
	}
	if len(p.AfiSafis) != 1 || p.AfiSafis[0].Config.Family.Afi != apipb.Family_AFI_IP {
		t.Errorf("leaf families = %v, want ipv4 unicast only", p.AfiSafis)
	}

	rr := &network.BGPSession{Name: "198.51.100.1", VRF: "Vrf_underlay", Internal: true, RemoteASN: 4210000017,
		KeepaliveTime: 30, HoldTime: 90, ConnectRetry: 5, L2VPNEVPN: true}
	p = buildSessionPeer(cfg, rr, rr.Name)
	if p.Conf.PeerAsn != 4210000017 || p.Conf.LocalAsn != 0 {
		t.Errorf("rr ASNs = %d/%d, want iBGP 4210000017", p.Conf.PeerAsn, p.Conf.LocalAsn)
	}
	if p.Transport.BindInterface != "Vrf_underlay" {
		t.Errorf("rr bind = %q, want the VRF device", p.Transport.BindInterface)
	}
	if len(p.AfiSafis) != 1 || p.AfiSafis[0].Config.Family.Safi != apipb.Family_SAFI_EVPN {
		t.Errorf("rr families = %v, want l2vpn evpn only", p.AfiSafis)
	}

	rr.VRF = ""
	if p = buildSessionPeer(cfg, rr, rr.Name); p.Transport.BindInterface != "" {
		t.Errorf("default VRF bind = %q, want none", p.Transport.BindInterface)
	}
}

func TestValidateSessions(t *testing.T) {
	ok := network.BGPSession{Name: "192.0.2.1", KeepaliveTime: 3, HoldTime: 9, IPv4Unicast: true}
	if err := validateSessions([]network.BGPSession{ok}); err != nil {
		t.Fatalf("validateSessions() = %v", err)
	}
	for name, s := range map[string]network.BGPSession{
		"bad address": {Name: "leaf", KeepaliveTime: 3, HoldTime: 9, IPv4Unicast: true},
		"hold 2":      {Name: "192.0.2.1", KeepaliveTime: 3, HoldTime: 2, IPv4Unicast: true},
		"no family":   {Name: "192.0.2.1", KeepaliveTime: 3, HoldTime: 9},
		// FRR "timers 0 0" disables keepalives; GoBGP would use 30/90.
		"timers 0 0":  {Name: "192.0.2.1", IPv4Unicast: true},
		"keepalive 0": {Name: "192.0.2.1", HoldTime: 9, IPv4Unicast: true},
	} {
		if err := validateSessions([]network.BGPSession{s}); err == nil {
			t.Errorf("%s: validateSessions() = nil, want an error", name)
		}
	}
}

// An iBGP session uses the router AS even when the session has no remote
// AS of its own (native or older inputs).
// remote-as N with local-as N: iBGP in AS N, so GoBGP gets PeerAsn and
// LocalAsn N.
func TestBuildSessionPeerInternalLocalAS(t *testing.T) {
	p := buildSessionPeer(&Config{ASN: 65000}, &network.BGPSession{Name: "192.0.2.1", Internal: true, RemoteASN: 65501, LocalASN: 65501, IPv4Unicast: true}, "192.0.2.1")
	if p.Conf.PeerAsn != 65501 || p.Conf.LocalAsn != 65501 {
		t.Fatalf("PeerAsn %d LocalAsn %d, want 65501 for both", p.Conf.PeerAsn, p.Conf.LocalAsn)
	}
}

func TestBuildSessionPeerInternalWithoutRemoteASN(t *testing.T) {
	p := buildSessionPeer(&Config{ASN: 65000}, &network.BGPSession{Name: "192.0.2.1", Internal: true, IPv4Unicast: true}, "192.0.2.1")
	if p.Conf.PeerAsn != 65000 {
		t.Fatalf("PeerAsn = %d, want the router AS 65000", p.Conf.PeerAsn)
	}
}
