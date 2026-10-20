//go:build linux

package lldp

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// txTimer is the transmit timer of networkd 249 (networkd-lldp-tx.c) for
// one NIC.
type txTimer struct {
	fast int       // lldp_tx_fast: fast frames still to send
	next time.Time // the next frame; zero when stopped
}

// jitter is the random delay that networkd adds to each timeout.
func jitter() time.Duration {
	return rand.N(txJitter) //nolint:gosec // timing jitter, not a secret
}

// start is link_lldp_emit_start: the next txFastInit frames are fast, the
// first after txFastInterval plus jitter. On a running timer it only
// lowers the timeout.
func (t *txTimer) start(now time.Time) {
	t.fast = txFastInit
	next := now.Add(txFastInterval + jitter())
	if !t.next.IsZero() && !t.next.After(next) {
		return
	}
	t.next = next
}

// stop is link_lldp_emit_stop.
func (t *txTimer) stop() { t.next = time.Time{} }

// fired is on_lldp_timer after the send, failed or not: one fast frame
// less, then the next timeout from now.
func (t *txTimer) fired(now time.Time) {
	if t.fast > 0 {
		t.fast--
	}
	d := txInterval
	if t.fast > 0 {
		d = txFastInterval
	}
	t.next = now.Add(d + jitter())
}

// linkState is the carrier of a NIC, or why the NIC is missing.
type linkState struct {
	carrier bool
	err     error
}

// linkHasCarrier is link_has_carrier of networkd 249: operstate up, or
// operstate unknown (not implemented by the driver) with lower up and
// running and not dormant.
func linkHasCarrier(a *netlink.LinkAttrs) bool {
	switch a.OperState {
	case netlink.OperUp:
		return true
	case netlink.OperUnknown:
		const want = unix.IFF_LOWER_UP | unix.IFF_RUNNING
		return a.RawFlags&want == want && a.RawFlags&unix.IFF_DORMANT == 0
	}
	return false
}

// linkSubscribe and linkByName are the netlink calls of watchCarrier;
// tests replace them.
var (
	linkSubscribe = netlink.LinkSubscribeWithOptions
	linkByName    = netlink.LinkByName
)

// watchCarrier sends the state of each NIC in states to its channel (only
// the latest state waits there) until ctx ends: first as read, then on
// each link update. A deleted or renamed NIC is missing. When the
// subscription ends (ENOBUFS: the socket lost updates) it subscribes again
// and reads the state again.
func watchCarrier(ctx context.Context, states map[string]chan linkState, log *slog.Logger) {
	push := func(name string, s linkState) {
		ch := states[name]
		select {
		case <-ch:
		default:
		}
		ch <- s
	}
	for ctx.Err() == nil {
		if err := watchLinks(ctx, states, push, log); err != nil {
			log.Warn("link watch for LLDP not started, trying again", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

// watchLinks runs one subscription until it ends.
func watchLinks(ctx context.Context, states map[string]chan linkState, push func(string, linkState), log *slog.Logger) error {
	updates := make(chan netlink.LinkUpdate, 16)
	// One context per subscription, canceled when it ends: the netlink
	// fork (#833) closes the socket after a receive error itself; upstream
	// v1.3.1 keeps it until done closes.
	sub, end := context.WithCancel(ctx)
	defer end()
	opts := netlink.LinkSubscribeOptions{ErrorCallback: linkWatchError(sub, log)}
	if err := linkSubscribe(updates, sub.Done(), opts); err != nil {
		return fmt.Errorf("subscribe to link updates: %w", err)
	}
	// Read after subscribing: a change from then on is an update.
	names := map[int]string{} // index -> watched name
	for name := range states {
		l, err := linkByName(name)
		if err != nil {
			push(name, linkState{err: fmt.Errorf("interface %s: %w", name, err)})
			continue
		}
		names[l.Attrs().Index] = name
		push(name, linkState{carrier: linkHasCarrier(l.Attrs())})
	}
	for u := range updates {
		if u.Link == nil {
			continue
		}
		a := u.Attrs()
		if old, ok := names[a.Index]; ok && (old != a.Name || u.Header.Type == unix.RTM_DELLINK) {
			delete(names, a.Index)
			err := fmt.Errorf("interface %s: deleted", old)
			if old != a.Name {
				err = fmt.Errorf("interface %s: renamed to %s", old, a.Name)
			}
			push(old, linkState{err: err})
		}
		if _, ok := states[a.Name]; ok && u.Header.Type == unix.RTM_NEWLINK {
			names[a.Index] = a.Name
			push(a.Name, linkState{carrier: linkHasCarrier(a)})
		}
	}
	return nil
}

// linkWatchError logs a netlink error of the link watch: a failed receive
// ends the subscription (the watch subscribes again), a message of another
// sender is skipped, and the end of the subscription is no error.
func linkWatchError(sub context.Context, log *slog.Logger) func(error) {
	return func(err error) {
		switch {
		case sub.Err() != nil:
		case strings.Contains(err.Error(), "Wrong sender portid"):
			log.Debug("link watch for LLDP skipped a message", "error", err)
		default:
			log.Warn("link watch for LLDP failed, subscribing again", "error", err)
		}
	}
}
