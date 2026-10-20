//go:build linux

package lldp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// fakeLinks replaces the netlink calls of watchCarrier: linkByName reads
// links, an update goes to the current subscription.
type fakeLinks struct {
	mu    sync.Mutex
	links map[string]netlink.LinkAttrs
	sub   chan<- netlink.LinkUpdate
	subs  int
}

func upAttrs(index int, name string) netlink.LinkAttrs {
	return netlink.LinkAttrs{Index: index, Name: name, OperState: netlink.OperUp}
}

func downAttrs(index int, name string) netlink.LinkAttrs {
	return netlink.LinkAttrs{Index: index, Name: name, OperState: netlink.OperDown}
}

func (f *fakeLinks) install(t *testing.T) {
	t.Helper()
	origSub, origByName := linkSubscribe, linkByName
	t.Cleanup(func() { linkSubscribe, linkByName = origSub, origByName })
	linkSubscribe = func(ch chan<- netlink.LinkUpdate, done <-chan struct{}, _ netlink.LinkSubscribeOptions) error {
		f.mu.Lock()
		f.sub, f.subs = ch, f.subs+1
		f.mu.Unlock()
		go func() {
			<-done
			f.drop(ch)
		}()
		return nil
	}
	linkByName = func(name string) (netlink.Link, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		a, ok := f.links[name]
		if !ok {
			return nil, errors.New("Link not found")
		}
		return &netlink.Device{LinkAttrs: a}, nil
	}
}

// drop ends the subscription ch, like a receive error (ENOBUFS).
func (f *fakeLinks) drop(ch chan<- netlink.LinkUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sub == ch && ch != nil {
		close(ch)
		f.sub = nil
	}
}

// set changes a link without an update, as when the update was lost.
func (f *fakeLinks) set(a netlink.LinkAttrs) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.links == nil {
		f.links = map[string]netlink.LinkAttrs{}
	}
	f.links[a.Name] = a
}

// update changes a link and sends the update.
func (f *fakeLinks) update(typ uint16, a netlink.LinkAttrs) {
	f.set(a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if typ == unix.RTM_DELLINK {
		delete(f.links, a.Name)
	}
	f.sub <- netlink.LinkUpdate{Header: unix.NlMsghdr{Type: typ}, Link: &netlink.Device{LinkAttrs: a}}
}

// rename renames a link and sends the update.
func (f *fakeLinks) rename(from string, a netlink.LinkAttrs) {
	f.mu.Lock()
	delete(f.links, from)
	f.mu.Unlock()
	f.update(unix.RTM_NEWLINK, a)
}

// sent is a frame that the fake sendFrame got.
type sent struct {
	at  time.Time
	ttl uint16
}

// fakeSend replaces sendFrame; it records each frame and fails when fail
// is set.
type fakeSend struct {
	mu     sync.Mutex
	frames []sent
	fail   error
}

func (s *fakeSend) install(t *testing.T) {
	t.Helper()
	orig := sendFrame
	t.Cleanup(func() { sendFrame = orig })
	sendFrame = func(iface string, info *Info, build func(string, net.HardwareAddr, *Info) ([]byte, error)) ([]byte, error) {
		b, err := build(iface, net.HardwareAddr{2, 0, 0, 0, 0, 1}, info)
		if err != nil {
			return nil, err
		}
		l, _ := gopacket.NewPacket(b, layers.LayerTypeEthernet, gopacket.Default).Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.frames = append(s.frames, sent{time.Now(), l.TTL})
		if s.fail != nil {
			return nil, s.fail
		}
		return b, nil
	}
}

// since returns the times of the frames with TTL ttl at or after t0.
func (s *fakeSend) since(t0 time.Time, ttl uint16) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var at []time.Time
	for _, f := range s.frames {
		if !f.at.Before(t0) && f.ttl == ttl {
			at = append(at, f.at)
		}
	}
	return at
}

