package bfd

import (
	"math/rand/v2"
	"time"
)

// Timers are the configured BFD parameters of a session, as in an FRR
// "bfd profile".
type Timers struct {
	DesiredMinTx  time.Duration
	RequiredMinRx time.Duration
	DetectMult    uint8
}

// slowTx is the transmit interval while the session is not Up (RFC 5880
// 6.8.3: at least one second; FRR bfdd uses one second).
const slowTx = time.Second

// SlowTx is slowTx for the callers: the transmit interval while a session
// is not Up.
const SlowTx = slowTx

// Session is the state machine of one BFD session (RFC 5880 6.8). It does
// no I/O and takes the time as an argument, so it is tested without a
// network. The caller serializes the calls.
type Session struct {
	timers     Timers
	state      State
	diag       Diag
	localDiscr uint32

	remoteDiscr      uint32
	remoteState      State
	remoteMinRx      time.Duration
	remoteDesiredTx  time.Duration
	remoteDetectMult uint8
	// remoteDemand is the Demand (D) bit of the last received packet
	// (bfd.RemoteDemandMode).
	remoteDemand bool

	// desiredTx is the advertised DesiredMinTxInterval: slowTx until Up.
	desiredTx time.Duration
	// curRx is the Required Min RX Interval in use: slowTx until the Poll
	// Sequence that starts at Up completes, like FRR bfdd (cur_timers).
	// The Poll advertises the configured value; the detection time uses
	// curRx, so the peer has the Poll Sequence to speed up.
	curRx time.Duration
	// polling is a Poll Sequence in progress (RFC 5880 6.5); sendFinal is a
	// received Poll that still needs its Final.
	polling   bool
	sendFinal bool

	lastRx time.Time
}

// NewSession returns a session in state Down with the local discriminator
// discr (nonzero, unique per system).
func NewSession(t Timers, discr uint32) *Session {
	// bfd.RemoteMinRxInterval starts at 1 microsecond (RFC 5880 6.8.1).
	return &Session{timers: t, state: StateDown, localDiscr: discr, desiredTx: max(t.DesiredMinTx, slowTx),
		curRx: max(t.RequiredMinRx, slowTx), remoteMinRx: time.Microsecond}
}

// State returns the session state.
func (s *Session) State() State { return s.state }

// Diag returns the local diagnostic.
func (s *Session) Diag() Diag { return s.diag }

// LocalDiscriminator returns the local discriminator.
func (s *Session) LocalDiscriminator() uint32 { return s.localDiscr }

// RemoteDiscriminator returns the remote discriminator, 0 when unknown.
func (s *Session) RemoteDiscriminator() uint32 { return s.remoteDiscr }

// Receive processes a parsed control packet for this session (RFC 5880
// 6.8.6) and reports whether the session state changed.
func (s *Session) Receive(p *Packet, now time.Time) bool {
	if p.YourDiscriminator != 0 && p.YourDiscriminator != s.localDiscr {
		return false
	}
	s.remoteDiscr = p.MyDiscriminator
	s.remoteState = p.State
	s.remoteMinRx = us(p.RequiredMinRxUS)
	s.remoteDesiredTx = us(p.DesiredMinTxUS)
	s.remoteDetectMult = p.DetectMult
	s.remoteDemand = p.Demand
	if p.Final && s.polling {
		s.polling = false
		s.curRx = s.timers.RequiredMinRx
	}
	s.lastRx = now
	if p.Poll {
		s.sendFinal = true
	}
	if s.state == StateAdminDown {
		return false
	}
	old := s.state
	s.transition(p.State)
	return s.state != old
}

// transition applies the remote state (RFC 5880 6.8.6, section 6.2).
func (s *Session) transition(remote State) {
	switch {
	case remote == StateAdminDown:
		if s.state != StateDown {
			s.down(DiagNeighborSaysDown)
		}
	case s.state == StateDown && remote == StateDown:
		s.state = StateInit
	case s.state == StateDown && remote == StateInit,
		s.state == StateInit && (remote == StateInit || remote == StateUp):
		s.up()
	case s.state == StateUp && remote == StateDown:
		s.down(DiagNeighborSaysDown)
	}
}

