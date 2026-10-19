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
// while BOOTy keeps the peer up. GoBGP then holds the peer 5 s in Idle
// (holdtimeIdle, pkg/server/fsm.go) and closes every connection that the
// peer opens meanwhile, so a leaf that ends the session (clear bgp, BFD
// down on the leaf, hold timer) and connects again when FRR's start timer
// expires (1 s after a TCP close, 2 s after a NOTIFICATION) is refused and retries only after its connect timer
// (osrg/gobgp#3735; lab: 36 s against 2.3 s for FRR). FRR accepts
// connections while it waits. A peer that is added again starts in Active
// at once: it accepts the leaf's connection and, with BGP_CONNECT_AT_ONCE
// (ConnectAtOnce), connects itself at once. ConnectAtOnce does not replace the
// restart: it shortens the first connect after Idle, but GoBGP still holds
// the peer 5 s in Idle and refuses the leaf meanwhile.
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
		return false, fmt.Errorf("add peer %s: %w", addr, err)
	}
	return true, nil
}
