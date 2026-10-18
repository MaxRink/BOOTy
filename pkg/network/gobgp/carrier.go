//go:build linux

package gobgp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/server"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// carrierWatch ends the session of an unnumbered peer as soon as its
// interface loses carrier and starts it again when the carrier returns.
// FRR does this for directly connected eBGP peers ("bgp
// fast-external-failover", on by default): zebra reports the interface
// down and bgpd stops the peer, so its routes are withdrawn at once
// instead of after the hold time.
type carrierWatch struct {
	peers map[string]string // interface -> peer address as added to GoBGP
	// current returns the peer address now, after the relearner moved it
	// to a new link-local address; nil keeps the address of peers.
	current func(iface string) string
	down    map[string]bool
	// disable and enable stop and start a peer; GoBGP by default.
	disable, enable func(ctx context.Context, addr string) error
	// announce sends a Router Advertisement on an interface whose carrier
	// came back. An FRR unnumbered peer forgets the neighbor address on
	// interface down and refuses our connect until it gets an RA; zebra
	// sends one at once on link up, the periodic RA comes up to 10 s later.
	announce func(iface string)
	// raRepeat sends the RA again after this time, in case the link-local
	// address was still tentative; 0 sends it once. The timers stop with
	// the watch.
	raRepeat time.Duration
	raTimers map[string]*time.Timer
	log      *slog.Logger
}

func newCarrierWatch(bgp *server.BgpServer, peers map[string]string, log *slog.Logger) *carrierWatch {
	return &carrierWatch{
		peers: peers,
		down:  map[string]bool{},
		disable: func(ctx context.Context, addr string) error {
			return bgp.DisablePeer(ctx, &apipb.DisablePeerRequest{Address: addr, Communication: "interface down"}) //nolint:wrapcheck // logged with the peer
		},
		enable: func(ctx context.Context, addr string) error {
			return bgp.EnablePeer(ctx, &apipb.EnablePeerRequest{Address: addr}) //nolint:wrapcheck // logged with the peer
		},
		announce: sendRouterAdvertisement,
		// GoBGP connects 2 to 3 s after the enable.
		raRepeat: time.Second,
		log:      log,
	}
}

// start subscribes to link updates. Closing stop ends the watch; the
// returned channel is closed when the last update has been handled.
func (w *carrierWatch) start(stop chan struct{}) (<-chan struct{}, error) {
	updates := make(chan netlink.LinkUpdate, 16)
	if err := netlink.LinkSubscribeWithOptions(updates, stop, netlink.LinkSubscribeOptions{
		ErrorCallback: func(err error) {
			select {
			case <-stop: // the subscription ends with an error when stopped
			default:
				w.log.Warn("link updates", "error", err)
			}
		},
	}); err != nil {
		return nil, fmt.Errorf("subscribe to link updates: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer w.stopRATimers()
		for u := range updates {
			w.handleUpdate(u)
		}
	}()
	return done, nil
}

// handleUpdate maps a netlink link update to the carrier state:
// IFF_LOWER_UP is the carrier.
func (w *carrierWatch) handleUpdate(u netlink.LinkUpdate) {
	if u.Link == nil {
		return
	}
	w.handle(u.Attrs().Name, u.Flags&unix.IFF_LOWER_UP != 0)
}

func (w *carrierWatch) handle(iface string, carrier bool) {
	addr, ok := w.peers[iface]
	if !ok || w.down[iface] == !carrier {
		return
	}
	w.down[iface] = !carrier
	if w.current != nil {
		addr = w.current(iface)
	}
	ctx := context.Background()
	if !carrier {
		w.log.Info("uplink lost carrier, stopping its BGP session", "interface", iface, "neighbor", addr)
		if err := w.disable(ctx, addr); err != nil {
			w.log.Warn("stop BGP session", "neighbor", addr, "error", err)
		}
		return
	}
	w.log.Info("uplink has carrier again, starting its BGP session", "interface", iface, "neighbor", addr)
	w.announce(iface)
	if w.raRepeat > 0 {
		if w.raTimers == nil {
			w.raTimers = map[string]*time.Timer{}
		}
		if t := w.raTimers[iface]; t != nil {
			t.Stop()
		}
		w.raTimers[iface] = time.AfterFunc(w.raRepeat, func() { w.announce(iface) })
	}
	if err := w.enable(ctx, addr); err != nil {
		w.log.Warn("start BGP session", "neighbor", addr, "error", err)
	}
}

// stopRATimers stops the repeated RAs; it runs on the update goroutine.
func (w *carrierWatch) stopRATimers() {
	for _, t := range w.raTimers {
		t.Stop()
	}
}
