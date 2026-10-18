//go:build linux

package gobgp

import (
	"context"
	"net/netip"
	"time"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/vishvananda/netlink"

	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/bfd"
)

// startBFD starts a BFD session for every peer that asks for one: the
// frr.conf sessions with "neighbor X bfd", or every native peer when
// BFD_TRANSMIT_MS and BFD_RECEIVE_MS are set.
func (u *UnderlayTier) startBFD() {
	peers := u.bfdPeers()
	if len(peers) == 0 {
		return
	}
	u.bfd = bfd.NewManager(u.log.With("component", "bfd"), u.bfdChanged)
	for i := range peers {
		p := &peers[i]
		if err := u.bfd.Add(p); err != nil {
			// FRR keeps the BGP session when its BFD session fails.
			u.log.Warn("BFD session not started; the BGP session runs without it", "peer", p.Name, "error", err)
		}
	}
}

func (u *UnderlayTier) bfdPeers() []bfd.Peer {
	var peers []bfd.Peer
	if len(u.cfg.Sessions) > 0 {
		for i := range u.cfg.Sessions {
			s := &u.cfg.Sessions[i]
			if !s.BFD {
				continue
			}
			t := bfdTimers(s.BFDTransmitMS, s.BFDReceiveMS, s.BFDDetectMult)
			if s.Interface {
				peers = u.appendIfacePeer(peers, s.Name, t)
			} else {
				peers = u.appendNumberedPeer(peers, s.Name, sessionVRF(s), s.Internal, t)
			}
		}
		return peers
	}
	if u.cfg.BFDTransmitMS == 0 {
		return nil
	}
	t := bfdTimers(u.cfg.BFDTransmitMS, u.cfg.BFDReceiveMS, u.cfg.BFDDetectMult)
	for iface := range u.ifacePeers {
		peers = u.appendIfacePeer(peers, iface, t)
	}
	for _, addr := range u.cfg.NeighborAddrs {
		peers = u.appendNumberedPeer(peers, addr, u.cfg.NeighborVRF, u.cfg.IsiBGP(), t)
	}
	return peers
}

// appendIfacePeer adds the single-hop session of an unnumbered peer over
// its IPv6 link-local address (RFC 5881).
func (u *UnderlayTier) appendIfacePeer(peers []bfd.Peer, iface string, t bfd.Timers) []bfd.Peer {
	name, ok := u.ifacePeers[iface]
	addr, err := netip.ParseAddr(name)
	if !ok || err != nil {
		u.log.Warn("BFD: no link-local peer address", "interface", iface)
		return peers
	}
	return append(peers, bfd.Peer{Name: name, Addr: addr, Device: iface, Timers: t})
}

// appendNumberedPeer adds the session of a numbered peer, bound to the VRF
// of the BGP session. Like FRR 10.3 bgpd (bgp_bfd.h PEER_IS_MULTIHOP), an
// iBGP peer that is not on a connected network of that VRF is multihop
// (RFC 5883) and sent from the router ID; an eBGP peer is single-hop
// (BOOTy has no ebgp-multihop).
func (u *UnderlayTier) appendNumberedPeer(peers []bfd.Peer, name, vrf string, internal bool, t bfd.Timers) []bfd.Peer {
	addr, err := netip.ParseAddr(name)
	if err != nil {
		return peers
	}
	p := bfd.Peer{Name: name, Addr: addr, Device: vrf, Timers: t}
	connected := u.connected
	if connected == nil {
		connected = connectedInVRF
	}
	if internal && !connected(addr, vrf) {
		p.Multihop = true
		p.Local, _ = netip.ParseAddr(u.cfg.RouterID)
	}
	return append(peers, p)
}

