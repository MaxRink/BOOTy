//go:build linux

package gobgp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/server"
	"google.golang.org/protobuf/proto"
)

// peerRestarter starts a GoBGP peer again at once when its session ends
// while BOOTy keeps the peer up. GoBGP then holds the peer in Idle for its
// idle hold time and closes every connection that the peer opens meanwhile
// (RFC 4271 section 8.2.2). Since the GoBGP fork tag v4.10.0-booty.19 the
// idle hold time follows v_start of FRR 10.2.1 (before: a fixed 5 s,
// osrg/gobgp#3735; lab: 36 s against 2.3 s for FRR): 1 s after
// Established and after an administrative shutdown; doubled, up to 120 s,
// for a NOTIFICATION received in OpenConfirm or Established and for a
// NOTIFICATION that GoBGP sends for a message error or BFD Down; back to
// 1 s for a hold timer expiry and for the FSM Error in OpenSent; kept for
// a closed TCP connection. A reset (ResetPeer) waits
// IdleHoldTimeAfterReset (idleHoldAfterReset). A leaf that ends the session (clear
// bgp, BFD down on the leaf, hold timer) and connects again when FRR's
// start timer expires (1 s after a TCP close, 2 s after a NOTIFICATION)
// can still be refused and then retries only after its connect timer. FRR
// accepts connections while it waits. A peer that is added again starts
// in Active at once: it accepts the leaf's connection and, with
// BGP_CONNECT_AT_ONCE (ConnectAtOnce), connects itself at once.
// ConnectAtOnce does not replace the restart: it shortens the first
// connect after Idle, but GoBGP still holds the peer in Idle and refuses
// the leaf meanwhile.
type peerRestarter struct {
	// peers returns the GoBGP peer addresses whose host is addr: the peer
	// state events carry the address without the zone of a link-local
	// peer, so two unnumbered peers can share a host.
	peers func(addr string) []string
	// restart starts the peer at addr again if it still needs it (see
	// restartPeer) and reports whether it did.
	restart func(ctx context.Context, addr string) (bool, error)
	log     *slog.Logger

	mu      sync.Mutex // guards stopped and wg.Add
	stopped bool
	wg      sync.WaitGroup
}

// handle takes a peer state event. GoBGP calls it from one goroutine.
func (r *peerRestarter) handle(ctx context.Context, p *apipb.Peer) {
	if p.GetState().GetSessionState() != apipb.PeerState_SESSION_STATE_IDLE || p.GetState().GetAdminState() != apipb.PeerState_ADMIN_STATE_UP {
		return
	}
	host := p.GetState().GetNeighborAddress()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for _, addr := range r.peers(host) {
			restarted, err := r.restart(ctx, addr)
			if err != nil {
				r.log.Warn("start the BGP peer again", "neighbor", addr, "error", err)
				continue
			}
			if restarted {
				r.log.Info("BGP session ended, peer started again at once like FRR", "neighbor", addr)
			}
		}
	}()
}

// stop waits for the restarts in progress; later events are ignored.
func (r *peerRestarter) stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.wg.Wait()
}

// startRestarter starts the peer restarter until Teardown.
func (u *UnderlayTier) startRestarter() error {
	r := &peerRestarter{peers: u.peersOfHost, restart: u.restartPeer, log: u.log}
	ctx, cancel := context.WithCancel(context.Background())
	err := u.bgp.WatchEvent(ctx, server.WatchEventMessageCallbacks{
		OnPeerUpdate: func(e *apiutil.WatchEventMessage_PeerEvent, _ time.Time) {
			if e.Type == apiutil.PEER_EVENT_STATE {
				r.handle(ctx, peerStateEvent(&e.Peer.State))
			}
		},
	}, server.WatchPeer())
	if err != nil {
		cancel()
		return fmt.Errorf("watch BGP peer states: %w", err)
	}
	u.stopRestart = func() {
		cancel()
		r.stop()
	}
	return nil
}

