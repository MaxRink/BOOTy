//go:build linux

package bfd

import (
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// fakeConn is the send socket of a session in the test: every packet goes
// to the fake peer.
type fakeConn struct{ out chan Packet }

func (c *fakeConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	p, err := Parse(b)
	if err == nil {
		select {
		case c.out <- p:
		default:
		}
	}
	return len(b), nil
}
func (c *fakeConn) ReadFrom([]byte) (int, net.Addr, error) { select {} }
func (c *fakeConn) Close() error                           { return nil }
func (c *fakeConn) LocalAddr() net.Addr                    { return &net.UDPAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error            { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error        { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error       { return nil }

// fakeBFDD is a peer with the timer rules of FRR bfdd (bfd.c, bfd_packet.c):
// slow timers (1 s) until Up, then a Poll with the configured ones that
// apply after the Final (cur_timers); detection = remote multiplier x
// max(current Required Min RX, remote Desired Min TX), from every packet.
type fakeBFDD struct {
	mu                           sync.Mutex
	state                        State
	remoteDiscr                  uint32
	curTx, curRx, fast           time.Duration
	polling, sendFinal           bool
	remoteDesiredTx, remoteMinRx time.Duration
	remoteMult                   uint8
	lastRx                       time.Time
	downs                        int
}

func (f *fakeBFDD) packet() Packet {
	p := Packet{State: f.state, DetectMult: 3, MyDiscriminator: 99, YourDiscriminator: f.remoteDiscr,
		DesiredMinTxUS: toUS(f.curTx), RequiredMinRxUS: toUS(f.curRx)}
	if f.polling {
		p.DesiredMinTxUS, p.RequiredMinRxUS, p.Poll = toUS(f.fast), toUS(f.fast), true
	}
	if f.sendFinal {
		p.Final, p.Poll, f.sendFinal = true, false, false
	}
	return p
}

func (f *fakeBFDD) receive(q *Packet) (reply bool) {
	f.remoteDiscr = q.MyDiscriminator
	f.remoteDesiredTx, f.remoteMinRx, f.remoteMult = us(q.DesiredMinTxUS), us(q.RequiredMinRxUS), q.DetectMult
	f.lastRx = time.Now()
	old := f.state
	switch {
	case f.state == StateDown && q.State == StateDown:
		f.state = StateInit
	case f.state == StateDown && q.State == StateInit, f.state == StateInit && q.State >= StateInit:
		f.state, f.polling = StateUp, true
	case f.state == StateUp && q.State == StateDown:
		f.state = StateDown
	}
	if f.polling && q.Final {
		f.polling, f.curTx, f.curRx = false, f.fast, f.fast
	}
	if q.Poll {
		f.sendFinal = true
	}
	return f.state != old || f.sendFinal
}

func (f *fakeBFDD) detection() time.Duration {
	return time.Duration(f.remoteMult) * max(f.curRx, f.remoteDesiredTx)
}

// A session against a peer that behaves like FRR bfdd with the fabric
// timers stays Up: the peer detects in 450 ms once our Final completes its
// Poll, so our next packet must follow its lower Required Min RX at once.
// Before, BOOTy kept the 1 s timer and the peer went Down about 450 ms
// after every Up (kindmetal flap storm).
func TestManagerAgainstFakeBFDD(t *testing.T) {
	m := NewManager(slog.Default(), nil)
	t.Cleanup(func() { closeWithin(t, m) })
	conn := &fakeConn{out: make(chan Packet, 64)}
	r := &run{
		peer: Peer{Name: "leaf", Addr: netip.MustParseAddr("192.0.2.1"), Timers: Timers{
			DesiredMinTx: 150 * time.Millisecond, RequiredMinRx: 150 * time.Millisecond, DetectMult: 3}},
		sess: NewSession(Timers{DesiredMinTx: 150 * time.Millisecond, RequiredMinRx: 150 * time.Millisecond, DetectMult: 3}, 11),
		tx:   conn,
		rx:   make(chan Packet, 16),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	m.wg.Add(1)
	go m.loop(r)

	f := &fakeBFDD{state: StateDown, curTx: time.Second, curRx: time.Second, fast: 150 * time.Millisecond, remoteMult: 3}
	send := func() {
		p := f.packet()
		select {
		case r.rx <- p:
		default:
		}
	}
	var upAt time.Time
	deadline := time.After(10 * time.Second)
	tx := time.NewTimer(0)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case q := <-conn.out:
			f.mu.Lock()
			if f.receive(&q) {
				send()
			}
			if f.state == StateUp && upAt.IsZero() {
				upAt = time.Now()
			}
			f.mu.Unlock()
		case <-tx.C:
			f.mu.Lock()
			send()
			iv := max(f.curTx, f.remoteMinRx)
			if f.polling {
				iv = max(f.fast, f.remoteMinRx)
			}
			f.mu.Unlock()
			tx.Reset(time.Duration(float64(iv) * (0.75 + rand.Float64()*0.25))) //nolint:gosec // jitter
		case <-tick.C:
			f.mu.Lock()
			if f.state != StateDown && !f.lastRx.IsZero() && time.Since(f.lastRx) > f.detection() {
				f.state, f.polling = StateDown, false
				f.curTx, f.curRx = time.Second, time.Second
				f.downs++
				t.Logf("peer: no packet for %s (detection %s), Down", time.Since(f.lastRx).Round(time.Millisecond), f.detection())
				send()
			}
			f.mu.Unlock()
		case <-deadline:
			f.mu.Lock()
			defer f.mu.Unlock()
			if upAt.IsZero() || f.downs > 0 || f.state != StateUp {
				t.Fatalf("peer state %s, Up at %v, %d downs; want Up without a down", f.state, upAt, f.downs)
			}
			return
		}
	}
}
