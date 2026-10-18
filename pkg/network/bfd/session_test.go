package bfd

import (
	"errors"
	"testing"
	"time"
)

var fabric = Timers{DesiredMinTx: 150 * time.Millisecond, RequiredMinRx: 150 * time.Millisecond, DetectMult: 3}

func remote(state State, your uint32) *Packet {
	return &Packet{State: state, DetectMult: 3, MyDiscriminator: 77, YourDiscriminator: your,
		DesiredMinTxUS: 300000, RequiredMinRxUS: 300000}
}

// RFC 5880 6.8.6, the state table of section 6.2.
func TestSessionTransitions(t *testing.T) {
	now := time.Unix(0, 0)
	tests := []struct {
		name       string
		start      []State // packets that bring the session to its start state
		rx         State
		want       State
		wantDiag   Diag
		wantChange bool
	}{
		{"down + down = init", nil, StateDown, StateInit, DiagNone, true},
		{"down + init = up", nil, StateInit, StateUp, DiagNone, true},
		{"down + up stays down", nil, StateUp, StateDown, DiagNone, false},
		{"down + admin down stays down", nil, StateAdminDown, StateDown, DiagNone, false},
		{"init + init = up", []State{StateDown}, StateInit, StateUp, DiagNone, true},
		{"init + up = up", []State{StateDown}, StateUp, StateUp, DiagNone, true},
		{"init + down stays init", []State{StateDown}, StateDown, StateInit, DiagNone, false},
		{"init + admin down = down", []State{StateDown}, StateAdminDown, StateDown, DiagNeighborSaysDown, true},
		{"up + down = down", []State{StateInit}, StateDown, StateDown, DiagNeighborSaysDown, true},
		{"up + admin down = down", []State{StateInit}, StateAdminDown, StateDown, DiagNeighborSaysDown, true},
		{"up + init stays up", []State{StateInit}, StateInit, StateUp, DiagNone, false},
		{"up + up stays up", []State{StateInit}, StateUp, StateUp, DiagNone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSession(fabric, 11)
			for _, st := range tt.start {
				s.Receive(remote(st, 11), now)
			}
			if got := s.Receive(remote(tt.rx, 11), now); got != tt.wantChange {
				t.Errorf("changed = %v, want %v", got, tt.wantChange)
			}
			if s.State() != tt.want || s.Diag() != tt.wantDiag {
				t.Errorf("state %s diag %d, want %s %d", s.State(), s.Diag(), tt.want, tt.wantDiag)
			}
		})
	}
}

func TestSessionIgnoresOtherDiscriminator(t *testing.T) {
	s := NewSession(fabric, 11)
	if s.Receive(remote(StateDown, 12), time.Now()) || s.State() != StateDown || s.RemoteDiscriminator() != 0 {
		t.Fatalf("packet for discriminator 12 changed session 11: %s %d", s.State(), s.RemoteDiscriminator())
	}
}

// RFC 5880 6.8.4: detection time = remote multiplier x max(Required Min
// RX in use, remote DesiredMinTx). The Required Min RX in use is 1 s until
// our Poll Sequence completes, like FRR bfdd. No packet in that time: Down
// with diagnostic 1.
func TestSessionDetectionTime(t *testing.T) {
	now := time.Unix(100, 0)
	s := NewSession(fabric, 11)
	// The detection time uses the multiplier of the remote system (RFC
	// 5880 6.8.4), here 4, not the local 3.
	p := remote(StateInit, 11)
	p.DetectMult = 4
	s.Receive(p, now)
	if got, want := s.DetectionTime(), 4*time.Second; got != want {
		t.Fatalf("detection time during the Poll Sequence = %s, want %s (remote 4 x max(1 s, 300ms))", got, want)
	}
	final := remote(StateUp, 11)
	final.DetectMult, final.Final = 4, true
	s.Receive(final, now)
	if got, want := s.DetectionTime(), 1200*time.Millisecond; got != want {
		t.Fatalf("detection time = %s, want %s (remote 4 x max(150ms, 300ms))", got, want)
	}
	if s.CheckTimeout(now.Add(1200 * time.Millisecond)) {
		t.Fatal("down at exactly the detection time")
	}
	if !s.CheckTimeout(now.Add(1201*time.Millisecond)) || s.State() != StateDown || s.Diag() != DiagTimeExpired {
		t.Fatalf("after the detection time: %s diag %d, want Down diag 1", s.State(), s.Diag())
	}
	if s.RemoteDiscriminator() != 0 {
		t.Fatal("remote discriminator kept after the timeout")
	}
	if s.CheckTimeout(now.Add(time.Hour)) {
		t.Fatal("a Down session timed out again")
	}
}