// connectedInVRF reports whether addr is in the prefix of an address on a
// link of the VRF ("" is the default VRF), like FRR shared_network. Host
// routes do not count.
func connectedInVRF(addr netip.Addr, vrf string) bool {
	links, err := network.RetryDump(netlink.LinkList)
	if err != nil {
		return false
	}
	vrfs := map[int]string{}
	for _, l := range links {
		if l.Type() == "vrf" {
			vrfs[l.Attrs().Index] = l.Attrs().Name
		}
	}
	for _, l := range links {
		a := l.Attrs()
		in := vrfs[a.MasterIndex]
		if _, isVRF := vrfs[a.Index]; isVRF {
			in = a.Name
		}
		if in != vrf {
			continue
		}
		addrs, err := network.RetryDump(func() ([]netlink.Addr, error) { return netlink.AddrList(l, netlink.FAMILY_ALL) })
		if err != nil {
			continue
		}
		for _, la := range addrs {
			ones, bits := la.Mask.Size()
			if ones == bits {
				continue
			}
			ip, ok := netip.AddrFromSlice(la.IP)
			if ok && netip.PrefixFrom(ip.Unmap(), ones).Masked().Contains(addr.Unmap()) {
				return true
			}
		}
	}
	return false
}

func bfdTimers(txMS, rxMS, mult uint32) bfd.Timers {
	if mult == 0 {
		mult = 3
	}
	return bfd.Timers{
		DesiredMinTx:  time.Duration(txMS) * time.Millisecond,
		RequiredMinRx: time.Duration(rxMS) * time.Millisecond,
		DetectMult:    uint8(min(mult, 255)), //nolint:gosec // bounded above
	}
}

// bfdChanged couples BFD to BGP like FRR 10.3 bgpd (bgp_bfd.c
// bfd_session_status_update): BFD Up to Down ends the BGP session. GoBGP
// sends Cease/Administrative Reset, not Cease/BFD Down (RFC 9384), and
// connects again after idleHoldAfterReset. FRR also starts a connect at
// BFD Up; GoBGP has no API for that, its Active state connects within
// 1 to 2 s by itself.
//
// Like bgpd, a BFD Down does not reset a BGP session that came up in the
// same second (bss->last_event > peer->uptime, both in seconds): a
// route reflector with "update-source" re-keys its BFD session for us
// when the BGP session comes up (bgp_peer_bfd_update_source), so the BFD
// session we just brought Up goes Down in that second. The FRR shadow
// node sees the same Down and keeps the session; kindmetal and the lab
// saw BOOTy reset the RR session once at every start.
func (u *UnderlayTier) bfdChanged(p *bfd.Peer, from, to bfd.State, diag bfd.Diag) {
	if from != bfd.StateUp || to != bfd.StateDown {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uptime := u.bgpUptime
	if uptime == nil {
		uptime = u.establishedAt
	}
	now := time.Now
	if u.now != nil {
		now = u.now
	}
	// Only the same second: GoBGP's uptime is wall-clock time, and a time
	// sync that steps the clock back puts it in the future (FRR compares
	// monotonic times). Then the BFD Down resets the session as usual.
	if at, ok := uptime(ctx, p.Name); ok && now().Unix() == at.Unix() {
		u.log.Info("BFD down in the second the BGP session came up, not resetting it, like FRR", "peer", p.Name, "diag", diag)
		return
	}
	u.log.Warn("BFD down, resetting the BGP session", "peer", p.Name, "diag", diag)
	reset := u.resetBGP
	if reset == nil {
		reset = u.resetPeer
	}
	reset(ctx, p.Name, "BFD down")
}

// establishedAt returns when the BGP session of addr came up, and false
// when it is not up.
func (u *UnderlayTier) establishedAt(ctx context.Context, addr string) (time.Time, bool) {
	var at time.Time
	up := false
	if u.bgp == nil {
		return at, false
	}
	_ = u.bgp.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
		if p.GetState().GetSessionState() == apipb.PeerState_ESTABLISHED && p.GetTimers().GetState().GetUptime() != nil {
			at, up = p.GetTimers().GetState().GetUptime().AsTime(), true
		}
	})
	return at, up
}

func (u *UnderlayTier) resetPeer(ctx context.Context, addr, reason string) {
	if err := u.bgp.ResetPeer(ctx, &apipb.ResetPeerRequest{Address: addr, Communication: reason}); err != nil {
		u.log.Warn("reset BGP peer", "peer", addr, "error", err)
	}
}
