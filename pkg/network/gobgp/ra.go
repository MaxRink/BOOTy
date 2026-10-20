//go:build linux

package gobgp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"

	"github.com/telekom/BOOTy/pkg/network"
)

// Router Advertisement timing of FRR 10.2 zebra (zebra/rtadv.c,
// zebra/rtadv.h) and RFC 4861 section 10.
const (
	// raFastRexmits is RTADV_NUM_FAST_REXMITS: zebra sets inFastRexmit
	// with it in ipv6_nd_suppress_ra_set and rtadv_if_up, and
	// rtadv_timer sends them 1 s apart (RTADV_FAST_REXMIT_PERIOD).
	raFastRexmits = 4

	// raFastCount is the number of RAs sent 1 s apart when the RAs
	// start: the fast retransmits and then, one tick later, the first
	// regular RA, because AdvIntervalTimer starts at 0.
	raFastCount = raFastRexmits + 1

	// raFastInterval is RTADV_FAST_REXMIT_PERIOD.
	raFastInterval = time.Second

	// minDelayBetweenRAs is MIN_DELAY_BETWEEN_RAS (RFC 4861 section 10;
	// zebra/rtadv.h).
	minDelayBetweenRAs = 3 * time.Second

	// maxRADelay is MAX_RA_DELAY_TIME (RFC 4861 section 10).
	maxRADelay = 500 * time.Millisecond
)

// raDelay is the random delay of an answer to a Router Solicitation;
// tests replace it.
var raDelay = func() time.Duration {
	return rand.N(maxRADelay) //nolint:gosec // timing jitter
}

// raSchedule is the RA timing of one NIC.
type raSchedule struct {
	fast int       // fast RAs left
	last time.Time // last RA sent
	next time.Time // next RA due
}

// start starts the fast RAs at once, when the RAs start.
func (s *raSchedule) start(now time.Time) {
	s.fast = raFastCount
	s.next = now
}

// carrierUp starts the fast retransmits again on the next 1 s tick, like
// zebra rtadv_if_up, which only sets them for rtadv_timer.
func (s *raSchedule) carrierUp(now time.Time) {
	s.fast = raFastRexmits
	s.next = now.Add(raFastInterval)
}

// retry schedules the next try 1 s after an RA that did not go out: no
// carrier, a tentative link-local address or a failed write. Only an RA
// that went out counts as a fast RA, so all of them reach the switch
// after DAD, as in zebra, which skips the interface until then.
func (s *raSchedule) retry(now time.Time) {
	s.next = now.Add(raFastInterval)
}

// sent records an RA sent at now and schedules the next one: 1 s later
// while fast RAs are left, else after raInterval (zebra resets
// AdvIntervalTimer to MaxRtrAdvInterval after each regular RA and after
// the answer to a solicitation).
func (s *raSchedule) sent(now time.Time) {
	s.last = now
	if s.fast > 0 {
		s.fast--
	}
	if s.fast > 0 {
		s.next = now.Add(raFastInterval)
		return
	}
	s.next = now.Add(raInterval)
}

// solicited schedules the answer to a Router Solicitation received at now
// (RFC 4861 section 6.2.6): after the random delay, and not before
// MIN_DELAY_BETWEEN_RAS plus the delay after the last RA. An RA that is
// due earlier anyway answers the solicitation.
func (s *raSchedule) solicited(now time.Time, delay time.Duration) {
	at := now.Add(delay)
	if earliest := s.last.Add(minDelayBetweenRAs + delay); at.Before(earliest) {
		at = earliest
	}
	if at.Before(s.next) {
		s.next = at
	}
}

// startPeriodicRA starts the Router Advertisements on the NICs, like
// zebra for a bgpd unnumbered peer: each NIC joins the all-routers group
// and answers Router Solicitations, sends raFastCount RAs 1 s apart, then
// one every raInterval, and starts the fast RAs again when it gets
// carrier back. The goroutines get the stop channel as an argument,
// because Teardown clears the field after closing it.
func (u *UnderlayTier) startPeriodicRA() {
	stop, done := make(chan struct{}), make(chan struct{})
	u.stopRA, u.raDone = stop, done
	var wg sync.WaitGroup
	u.raUp = make(map[string]chan struct{}, len(u.nics))
	for _, nic := range u.nics {
		rs, up := make(chan struct{}, 1), make(chan struct{}, 1)
		u.raUp[nic] = up
		c, err := listenRS(nic, rs)
		if err != nil {
			u.log.Warn("router solicitations are not answered", "nic", nic, "error", err)
		}
		wg.Go(func() {
			advertise(nic, stop, rs, up)
			if c != nil {
				_ = c.Close() // leaves the all-routers group after the final RA
			}
		})
	}
	if err := watchCarrierUp(stop, u.raUp, &wg); err != nil {
		u.log.Warn("RAs do not start fast again after a carrier loss", "error", err)
	}
	go func() {
		wg.Wait()
		close(done)
	}()
}