// Slow start at 1 s until Up, then the configured interval with a Poll
// Sequence (RFC 5880 6.8.3, 6.5); a received Poll is answered once with
// Final and without Poll.
func TestSessionPollFinal(t *testing.T) {
	now := time.Now()
	s := NewSession(fabric, 11)
	if p := s.Packet(); p.DesiredMinTxUS != 1000000 || p.Poll || p.RequiredMinRxUS != 1000000 {
		t.Fatalf("down packet = %+v, want 1 s slow start for both timers and no Poll", p)
	}
	s.Receive(remote(StateInit, 11), now)
	p := s.Packet()
	if !p.Poll || p.DesiredMinTxUS != 150000 || p.RequiredMinRxUS != 150000 || p.State != StateUp || p.YourDiscriminator != 77 {
		t.Fatalf("first Up packet = %+v, want Poll with 150 ms", p)
	}
	if got := s.TxInterval(); got != 300*time.Millisecond {
		t.Fatalf("tx interval = %s, want max(150 ms, remote 300 ms)", got)
	}
	poll := remote(StateUp, 11)
	poll.Poll = true
	s.Receive(poll, now)
	if p := s.Packet(); !p.Final || p.Poll {
		t.Fatalf("answer to Poll = %+v, want Final without Poll", p)
	}
	if p := s.Packet(); !p.Poll || p.Final {
		t.Fatalf("after the Final = %+v, want our Poll again", p)
	}
	final := remote(StateUp, 11)
	final.Final = true
	s.Receive(final, now)
	if p := s.Packet(); p.Poll || p.Final || p.RequiredMinRxUS != 150000 {
		t.Fatalf("after the peer's Final = %+v, want neither bit and 150 ms", p)
	}
	s.Receive(remote(StateDown, 11), now)
	if p := s.Packet(); p.DesiredMinTxUS != 1000000 || p.RequiredMinRxUS != 1000000 || p.Diag != DiagNeighborSaysDown {
		t.Fatalf("after Down = %+v, want slow start again with diag 3", p)
	}
}

// RFC 5880 6.8.7: no periodic packets when the peer asks for none.
func TestSessionNoPacketsWhenRemoteRxIsZero(t *testing.T) {
	s := NewSession(fabric, 11)
	if s.TxInterval() != time.Second {
		t.Fatalf("initial interval = %s, want 1 s", s.TxInterval())
	}
	p := remote(StateDown, 0)
	p.RequiredMinRxUS = 0
	s.Receive(p, time.Now())
	if s.TxInterval() != 0 {
		t.Fatalf("interval = %s, want 0", s.TxInterval())
	}
}