// peerStateEvent is the API form of a GoBGP v4 peer state event, with the
// neighbor address without zone like the v3 events. The API session
// state is the FSM state plus one, as in GoBGP's gRPC server
// (pkg/server/grpc_server.go:441).
func peerStateEvent(s *apiutil.PeerState) *apipb.Peer {
	return &apipb.Peer{State: &apipb.PeerState{
		NeighborAddress: s.NeighborAddress.WithZone("").String(),
		SessionState:    apipb.PeerState_SessionState(int(s.SessionState) + 1),
		AdminState:      s.AdminState,
	}}
}

// peersOfHost returns the added peers whose address without zone is host.
func (u *UnderlayTier) peersOfHost(host string) []string {
	u.peerOps.Lock()
	defer u.peerOps.Unlock()
	var out []string
	for addr := range u.peerConfs {
		if hostOf(addr) == host {
			out = append(out, addr)
		}
	}
	return out
}

// needsRestart reports whether the peer at addr ended a session and waits
// in Idle while BOOTy keeps it up: it was Established once (it has an
// uptime), it is in Idle, not administratively down (carrier loss, BOOTy
// stops), and GoBGP keeps no graceful restart routes of it. The caller
// holds peerOps, so the state cannot change between the check and the
// restart through BOOTy.
func (u *UnderlayTier) needsRestart(ctx context.Context, addr string) bool {
	needs := false
	_ = u.bgp.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
		needs = p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_IDLE && p.GetState().GetAdminState() == apipb.PeerState_ADMIN_STATE_UP &&
			p.GetTimers().GetState().GetUptime() != nil && !keepsGracefulRestartRoutes(p)
	})
	return needs
}

// keepsGracefulRestartRoutes reports whether GoBGP keeps routes of the
// peer as stale for its graceful restart: the peer restarts and sent
// address family tuples. Deleting the peer would drop them.
func keepsGracefulRestartRoutes(p *apipb.Peer) bool {
	if !p.GetGracefulRestart().GetPeerRestarting() {
		return false
	}
	for _, af := range p.GetAfiSafis() {
		if af.GetMpGracefulRestart().GetState().GetReceived() {
			return true
		}
	}
	return false
}

// restartPeer deletes the peer at addr and adds it again as it was
// added, if it needs a restart (needsRestart). It reports whether it did.
func (u *UnderlayTier) restartPeer(ctx context.Context, addr string) (bool, error) {
	u.peerOps.Lock()
	defer u.peerOps.Unlock()
	conf := u.peerConfs[addr]
	if conf == nil || !u.needsRestart(ctx, addr) {
		return false, nil // moved by the relearner, or no longer waiting
	}
	if err := u.bgp.DeletePeer(ctx, &apipb.DeletePeerRequest{Address: addr}); err != nil {
		return false, fmt.Errorf("delete peer %s: %w", addr, err)
	}
	peer, _ := proto.Clone(conf).(*apipb.Peer)
	if err := u.bgp.AddPeer(ctx, &apipb.AddPeerRequest{Peer: peer}); err != nil {
		// A retry does not help: the same config was added before. The
		// peer stays down until BOOTy starts again.
		u.log.Error("BGP peer deleted but not added again, it stays down", "neighbor", addr, "error", err)
		return false, fmt.Errorf("add peer %s: %w", addr, err)
	}
	return true, nil
}