// requireNetworkdTiming checks frames after a carrier gain at t0 against
// networkd 249: the first 0.8 to 1.2 s after the gain, 3 more 0.8 to
// 1.2 s apart, then 29.8 to 30.2 s apart. Every delay has jitter: one
// without (exactly 0.8 or 29.8 s) has a chance of 1 in 400 million.
func requireNetworkdTiming(t *testing.T, t0 time.Time, at []time.Time, want int) {
	t.Helper()
	if len(at) != want {
		t.Fatalf("%d frames after the carrier gain, want %d: %v", len(at), want, at)
	}
	const jitter = 400 * time.Millisecond
	prev := t0
	for i, a := range at {
		d := a.Sub(prev)
		lo := 800 * time.Millisecond
		if i >= 4 {
			lo = 29800 * time.Millisecond
		}
		if d <= lo || d >= lo+jitter {
			t.Fatalf("frame %d %v after the one before, want %v to %v", i+1, d, lo, lo+jitter)
		}
		prev = a
	}
}

// emitTest runs Emit on eth0 in a synctest bubble with fake links and a
// fake sender; it returns the log.
func emitTest(t *testing.T, f *fakeLinks, s *fakeSend, info *Info, run func(cancel func())) string {
	t.Helper()
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	synctest.Test(t, func(t *testing.T) {
		testState(t, 0)
		f.install(t)
		s.install(t)
		ctx, cancel := context.WithCancel(context.Background())
		log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
			logMu.Lock()
			defer logMu.Unlock()
			return logBuf.Write(p)
		}), nil))
		wait := Emit(ctx, []string{"eth0"}, info, log)
		t.Cleanup(func() { cancel(); wait() }) // also after a t.Fatal
		synctest.Wait()
		run(cancel)
	})
	return logBuf.String()
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func txState(t *testing.T) TxState {
	t.Helper()
	live.Lock()
	defer live.Unlock()
	for _, s := range live.tx {
		if s.Interface == "eth0" {
			return s
		}
	}
	t.Fatal("no transmit state for eth0")
	return TxState{}
}

// The timing of networkd 249 at the start, after a carrier loss and after
// the carrier gain; without carrier nothing is sent or counted, and the
// state says so.
func TestEmitNetworkdTiming(t *testing.T) {
	var f fakeLinks
	var s fakeSend
	f.set(upAttrs(2, "eth0"))
	emitTest(t, &f, &s, &Info{MachineID: "m", SystemName: "host1"}, func(_ func()) {
		t0 := time.Now()
		time.Sleep(100 * time.Second)
		requireNetworkdTiming(t, t0, s.since(t0, txTTL), 7)

		f.update(unix.RTM_NEWLINK, downAttrs(2, "eth0"))
		down := time.Now()
		time.Sleep(100 * time.Second)
		if at := s.since(down, txTTL); len(at) != 0 {
			t.Fatalf("frames without carrier at %v", at)
		}
		if st := txState(t); st.Error != "no carrier" || st.Frames != 7 {
			t.Fatalf("state without carrier %+v, want no carrier and 7 frames", st)
		}

		f.update(unix.RTM_NEWLINK, upAttrs(2, "eth0"))
		up := time.Now()
		time.Sleep(40 * time.Second)
		requireNetworkdTiming(t, up, s.since(up, txTTL), 5)
		// A repeated update with carrier is no carrier gain.
		f.update(unix.RTM_NEWLINK, upAttrs(2, "eth0"))
		again := time.Now()
		time.Sleep(10 * time.Second)
		if at := s.since(again, txTTL); len(at) != 0 {
			t.Fatalf("frames %v after an update without a carrier change", at)
		}
	})
}

