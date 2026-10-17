package gobgp

import (
	"reflect"
	"testing"

	apipb "github.com/osrg/gobgp/v3/api"

	"github.com/telekom/BOOTy/pkg/network"
)

// The FRR default capabilities reach every kind of peer: graceful restart
// as helper (no family tuples), long-lived graceful restart, add-path
// receive; the software version only when asked for.
func TestSetCapabilities(t *testing.T) {
	frr := network.BGPCapabilities{GracefulRestart: true, GracefulRestartTime: 120, AddPathReceive: true, SoftwareVersion: true}
	for _, caps := range []network.BGPCapabilities{frr, {}} {
		cfg := &Config{ASN: 65000, Capabilities: caps}
		for name, p := range map[string]*apipb.Peer{
			"numbered":  buildNumberedPeer(cfg, "10.0.0.1", allFamilies()),
			"interface": buildInterfacePeer(cfg, "eth1", "fe80::2%eth1", ipv4Families()),
			"session":   buildSessionPeer(cfg, &network.BGPSession{Name: "10.0.0.1", Internal: true, L2VPNEVPN: true}, "10.0.0.1"),
		} {
			gr := p.GetGracefulRestart()
			if caps.GracefulRestart != gr.GetEnabled() || gr.GetHelperOnly() != caps.GracefulRestart || gr.GetRestartTime() != caps.GracefulRestartTime ||
				gr.GetLonglivedEnabled() != caps.GracefulRestart || p.GetConf().GetSendSoftwareVersion() != caps.SoftwareVersion {
				t.Errorf("%s peer with %+v: graceful restart %+v, software version %v", name, caps, gr, p.GetConf().GetSendSoftwareVersion())
			}
			for _, af := range p.GetAfiSafis() {
				llgr := af.GetLongLivedGracefulRestart().GetConfig()
				if af.GetMpGracefulRestart().GetConfig().GetEnabled() != caps.GracefulRestart ||
					llgr.GetEnabled() != caps.GracefulRestart || llgr.GetRestartTime() != 0 ||
					af.GetAddPaths().GetConfig().GetReceive() != caps.AddPathReceive || af.GetAddPaths().GetConfig().GetSendMax() != 0 {
					t.Errorf("%s peer with %+v: family %v gr %v llgr %v add-path %v", name, caps, af.GetConfig().GetFamily(),
						af.GetMpGracefulRestart(), llgr, af.GetAddPaths())
				}
			}
		}
	}
}

// The families of the BGP_PEER_MODE peers: in dual mode the numbered peers
// (route reflectors) get only L2VPN-EVPN, like the bm4x template.
func TestPeerModeFamilies(t *testing.T) {
	names := func(f func() []*apipb.AfiSafi) []string {
		if f == nil {
			return nil
		}
		var out []string
		for _, af := range f() {
			out = append(out, af.GetConfig().GetFamily().GetSafi().String())
		}
		return out
	}
	for _, tt := range []struct {
		mode            network.PeerMode
		iface, numbered []string
	}{
		{network.PeerModeUnnumbered, []string{"SAFI_UNICAST", "SAFI_EVPN"}, nil},
		{network.PeerModeDual, []string{"SAFI_UNICAST"}, []string{"SAFI_EVPN"}},
		{network.PeerModeNumbered, nil, []string{"SAFI_UNICAST", "SAFI_EVPN"}},
	} {
		iface, numbered, err := peerModeFamilies(tt.mode)
		if err != nil || !reflect.DeepEqual(names(iface), tt.iface) || !reflect.DeepEqual(names(numbered), tt.numbered) {
			t.Errorf("%s: interface %v numbered %v (%v), want %v %v", tt.mode, names(iface), names(numbered), err, tt.iface, tt.numbered)
		}
	}
	if _, _, err := peerModeFamilies("bogus"); err == nil {
		t.Error("an unknown peer mode must fail")
	}
}

// NewConfig passes the capabilities to the peers and turns zebra's
// resolve-via-default on only for "frr defaults traditional".
func TestNewConfigCapabilitiesAndProfile(t *testing.T) {
	caps := network.BGPCapabilities{GracefulRestart: true, GracefulRestartTime: 90, AddPathReceive: true, SoftwareVersion: true}
	for profile, via := range map[string]bool{"traditional": true, "datacenter": false} {
		cfg, err := NewConfig(&network.Config{
			UnderlayIP: "10.0.0.1", ASN: 65000, ProvisionVNI: 4000, BGPPeerMode: network.PeerModeUnnumbered, EVPNType5Only: true,
			BGPCapabilities: caps, BGPDefaults: profile,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Capabilities != caps || cfg.ResolveViaDefault != via {
			t.Errorf("%s: capabilities %+v resolve-via-default %v, want %+v %v", profile, cfg.Capabilities, cfg.ResolveViaDefault, caps, via)
		}
	}
}