// startStoppedPeer starts the peer at addr again when the carrier of its
// interface returns; the caller holds peerOps.
//
// GoBGP's EnablePeer starts the idle hold timer of the peer, and the peer
// closes the leaf's connections until it expires. With GoBGP 4.10.0 its
// length depended on the order of BFD loss and carrier loss (1 s or the
// fixed 5 s). With the GoBGP fork tag v4.10.0-booty.21 an administrative
// shutdown sets the idle hold time to 1 s, also for a peer that already
// waits in Idle, like "no neighbor shutdown" in FRR. A reset of the
// stopped peer sends nothing and sets IdleHoldTimeAfterReset for the next
// start (osrg/gobgp#3740). Since booty.21 the reset is redundant with
// BOOTy's IdleHoldTimeAfterReset of 1 s (the carrier tests pass without
// it); it is kept so that the wait stays IdleHoldTimeAfterReset if that
// value changes.
//
// The peer is not deleted and added again, which would start it at once:
// it then connects 1.5 to 2 s after the carrier, when the leaf (FRR)
// connects too. GoBGP up to the fork tag booty.17 took a connection that
// completes after the session is up as a graceful restart of the leaf
// whenever the leaf sent the graceful restart capability, and ended the
// session (production template lab). Since booty.18 and booty.19 it does
// that only when the leaf sent address family tuples in the current
// session, like RFC 4724 section 5 and FRR (bgp_accept,
// PEER_STATUS_NSF_MODE), so helper-only leaves are not affected. Not
// deleting the peer also keeps the stale routes of a leaf in graceful
// restart, as FRR keeps those of a peer in NSF wait across interface down
// and up (bgp_stop in bgp_fsm.c).
//
// A peer that is not administratively down (the carrier loss did not stop
// it, so its session may still be up) is left alone. Up to the fork tag
// booty.19, EnablePeer on an enabled GoBGP peer restarted the running idle
// hold timer in Idle, logged "code logic bug" in Active, OpenSent and
// OpenConfirm, and flipped the peer's AdminDown state flag; booty.20 makes
// it a no-op. stopPeer waits until GoBGP has applied the stop, so the
// admin state read here is not a stop that is still queued.
func (u *UnderlayTier) startStoppedPeer(ctx context.Context, addr string) error {
	stopped := false
	if err := u.bgp.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
		stopped = p.GetState().GetAdminState() == apipb.PeerState_ADMIN_STATE_DOWN
	}); err != nil {
		return fmt.Errorf("list peer %s: %w", addr, err)
	}
	if !stopped {
		return nil
	}
	if err := u.bgp.ResetPeer(ctx, &apipb.ResetPeerRequest{Address: addr}); err != nil {
		return fmt.Errorf("reset peer %s: %w", addr, err)
	}
	if err := u.bgp.EnablePeer(ctx, &apipb.EnablePeerRequest{Address: addr}); err != nil {
		return fmt.Errorf("enable peer %s: %w", addr, err)
	}
	return u.waitAdminState(ctx, addr, apipb.PeerState_ADMIN_STATE_UP)
}

// stopPeer stops the peer at addr (DisablePeer) and waits until GoBGP has
// applied it; the caller holds peerOps. DisablePeer and EnablePeer only
// queue the change for the FSM, so an admin state read before the FSM
// took the change is the old one. Up to the fork tag booty.19 GoBGP also
// dropped a change while another one was queued, and a stop or start
// right after another one could be lost: the peer stayed down for good
// after a fast carrier flap, or up after down, up, down. Since booty.20
// the latest change wins, so the wait is belt and braces for that.
// startStoppedPeer waits for its start the same way.
func (u *UnderlayTier) stopPeer(ctx context.Context, addr string) error {
	if err := u.bgp.DisablePeer(ctx, &apipb.DisablePeerRequest{Address: addr, Communication: "interface down"}); err != nil {
		return fmt.Errorf("disable peer %s: %w", addr, err)
	}
	return u.waitAdminState(ctx, addr, apipb.PeerState_ADMIN_STATE_DOWN)
}

// waitAdminState waits until the peer at addr has the admin state want,
// at most stopWait.
func (u *UnderlayTier) waitAdminState(ctx context.Context, addr string, want apipb.PeerState_AdminState) error {
	for deadline := time.Now().Add(stopWait); ; time.Sleep(5 * time.Millisecond) {
		state := apipb.PeerState_ADMIN_STATE_UNSPECIFIED
		if err := u.bgp.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
			state = p.GetState().GetAdminState()
		}); err != nil {
			return fmt.Errorf("list peer %s: %w", addr, err)
		}
		if state == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("peer %s: admin state %v, not %v, %v after the change", addr, state, want, stopWait)
		}
	}
}

// stopWait bounds the wait for an admin state change. GoBGP applies one
// within a few milliseconds (at most 10 ms seen in 250 stops and starts with
// the race detector); peerOps is held meanwhile.
const stopWait = 200 * time.Millisecond
