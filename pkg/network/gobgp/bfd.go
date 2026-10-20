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
// bfd_session_status_update): BFD Up to Down ends the BGP session with
// Cease/BFD Down (RFC 9384, bgp_bfd.c:70; ResetPeerRequest.BfdDown of the
// GoBGP fork), and GoBGP connects again after idleHoldAfterReset. FRR
// also starts a connect at BFD Up; GoBGP has no API for that. With
// BGP_CONNECT_AT_ONCE (ConnectAtOnce) its Active state connects at once by
// itself (the outgoingConnManager.run and connectLoop functions of the
// fork); without it, after 1.5 to 2 s.
//
// Like bgpd, a BFD Down does not reset a BGP session that is just coming
// up: OpenConfirm, Established before GoBGP has set the uptime, or the
// same second as the uptime (FRR 10.3 and later: bss->last_event >
// peer->uptime, both in seconds; 10.2 stops the session on every Up to
// Down).
//
// A route reflector with "update-source" re-keys its BFD session for us
// when its side of the BGP session comes up (bgp_peer_bfd_update_source),
// so the BFD session we just brought Up goes Down at once, with diag 3
// (Neighbor Signaled Session Down). The FRR shadow node sees the same Down
// and keeps the session. For that diag only, BOOTy is more tolerant,
// because GoBGP shows the start late: the RR comes up when it receives our
// KEEPALIVE, and GoBGP writes that KEEPALIVE before its server loop moves
// the peer out of Active (outgoing connection) or OpenSent (incoming
// connection), so ListPeer can show any state from Connect on; and
// GoBGP's uptime is in whole seconds, so a Down in the next second can be
// 1 ms after the session came up. Idle (no session; the reset sets the
// idle hold time after reset) still resets at once, and a Down for a peer
// that GoBGP does not know (BOOTy restarted it) is ignored: the new peer
// has a new session. Any other diag, such as an expired detection time,
// is a path failure and keeps the rule above.
//
// A skipped BFD Down is checked again (recheck), so a real failure is not
// hidden until the hold timer expires: if BFD is not Up again within
// bfdRecheckGrace of the Down and the session is Established, it is reset
// then.
func (u *UnderlayTier) bfdChanged(p *bfd.Peer, from, to bfd.State, diag bfd.Diag) {
	u.bfdUp.Store(p.Name, to == bfd.StateUp)
	if from != bfd.StateUp || to != bfd.StateDown {
		return
	}
	now := time.Now
	if u.now != nil {
		now = u.now
	}
	downAt := now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := u.bgpSession
	if session == nil {
		session = u.sessionState
	}
	rekey := diag == bfd.DiagNeighborSaysDown
	// GoBGP's uptime is wall-clock time, and a time sync that steps the
	// clock back puts it in the future (FRR compares monotonic times).
	// Then the BFD Down resets the session as usual.
	state, at := session(ctx, p.Name)
	switch upFor := downAt.Unix() - at.Unix(); {
	case state == apipb.PeerState_SESSION_STATE_IDLE:
	case rekey && state == apipb.PeerState_SESSION_STATE_UNSPECIFIED:
		u.log.Info("BFD down for an unknown BGP peer, not resetting it", "peer", p.Name, "diag", diag)
		return
	case state == apipb.PeerState_SESSION_STATE_OPENCONFIRM,
		state == apipb.PeerState_SESSION_STATE_ESTABLISHED && at.IsZero(),
		rekey && state != apipb.PeerState_SESSION_STATE_ESTABLISHED:
		u.log.Info("BFD down while the BGP session is coming up, not resetting it yet", "peer", p.Name, "state", state, "diag", diag)
		u.recheck(p, time.Time{})
		return
	case upFor == 0 || rekey && upFor == 1:
		u.log.Info("BFD down right after the BGP session came up, not resetting it yet, like FRR", "peer", p.Name, "diag", diag)
		u.recheck(p, at)
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

// bfdRecheck is the running recheck of a BFD peer. checkMu guards it.
type bfdRecheck struct {
	downAt time.Time // the last BFD Down; the grace time counts from it
	uptime time.Time // the uptime of the session it checks; zero until known
}

// recheck starts resetUnlessBFDUp for p, for the session with uptime (zero:
// the session that comes up next). A peer has at most one recheck: a
// Down while one runs restarts its grace time and binds it to the
// session of that Down. Teardown stops it
// (stopRechecks).
func (u *UnderlayTier) recheck(p *bfd.Peer, uptime time.Time) {
	grace := cmp.Or(u.bfdUpGrace, bfdRecheckGrace(p))
	u.checkMu.Lock()
	defer u.checkMu.Unlock()
	if u.checkCtx == nil {
		u.checkCtx, u.stopChecks = context.WithCancel(context.Background())
	}
	if rc, ok := u.rechecks[p.Name]; ok {
		// The Down can be one of a new session (BOOTy restarted the
		// peer within a tick): check that one.
		rc.downAt, rc.uptime = time.Now(), uptime
		return
	}
	if u.rechecks == nil {
		u.rechecks = map[string]*bfdRecheck{}
	}
	rc := &bfdRecheck{downAt: time.Now(), uptime: uptime}
	u.rechecks[p.Name] = rc
	ctx := u.checkCtx
	u.checks.Add(1)
	go func() {
		defer u.checks.Done()
		u.resetUnlessBFDUp(ctx, p.Name, rc, grace)
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
// the last Down. It ends without a reset when the grace time is over and
// the session is not coming up (Connect, Active or OpenSent), and when the
// session it checks ended (Idle, the peer is gone, or another uptime):
// BOOTy restarted the peer, and the new session has its own BFD Down.
func (u *UnderlayTier) resetUnlessBFDUp(ctx context.Context, name string, rc *bfdRecheck, grace time.Duration) {
	session := u.bgpSession
	if session == nil {
		session = u.sessionState
	}
	tick := time.NewTicker(cmp.Or(grace/30, 10*time.Millisecond))
	defer tick.Stop()
	for range tick.C {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		state, at := session(qctx, name)
		cancel()
		// The decision and the end of the recheck under checkMu: a Down
		// either restarts this recheck's grace time or starts a new one.
		u.checkMu.Lock()
		reset, done := u.recheckStep(ctx, name, rc, grace, state, at)
		if done {
			delete(u.rechecks, name)
		}
		u.checkMu.Unlock()
		if reset {
			u.log.Warn("BGP session came up while BFD stayed down, resetting it", "peer", name)
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			u.reset(rctx, name)
			cancel()
		}
		if done {
			return
		}
	}
}

// recheckStep decides one tick of resetUnlessBFDUp; checkMu is held.
func (u *UnderlayTier) recheckStep(ctx context.Context, name string, rc *bfdRecheck, grace time.Duration,
	state apipb.PeerState_SessionState, at time.Time) (reset, done bool) {
	established := state == apipb.PeerState_SESSION_STATE_ESTABLISHED && !at.IsZero()
	if established && rc.uptime.IsZero() {
		rc.uptime = at
	}
	up, _ := u.bfdUp.Load(name)
	switch isUp, _ := up.(bool); {
	case ctx.Err() != nil, isUp: // Teardown, or BFD is Up again
		return false, true
	case established && !at.Equal(rc.uptime),
		state == apipb.PeerState_SESSION_STATE_IDLE, state == apipb.PeerState_SESSION_STATE_UNSPECIFIED:
		return false, true // the session ended
	case state == apipb.PeerState_SESSION_STATE_OPENCONFIRM || state == apipb.PeerState_SESSION_STATE_ESTABLISHED && at.IsZero():
		return false, false // still coming up; the hold timer bounds this
	case time.Since(rc.downAt) < grace:
		return false, false // ListPeer can lag behind the FSM (bfdChanged)
	}
	return established, true
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
	// BfdDown sends Cease/BFD Down (subcode 10) like FRR; GoBGP sends no
	// shutdown communication with it, so reason only goes to the log.
	if err := u.bgp.ResetPeer(ctx, &apipb.ResetPeerRequest{Address: addr, BfdDown: true}); err != nil {
		u.log.Warn("reset BGP peer", "peer", addr, "reason", reason, "error", err)
	}
}
