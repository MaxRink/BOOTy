//go:build linux

package gobgp

import (
	"cmp"
	"context"
	"net/netip"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
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
// 1.5 to 2 s by itself (pkg/server/fsm.go:980-982: minConnectRetryInterval
// 2 with 0.75 to 1 jitter since GoBGP v4.0.0).
//
// Like bgpd, a BFD Down does not reset a BGP session that came up in the
// same second (bss->last_event > peer->uptime, both in seconds): a
// route reflector with "update-source" re-keys its BFD session for us
// when the BGP session comes up (bgp_peer_bfd_update_source), so the BFD
// session we just brought Up goes Down in that second. The FRR shadow
// node sees the same Down and keeps the session; kindmetal and the lab
// saw BOOTy reset the RR session once at every start. The RR comes up
// when it receives our KEEPALIVE and can re-key BFD before its KEEPALIVE
// moves our session from OpenConfirm to Established, so a BFD Down in
// OpenConfirm does not reset the session at once either (bgpd would, but
// its BFD Down comes through zebra and arrives after its KEEPALIVE).
// GoBGP's ListPeer shows OpenConfirm until its server loop handles the
// state change, after the FSM logged "Peer Up", and then Established
// without an uptime until the FSM sets it; both count as coming up. So a
// real failure is not hidden until the hold timer expires, a skipped BFD
// Down is checked again when the session is Established: if BFD is not
// Up again within bfdRecheckGrace of the Down, the session is reset then.
func (u *UnderlayTier) bfdChanged(p *bfd.Peer, from, to bfd.State, diag bfd.Diag) {
	u.bfdUp.Store(p.Name, to == bfd.StateUp)
	if from != bfd.StateUp || to != bfd.StateDown {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := u.bgpSession
	if session == nil {
		session = u.sessionState
	}
	now := time.Now
	if u.now != nil {
		now = u.now
	}
	// Only the same second: GoBGP's uptime is wall-clock time, and a time
	// sync that steps the clock back puts it in the future (FRR compares
	// monotonic times). Then the BFD Down resets the session as usual.
	switch state, at := session(ctx, p.Name); {
	case state == apipb.PeerState_SESSION_STATE_OPENCONFIRM:
		u.log.Info("BFD down while the BGP session is coming up (OpenConfirm), not resetting it yet", "peer", p.Name, "diag", diag)
		u.recheck(p)
		return
	case state == apipb.PeerState_SESSION_STATE_ESTABLISHED && (at.IsZero() || now().Unix() == at.Unix()):
		u.log.Info("BFD down in the second the BGP session came up, not resetting it yet, like FRR", "peer", p.Name, "diag", diag)
		u.recheck(p)
		return
	}
	u.log.Warn("BFD down, resetting the BGP session", "peer", p.Name, "diag", diag)
	u.reset(ctx, p.Name)
}

// bfdRecheckGrace is the time BFD gets to come Up again after a Down that
// did not reset a starting session: the detection time at the slow
// transmit interval of a session that is not Up (multiplier x 1 s). The
// RR's re-keyed session is Up again after about 1 s.
func bfdRecheckGrace(p *bfd.Peer) time.Duration {
	return time.Duration(cmp.Or(p.Timers.DetectMult, 3)) * bfd.SlowTx
}

// recheck starts resetUnlessBFDUp for p; Teardown stops it (stopRechecks).
func (u *UnderlayTier) recheck(p *bfd.Peer) {
	grace := cmp.Or(u.bfdUpGrace, bfdRecheckGrace(p))
	u.checkMu.Lock()
	defer u.checkMu.Unlock()
	if u.checkCtx == nil {
		u.checkCtx, u.stopChecks = context.WithCancel(context.Background())
	}
	ctx := u.checkCtx
	u.checks.Add(1)
	go func() {
		defer u.checks.Done()
		u.resetUnlessBFDUp(ctx, p.Name, time.Now(), grace)
	}()
}

// stopRechecks stops the rechecks and waits for them; later ones end at
// once. Teardown calls it before the BGP server stops, so no recheck
// waits on a stopped server or resets a peer during the teardown.
func (u *UnderlayTier) stopRechecks() {
	u.checkMu.Lock()
	if u.checkCtx == nil {
		u.checkCtx, u.stopChecks = context.WithCancel(context.Background())
	}
	u.stopChecks()
	u.checkMu.Unlock()
	u.checks.Wait()
}

// resetUnlessBFDUp waits until the BGP session of name is Established with
// an uptime, then resets it unless BFD came Up again within grace after
// downAt. A session that does not come up needs no reset.
func (u *UnderlayTier) resetUnlessBFDUp(ctx context.Context, name string, downAt time.Time, grace time.Duration) {
	session := u.bgpSession
	if session == nil {
		session = u.sessionState
	}
	tick := time.NewTicker(cmp.Or(grace/30, 10*time.Millisecond))
	defer tick.Stop()
	for range tick.C {
		if up, ok := u.bfdUp.Load(name); ok && up.(bool) {
			return
		}
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		state, at := session(qctx, name)
		cancel()
		switch {
		case ctx.Err() != nil: // Teardown
			return
		case state == apipb.PeerState_SESSION_STATE_OPENCONFIRM || state == apipb.PeerState_SESSION_STATE_ESTABLISHED && at.IsZero():
			continue // still coming up; the hold timer bounds this
		case state != apipb.PeerState_SESSION_STATE_ESTABLISHED:
			return
		case time.Since(downAt) < grace:
			continue
		}
		u.log.Warn("BGP session came up while BFD stayed down, resetting it", "peer", name)
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		u.reset(rctx, name)
		cancel()
		return
	}
}

func (u *UnderlayTier) reset(ctx context.Context, name string) {
	reset := u.resetBGP
	if reset == nil {
		reset = u.resetPeer
	}
	reset(ctx, name, "BFD down")
}

// sessionState returns the BGP session state of addr and, when it is
// established, when it came up. Without a server or peer it is UNKNOWN.
func (u *UnderlayTier) sessionState(ctx context.Context, addr string) (apipb.PeerState_SessionState, time.Time) {
	state, at := apipb.PeerState_SESSION_STATE_UNSPECIFIED, time.Time{}
	if u.bgp == nil {
		return state, at
	}
	_ = u.bgp.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
		state = p.GetState().GetSessionState()
		if state == apipb.PeerState_SESSION_STATE_ESTABLISHED && p.GetTimers().GetState().GetUptime() != nil {
			at = p.GetTimers().GetState().GetUptime().AsTime()
		}
	})
	return state, at
}

func (u *UnderlayTier) resetPeer(ctx context.Context, addr, reason string) {
	if err := u.bgp.ResetPeer(ctx, &apipb.ResetPeerRequest{Address: addr, Communication: reason}); err != nil {
		u.log.Warn("reset BGP peer", "peer", addr, "error", err)
	}
}
