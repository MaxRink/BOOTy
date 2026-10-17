//go:build linux

package gobgp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
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
	down  map[string]bool
	// disable and enable stop and start a peer; GoBGP by default.
	disable, enable func(ctx context.Context, addr string) error
	// announce sends a Router Advertisement on an interface whose carrier
	// came back. An FRR unnumbered peer forgets the neighbor address on
	// interface down and refuses our connect until it gets an RA; zebra
	// sends one at once on link up, the periodic RA comes up to 10 s later.
	announce func(iface string)
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
		announce: func(iface string) {
			sendRouterAdvertisement(iface)
			// Again after 1 s, in case the link-local address was still
			// tentative; GoBGP connects 2 to 3 s after the enable.
			time.AfterFunc(time.Second, func() { sendRouterAdvertisement(iface) })
		},
		log: log,
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
	if err := w.enable(ctx, addr); err != nil {
		w.log.Warn("start BGP session", "neighbor", addr, "error", err)
	}
}