// stopPeriodicRA stops the RAs and waits, so the final RA with lifetime 0
// is out before the handoff (kexec) can follow.
func (u *UnderlayTier) stopPeriodicRA() {
	if u.stopRA == nil {
		return
	}
	close(u.stopRA)
	<-u.raDone
	u.stopRA = nil
}

// advertise sends the RAs of one NIC until stop is closed, then a final
// RA with router lifetime 0 (RFC 4861 section 6.2.5). A value on up
// restarts the fast RAs, a value on rs is a Router Solicitation.
func advertise(nic string, stop, rs, up <-chan struct{}) {
	var s raSchedule
	send := func(now time.Time) {
		if raSend(nic, raLifetime) {
			s.sent(now)
		} else {
			s.retry(now)
		}
	}
	s.start(time.Now())
	send(time.Now())
	t := time.NewTimer(time.Until(s.next))
	defer t.Stop()
	for {
		select {
		case <-stop:
			raSend(nic, 0)
			return
		case <-up:
			s.carrierUp(time.Now())
		case <-rs:
			s.solicited(time.Now(), raDelay())
		case now := <-t.C:
			send(now)
		}
		t.Reset(time.Until(s.next))
	}
}

// listenRS joins the all-routers group ff02::2 on nic and reports each
// valid Router Solicitation on rs. RFC 4861 section 6.2.2: an interface
// that sends RAs MUST join it; zebra does in if_join_all_router. Closing
// the returned socket leaves the group and ends the reader.
func listenRS(nic string, rs chan<- struct{}) (io.Closer, error) {
	ifi, err := net.InterfaceByName(nic)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", nic, err)
	}
	conn, err := network.ListenICMPv6(context.Background(), nic)
	if err != nil {
		return nil, err //nolint:wrapcheck // names the NIC
	}
	pc := ipv6.NewPacketConn(conn)
	var f ipv6.ICMPFilter
	f.SetAll(true)
	f.Accept(ipv6.ICMPTypeRouterSolicitation)
	if err := errors.Join(pc.SetICMPFilter(&f), pc.SetControlMessage(ipv6.FlagHopLimit, true),
		pc.JoinGroup(ifi, &net.IPAddr{IP: net.IPv6linklocalallrouters})); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("router solicitations on %s: %w", nic, err)
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, cm, src, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if validRS(buf[:n], cm, src) {
				select {
				case rs <- struct{}{}:
				default: // an answer is already scheduled
				}
			}
		}
	}()
	return conn, nil
}

// validRS checks a Router Solicitation like RFC 4861 section 6.1.1 (the
// kernel checks the checksum): hop limit 255, code 0, at least 8 octets,
// options with a non-zero length, and no source link-layer address option
// from the unspecified address.
func validRS(b []byte, cm *ipv6.ControlMessage, src net.Addr) bool {
	if len(b) < 8 || b[0] != byte(ipv6.ICMPTypeRouterSolicitation) || b[1] != 0 || cm == nil || cm.HopLimit != mandatedHopLimit {
		return false
	}
	a, ok := src.(*net.IPAddr)
	unspecified := !ok || a.IP.IsUnspecified()
	for o := b[8:]; len(o) > 0; {
		if len(o) < 2 || o[1] == 0 || int(o[1])*8 > len(o) {
			return false
		}
		if o[0] == 1 && unspecified {
			return false
		}
		o = o[int(o[1])*8:]
	}
	return true
}

// watchCarrierUp signals up[nic] each time a NIC gets carrier back; zebra
// calls rtadv_if_up from if_up (zebra/interface.c). The watch ends when
// stop is closed.
func watchCarrierUp(stop chan struct{}, up map[string]chan struct{}, wg *sync.WaitGroup) error {
	carrier := make(map[string]bool, len(up))
	for nic := range up {
		carrier[nic] = hasCarrier(nic)
	}
	updates := make(chan netlink.LinkUpdate, 16)
	if err := netlink.LinkSubscribe(updates, stop); err != nil {
		return fmt.Errorf("subscribe to link updates: %w", err)
	}
	wg.Go(func() { relayCarrierUp(updates, carrier, up) })
	return nil
}

// relayCarrierUp signals up[nic] when an update shows carrier
// (IFF_LOWER_UP) on a NIC that had none.
func relayCarrierUp(updates <-chan netlink.LinkUpdate, carrier map[string]bool, up map[string]chan struct{}) {
	for u := range updates {
		if u.Link == nil {
			continue
		}
		nic := u.Attrs().Name
		ch, ok := up[nic]
		if !ok {
			continue
		}
		has := u.Flags&unix.IFF_LOWER_UP != 0
		if has && !carrier[nic] {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		carrier[nic] = has
	}
}