// A NIC that is missing or renamed is no NIC with carrier: no frames, a
// warning once per absence, and the error in the state.
func TestEmitMissingNIC(t *testing.T) {
	var f fakeLinks
	var s fakeSend
	log := emitTest(t, &f, &s, &Info{MachineID: "m", SystemName: "host1"}, func(_ func()) {
		time.Sleep(10 * time.Second)
		if len(s.since(time.Time{}, txTTL)) != 0 {
			t.Fatal("frames on a missing NIC")
		}
		if st := txState(t); !strings.Contains(st.Error, "interface eth0") {
			t.Fatalf("state of a missing NIC %+v", st)
		}
		f.update(unix.RTM_NEWLINK, upAttrs(3, "eth0"))
		up := time.Now()
		time.Sleep(10 * time.Second)
		requireNetworkdTiming(t, up, s.since(up, txTTL), 4)

		f.rename("eth0", upAttrs(3, "eth9"))
		gone := time.Now()
		time.Sleep(60 * time.Second)
		if at := s.since(gone, txTTL); len(at) != 0 {
			t.Fatalf("frames after the rename at %v", at)
		}
		if st := txState(t); !strings.Contains(st.Error, "renamed to eth9") {
			t.Fatalf("state after the rename %+v", st)
		}
	})
	if n := strings.Count(log, "LLDP frame not sent"); n != 2 {
		t.Fatalf("%d warnings, want one per absence (2):\n%s", n, log)
	}
}

// A failed send counts like in networkd: the fast frames end after 4
// attempts. It is logged once.
func TestEmitFailedSendCounts(t *testing.T) {
	var f fakeLinks
	s := fakeSend{fail: errors.New("send on eth0: no buffer space")}
	f.set(upAttrs(2, "eth0"))
	log := emitTest(t, &f, &s, &Info{MachineID: "m", SystemName: "host1"}, func(_ func()) {
		t0 := time.Now()
		time.Sleep(50 * time.Second)
		requireNetworkdTiming(t, t0, s.since(t0, txTTL), 5)
	})
	if n := strings.Count(log, "LLDP frame not sent"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, log)
	}
}