func TestPacketRoundTripAndChecks(t *testing.T) {
	in := Packet{Diag: DiagTimeExpired, State: StateUp, Poll: true, DetectMult: 3, MyDiscriminator: 1,
		YourDiscriminator: 2, DesiredMinTxUS: 150000, RequiredMinRxUS: 150000}
	out, err := Parse(in.Marshal())
	if err != nil || out != in {
		t.Fatalf("round trip = %+v, %v, want %+v", out, err, in)
	}
	bad := func(edit func(b []byte)) []byte {
		b := in.Marshal()
		edit(b)
		return b
	}
	for name, tc := range map[string]struct {
		b    []byte
		want error
	}{
		"short":       {in.Marshal()[:23], ErrShort},
		"version":     {bad(func(b []byte) { b[0] = 2 << 5 }), ErrVersion},
		"length":      {bad(func(b []byte) { b[3] = 30 }), ErrLength},
		"multiplier":  {bad(func(b []byte) { b[2] = 0 }), ErrDetectMult},
		"multipoint":  {bad(func(b []byte) { b[1] |= 1 }), ErrMultipoint},
		"my discr":    {bad(func(b []byte) { copy(b[4:8], []byte{0, 0, 0, 0}) }), ErrMyDiscr},
		"your discr":  {bad(func(b []byte) { copy(b[8:12], []byte{0, 0, 0, 0}) }), ErrYourDiscr},
		"auth":        {append(bad(func(b []byte) { b[1] |= 0x04; b[3] = 26 }), 0, 0), ErrAuthNotWanted},
		"auth length": {bad(func(b []byte) { b[1] |= 0x04 }), ErrLength},
	} {
		if _, err := Parse(tc.b); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestJitter(t *testing.T) {
	for range 1000 {
		if j := jitter(time.Second, 3); j < 750*time.Millisecond || j > time.Second {
			t.Fatalf("jitter = %s, want 75-100%%", j)
		}
		if j := jitter(time.Second, 1); j < 750*time.Millisecond || j > 900*time.Millisecond {
			t.Fatalf("jitter with multiplier 1 = %s, want 75-90%%", j)
		}
	}
}

// RFC 5880 6.8.1 and 6.8.18: a detection time without packets forgets the
// remote session in every state, also in Down, so the next packet goes out
// with Your Discriminator 0 and the slow interval. Before, a session that
// went Down by a remote Down kept the old discriminator of a peer that
// then restarted.
func TestSessionForgetsRemoteAfterDetectionTime(t *testing.T) {
	now := time.Unix(100, 0)
	s := NewSession(fabric, 11)
	s.Receive(remote(StateDown, 0), now) // Init
	bye := remote(StateAdminDown, 11)
	bye.RequiredMinRxUS = 2000000
	s.Receive(bye, now) // Down, diag 3; the peer then goes silent
	if s.State() != StateDown || s.RemoteDiscriminator() != 77 || s.TxInterval() != 2*time.Second {
		t.Fatalf("setup: %s discr %d interval %s", s.State(), s.RemoteDiscriminator(), s.TxInterval())
	}
	if s.CheckTimeout(now.Add(s.DetectionTime())) || s.RemoteDiscriminator() != 77 {
		t.Fatal("remote session forgotten before the detection time")
	}
	if s.CheckTimeout(now.Add(s.DetectionTime() + time.Millisecond)) {
		t.Error("a Down session reported a state change")
	}
	if s.RemoteDiscriminator() != 0 {
		t.Errorf("remote discriminator %d after the detection time, want 0", s.RemoteDiscriminator())
	}
	if s.TxInterval() != time.Second {
		t.Errorf("interval %s after the detection time, want 1 s (remote Required Min RX reset)", s.TxInterval())
	}
	if p := s.Packet(); p.YourDiscriminator != 0 {
		t.Errorf("Your Discriminator %d, want 0", p.YourDiscriminator)
	}
}

// RFC 5880 6.8.7: no periodic packets while Demand mode is active on the
// peer (D bit, both Up), except during a Poll Sequence.
func TestSessionNoPeriodicPacketsInRemoteDemandMode(t *testing.T) {
	now := time.Now()
	s := NewSession(fabric, 11)
	s.Receive(remote(StateInit, 11), now) // Up, our Poll Sequence runs
	demand := remote(StateUp, 11)
	demand.Demand = true
	s.Receive(demand, now)
	if s.TxInterval() == 0 {
		t.Fatal("periodic packets stopped during our Poll Sequence")
	}
	demand.Final = true
	s.Receive(demand, now)
	if s.TxInterval() != 0 {
		t.Fatalf("interval %s in remote Demand mode, want 0", s.TxInterval())
	}
	s.Receive(remote(StateUp, 11), now)
	if s.TxInterval() == 0 {
		t.Fatal("periodic packets did not resume after the peer left Demand mode")
	}

	// Both sides must be Up: the D bit alone does not stop the packets.
	t.Run("we Up, peer Init with D", func(t *testing.T) {
		s := NewSession(fabric, 11)
		s.Receive(remote(StateInit, 11), now) // we are Up
		p := remote(StateInit, 11)
		p.Demand, p.Final = true, true // Final ends our Poll Sequence
		s.Receive(p, now)
		if s.State() != StateUp || s.TxInterval() == 0 {
			t.Fatalf("state %s interval %s, want Up with periodic packets", s.State(), s.TxInterval())
		}
	})
	t.Run("we Down, peer Up with D", func(t *testing.T) {
		s := NewSession(fabric, 11)
		p := remote(StateUp, 11)
		p.Demand = true
		s.Receive(p, now) // Down + remote Up stays Down
		if s.State() != StateDown || s.TxInterval() == 0 {
			t.Fatalf("state %s interval %s, want Down with periodic packets", s.State(), s.TxInterval())
		}
	})
}
