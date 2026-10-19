package gobgp

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"

	"github.com/telekom/BOOTy/pkg/network"
)

// captureOpen starts a GoBGP server with peer p, accepts its connection
// on a loopback listener and returns the capabilities of its OPEN.
func captureOpen(t *testing.T, asn uint32, p *apipb.Peer) []bgp.ParameterCapabilityInterface {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := server.NewBgpServer()
	go s.Serve()
	defer s.Stop()
	if err := s.StartBgp(ctx, &apipb.StartBgpRequest{Global: &apipb.Global{Asn: asn, RouterId: "192.0.2.18", ListenPort: -1}}); err != nil {
		t.Fatal(err)
	}
	p.Conf.NeighborAddress = "127.0.0.1"
	p.Transport = &apipb.Transport{RemotePort: uint32(ln.Addr().(*net.TCPAddr).Port)} //nolint:gosec // a port
	if err := s.AddPeer(ctx, &apipb.AddPeerRequest{Peer: p}); err != nil {
		t.Fatal(err)
	}
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hdr := make([]byte, bgp.BGP_HEADER_LENGTH)
	if _, err := io.ReadFull(c, hdr); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, binary.BigEndian.Uint16(hdr[16:18]))
	copy(buf, hdr)
	if _, err := io.ReadFull(c, buf[bgp.BGP_HEADER_LENGTH:]); err != nil {
		t.Fatal(err)
	}
	m, err := bgp.ParseBGPMessage(buf)
	if err != nil {
		t.Fatal(err)
	}
	open, ok := m.Body.(*bgp.BGPOpen)
	if !ok {
		t.Fatalf("first message %T, want OPEN", m.Body)
	}
	var caps []bgp.ParameterCapabilityInterface
	for _, o := range open.OptParams {
		if c, ok := o.(*bgp.OptionParameterCapability); ok {
			caps = append(caps, c.Capability...)
		}
	}
	return caps
}

// capTokens renders the capabilities in the notation of the FRR captures
// in frrOpenCaps. Values that differ by host or AS (the FQDN, the 4-octet
// AS, the software version string) are left out.
func capTokens(caps []bgp.ParameterCapabilityInterface) []string {
	fam := func(afi uint16, safi uint8) string { return bgp.NewFamily(afi, safi).String() }
	var out []string
	for _, c := range caps {
		switch c := c.(type) {
		case *bgp.CapMultiProtocol:
			out = append(out, "mp:"+c.CapValue.String())
		case *bgp.CapExtendedNexthop:
			var t []string
			for _, tu := range c.Tuples {
				t = append(t, fam(tu.NLRIAFI, uint8(tu.NLRISAFI))+">"+map[uint16]string{bgp.AFI_IP: "ipv4", bgp.AFI_IP6: "ipv6"}[tu.NexthopAFI]) //nolint:gosec // a SAFI
			}
			out = append(out, "enhe:"+strings.Join(t, ","))
		case *bgp.CapGracefulRestart:
			var t []string
			for _, tu := range c.Tuples {
				t = append(t, fmt.Sprintf("%s/F%d", fam(tu.AFI, tu.SAFI), tu.Flags>>7))
			}
			out = append(out, fmt.Sprintf("gr:N=%d,time=%d,tuples=[%s]", c.Flags>>2&1, c.Time, strings.Join(t, ",")))
			if c.Flags&0x08 != 0 {
				out = append(out, "gr:R")
			}
		case *bgp.CapLongLivedGracefulRestart:
			var t []string
			for _, tu := range c.Tuples {
				t = append(t, fmt.Sprintf("%s/flags=%#x/stale=%d", fam(tu.AFI, tu.SAFI), tu.Flags, tu.RestartTime))
			}
			out = append(out, "llgr:["+strings.Join(t, ",")+"]")
		case *bgp.CapAddPath:
			var t []string
			for _, tu := range c.Tuples {
				t = append(t, tu.Family.String()+"/"+map[bgp.BGPAddPathMode]string{bgp.BGP_ADD_PATH_RECEIVE: "rx", bgp.BGP_ADD_PATH_SEND: "tx", bgp.BGP_ADD_PATH_BOTH: "rxtx"}[tu.Mode])
			}
			out = append(out, "addpath:"+strings.Join(t, ","))
		default:
			out = append(out, c.Code().String())
		}
	}
	slices.Sort(out)
	return out
}