// A lost update (ENOBUFS ends the subscription): the watch subscribes
// again and reads the carrier again.
func TestEmitResubscribes(t *testing.T) {
	var f fakeLinks
	var s fakeSend
	f.set(upAttrs(2, "eth0"))
	emitTest(t, &f, &s, &Info{MachineID: "m", SystemName: "host1"}, func(_ func()) {
		time.Sleep(10 * time.Second)
		f.set(downAttrs(2, "eth0"))
		f.mu.Lock()
		ch := f.sub
		f.mu.Unlock()
		f.drop(ch)
		synctest.Wait()
		lost := time.Now()
		time.Sleep(60 * time.Second)
		if at := s.since(lost, txTTL); len(at) != 0 {
			t.Fatalf("frames after the carrier loss seen on the new subscription: %v", at)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.subs != 2 {
			t.Fatalf("%d subscriptions, want 2", f.subs)
		}
	})
}

// The shutdown LLDPDU goes out when the sending stops, but not on a NIC
// without carrier.
func TestEmitShutdownFrame(t *testing.T) {
	for _, up := range []bool{true, false} {
		var f fakeLinks
		var s fakeSend
		attrs := upAttrs(2, "eth0")
		if !up {
			attrs = downAttrs(2, "eth0")
		}
		f.set(attrs)
		emitTest(t, &f, &s, &Info{MachineID: "m", SystemName: "host1"}, func(cancel func()) {
			time.Sleep(5 * time.Second)
			cancel()
			synctest.Wait()
		})
		if got := len(s.since(time.Time{}, 0)); got != map[bool]int{true: 1, false: 0}[up] {
			t.Errorf("carrier %v: %d shutdown frames", up, got)
		}
	}
}

// The carrier is link_has_carrier of networkd 249: operstate, and the
// flags only when the driver does not report an operstate.
func TestLinkHasCarrier(t *testing.T) {
	const lowerRunning = unix.IFF_UP | unix.IFF_LOWER_UP | unix.IFF_RUNNING
	for _, tt := range []struct {
		oper  netlink.LinkOperState
		flags uint32
		want  bool
	}{
		{netlink.OperUp, 0, true},
		{netlink.OperDown, lowerRunning, false},
		{netlink.OperLowerLayerDown, lowerRunning, false},
		{netlink.OperDormant, lowerRunning, false},
		{netlink.OperUnknown, lowerRunning, true},
		{netlink.OperUnknown, unix.IFF_UP | unix.IFF_LOWER_UP, false},
		{netlink.OperUnknown, unix.IFF_UP | unix.IFF_RUNNING, false},
		{netlink.OperUnknown, lowerRunning | unix.IFF_DORMANT, false},
	} {
		if got := linkHasCarrier(&netlink.LinkAttrs{OperState: tt.oper, RawFlags: tt.flags}); got != tt.want {
			t.Errorf("operstate %v flags %#x: carrier %v, want %v", tt.oper, tt.flags, got, tt.want)
		}
	}
}

// A start on a running timer (link_lldp_emit_start) sets the fast frames
// back to 4 and only lowers the timeout.
func TestTxTimerStartWhileRunning(t *testing.T) {
	now := time.Unix(1000, 0)
	var tx txTimer
	tx.start(now)
	if d := tx.next.Sub(now); tx.fast != txFastInit || d < txFastInterval || d >= txFastInterval+txJitter {
		t.Fatalf("start: fast %d, next in %v", tx.fast, d)
	}
	for range txFastInit {
		now = tx.next
		tx.fired(now)
	}
	if d := tx.next.Sub(now); tx.fast != 0 || d < txInterval {
		t.Fatalf("after the fast frames: fast %d, next in %v", tx.fast, d)
	}
	now = now.Add(time.Second)
	tx.start(now) // the 30 s timeout is lowered
	if d := tx.next.Sub(now); tx.fast != txFastInit || d >= txFastInterval+txJitter {
		t.Fatalf("start in the slow phase: fast %d, next in %v", tx.fast, d)
	}
	early := now.Add(100 * time.Millisecond)
	tx.next = early
	tx.start(now) // an earlier timeout stays
	if tx.next != early {
		t.Fatalf("start lowered nothing but moved the timeout to %v", tx.next)
	}
	tx.stop()
	if !tx.next.IsZero() {
		t.Fatal("stop left the timer running")
	}
}

// A record without a frame and with the same error as before changes
// nothing and does not write the state file.
func TestRecordTXUnchanged(t *testing.T) {
	testState(t, 0)
	recordTX("eth0", &Info{}, nil, nil, time.Now())
	if err := os.Remove(StatePath); err != nil {
		t.Fatal(err)
	}
	recordTX("eth0", &Info{}, nil, nil, time.Now())
	if _, err := os.Stat(StatePath); err == nil {
		t.Fatal("state file written again without a change")
	}
	recordTX("eth0", &Info{}, nil, errors.New("interface eth0: deleted"), time.Now())
	if s, _ := readState(StatePath, time.Now()); len(s.Transmit) != 1 || s.Transmit[0].Error != "interface eth0: deleted" {
		t.Fatalf("changed state not written: %+v", s)
	}
}

// The jitter of networkd: uniform in [0, 400 ms). 1000 samples fill the
// range to within 40 ms at both ends, except with a chance below 1e-45.
func TestJitter(t *testing.T) {
	lo, hi := time.Hour, time.Duration(0)
	for range 1000 {
		j := jitter()
		if j < 0 || j >= 400*time.Millisecond {
			t.Fatalf("jitter %v, want [0, 400ms)", j)
		}
		lo, hi = min(lo, j), max(hi, j)
	}
	if lo > 40*time.Millisecond || hi < 360*time.Millisecond {
		t.Fatalf("jitter from %v to %v, want about 0 to 400ms", lo, hi)
	}
}

// Only the latest state of a NIC waits for its sender: a sender that
// reads late gets the current carrier, not an old one.
func TestWatchCarrierKeepsLatest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var f fakeLinks
		f.set(upAttrs(2, "eth0"))
		f.install(t)
		ch := make(chan linkState, 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			watchCarrier(ctx, map[string]chan linkState{"eth0": ch}, slog.Default())
			close(done)
		}()
		synctest.Wait() // read: up
		f.update(unix.RTM_NEWLINK, downAttrs(2, "eth0"))
		synctest.Wait()
		if s := <-ch; s.carrier || s.err != nil {
			t.Errorf("state %+v, want the latest: no carrier", s)
		}
		cancel()
		<-done
	})
}