// up enters state Up and switches from the slow start interval to the
// configured one with a Poll Sequence, like FRR bfdd.
func (s *Session) up() {
	s.state = StateUp
	s.diag = DiagNone
	if s.desiredTx != s.timers.DesiredMinTx || s.curRx != s.timers.RequiredMinRx {
		// A lower DesiredMinTx applies at once (RFC 5880 6.8.3); the lower
		// Required Min RX after the Poll Sequence.
		s.desiredTx = s.timers.DesiredMinTx
		s.polling = true
	}
}

func (s *Session) down(d Diag) {
	s.state = StateDown
	s.diag = d
	s.desiredTx = max(s.timers.DesiredMinTx, slowTx)
	s.curRx = max(s.timers.RequiredMinRx, slowTx)
	s.polling = false
}

// DetectionTime is the time after the last received packet after which
// the session goes down (RFC 5880 6.8.4, asynchronous mode).
func (s *Session) DetectionTime() time.Duration {
	return time.Duration(s.remoteDetectMult) * max(s.curRx, s.remoteDesiredTx)
}

// CheckTimeout moves an Init or Up session to Down when no packet arrived
// within the detection time, and reports a state change. In every state,
// a detection time without packets also forgets the remote session: the
// remote discriminator (RFC 5880 6.8.1, bfd.RemoteDiscr: MUST be zero) and
// the remote Required Min RX (6.8.18: SHOULD be reset to 1 microsecond).
func (s *Session) CheckTimeout(now time.Time) bool {
	if s.lastRx.IsZero() || now.Sub(s.lastRx) <= s.DetectionTime() {
		return false
	}
	s.remoteDiscr = 0
	s.remoteMinRx = time.Microsecond
	s.remoteDemand = false
	if s.state != StateInit && s.state != StateUp {
		return false
	}
	s.down(DiagTimeExpired)
	return true
}

// TxInterval is the interval between periodic packets before jitter
// (RFC 5880 6.8.7). It is 0 when the peer asked for no packets: its
// Required Min RX is zero, or Demand mode is active on the peer (both
// sides Up) and no Poll Sequence is running.
func (s *Session) TxInterval() time.Duration {
	if s.remoteMinRx == 0 {
		return 0
	}
	if s.remoteDemand && s.state == StateUp && s.remoteState == StateUp && !s.polling {
		return 0
	}
	return max(s.desiredTx, s.remoteMinRx)
}

// NeedFinal reports a received Poll that waits for its Final.
func (s *Session) NeedFinal() bool { return s.sendFinal }

// Packet builds the next control packet. A pending Final is sent once and
// without Poll (RFC 5880 6.5).
func (s *Session) Packet() Packet {
	p := Packet{
		Diag:              s.diag,
		State:             s.state,
		DetectMult:        s.timers.DetectMult,
		MyDiscriminator:   s.localDiscr,
		YourDiscriminator: s.remoteDiscr,
		DesiredMinTxUS:    toUS(s.desiredTx),
		RequiredMinRxUS:   toUS(s.curRx),
	}
	if s.polling {
		// A Poll and the Final to a Poll advertise the new values.
		p.RequiredMinRxUS = toUS(s.timers.RequiredMinRx)
	}
	if s.sendFinal {
		p.Final = true
		s.sendFinal = false
	} else if s.polling {
		p.Poll = true
	}
	return p
}

func us(v uint32) time.Duration { return time.Duration(v) * time.Microsecond }

func toUS(d time.Duration) uint32 { return uint32(d / time.Microsecond) } //nolint:gosec // intervals are at most 60 s

// jitter reduces the interval by 0 to 25 percent, or by 10 to 25 percent
// with a detect multiplier of 1 (RFC 5880 6.8.7).
func jitter(iv time.Duration, mult uint8) time.Duration {
	lo, span := 0.75, 0.25
	if mult == 1 {
		span = 0.15
	}
	return time.Duration(float64(iv) * (lo + rand.Float64()*span)) //nolint:gosec // jitter needs no crypto random
}