// frrOpenCaps are the OPEN capabilities of FRR 10.2.1 (the legacy
// ramdisk) and 10.3.1, captured with tshark from bgpd with the bm4x
// template ("frr version 8.2.2", "frr defaults traditional") and with the
// CAPRF config ("frr version 9.1", "frr defaults datacenter"), to an
// unnumbered eBGP leaf (IPv4 unicast, local-as) and to a numbered iBGP
// route reflector (L2VPN EVPN only). Both versions send the same. "gr:R"
// is the Restart State bit: FRR sets it in the first restart time (120 s)
// after bgpd starts (bgpd/bgp_open.c, bgp->t_startup).
var frrOpenCaps = map[string][]string{
	"bm4x leaf": {"4-octet-as", "addpath:ipv4-unicast/rx", "enhanced-route-refresh", "enhe:ipv4-unicast>ipv6", "extended-message", "fqdn",
		"gr:N=0,time=120,tuples=[]", "gr:R", "llgr:[ipv4-unicast/flags=0x80/stale=0]", "mp:ipv4-unicast", "paths-limit", "route-refresh"},
	"bm4x rr": {"4-octet-as", "addpath:l2vpn-evpn/rx", "enhanced-route-refresh", "extended-message", "fqdn",
		"gr:N=0,time=120,tuples=[]", "gr:R", "llgr:[l2vpn-evpn/flags=0x80/stale=0]", "mp:l2vpn-evpn", "paths-limit", "route-refresh", "software-version"},
	"caprf leaf": {"4-octet-as", "addpath:ipv4-unicast/rx", "dynamic", "enhanced-route-refresh", "enhe:ipv4-unicast>ipv6", "extended-message", "fqdn",
		"gr:N=1,time=120,tuples=[]", "gr:R", "llgr:[ipv4-unicast/flags=0x80/stale=0]", "mp:ipv4-unicast", "paths-limit", "route-refresh", "software-version"},
	"caprf rr": {"4-octet-as", "addpath:l2vpn-evpn/rx", "dynamic", "enhanced-route-refresh", "extended-message", "fqdn",
		"gr:N=1,time=120,tuples=[]", "gr:R", "llgr:[l2vpn-evpn/flags=0x80/stale=0]", "mp:l2vpn-evpn", "paths-limit", "route-refresh", "software-version"},
}

// openDeviations are the known deviations of GoBGP from frrOpenCaps
// (docs/networking.md, BGP capabilities): what FRR sends and GoBGP does
// not, and what only GoBGP sends. A fixed deviation fails the test, so the
// list and the docs stay current.
var openDeviations = struct {
	missing []string
	extra   map[string][]string // per peer kind
}{
	missing: []string{"dynamic", "gr:R",
		"llgr:[ipv4-unicast/flags=0x80/stale=0]", "llgr:[l2vpn-evpn/flags=0x80/stale=0]"},
	extra: map[string][]string{"leaf": {"llgr:[]"}, "rr": {"llgr:[]"}},
}

// GoBGP sends the OPEN capabilities of FRR 10 with the bm4x template and
// the CAPRF config, except the known deviations.
func TestOpenCapabilitiesMatchFRR(t *testing.T) {
	bm4x := network.BGPCapabilities{GracefulRestart: true, GracefulRestartTime: 120, AddPathReceive: true}
	caprf := bm4x
	caprf.GracefulNotification, caprf.SoftwareVersion = true, true
	for name, caps := range map[string]network.BGPCapabilities{"bm4x": bm4x, "caprf": caprf} {
		cfg := &Config{ASN: 4210000017, Capabilities: caps}
		for kind, s := range map[string]*network.BGPSession{
			"leaf": {Name: "eth0", Interface: true, RemoteASN: 65500, LocalASN: 65501, IPv4Unicast: true},
			"rr":   {Name: "198.51.100.1", Internal: true, L2VPNEVPN: true},
		} {
			key := name + " " + kind
			var want []string
			for _, c := range frrOpenCaps[key] {
				if !slices.Contains(openDeviations.missing, c) {
					want = append(want, c)
				}
			}
			want = append(want, openDeviations.extra[kind]...)
			slices.Sort(want)
			got := capTokens(captureOpen(t, cfg.ASN, buildSessionPeer(cfg, s, "127.0.0.1")))
			if !slices.Equal(got, want) {
				t.Errorf("%s: OPEN capabilities\n got %v\nwant %v (FRR %v)", key, got, want, frrOpenCaps[key])
			}
		}
	}
}
