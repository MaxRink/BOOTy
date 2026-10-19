package dhcp6

import (
	"encoding/binary"
	"net"
	"slices"
	"testing"
	"time"
)

func iaAddr(ip string, pref, valid uint32, status ...uint16) []byte {
	b := append([]byte(nil), net.ParseIP(ip).To16()...)
	b = binary.BigEndian.AppendUint32(b, pref)
	b = binary.BigEndian.AppendUint32(b, valid)
	for _, st := range status {
		b = binary.BigEndian.AppendUint16(b, optStatusCode)
		b = binary.BigEndian.AppendUint16(b, 2)
		b = binary.BigEndian.AppendUint16(b, st)
	}
	return b
}

func iaNA(t1, t2 uint32, addrs ...[]byte) option {
	b := binary.BigEndian.AppendUint32(nil, 7)
	b = binary.BigEndian.AppendUint32(b, t1)
	b = binary.BigEndian.AppendUint32(b, t2)
	for _, a := range addrs {
		b = binary.BigEndian.AppendUint16(b, optIAAddr)
		b = binary.BigEndian.AppendUint16(b, uint16(len(a))) //nolint:gosec // short
		b = append(b, a...)
	}
	return option{optIANA, b}
}

// RFC 8415 21.4 and 21.6, T1 and T2 defaults and clamps.
func TestIAChecks(t *testing.T) {
	now := time.Unix(0, 0)
	for _, tt := range []struct {
		name           string
		ia             option
		ok             bool
		addr           string
		t1, t2, expire time.Duration
	}{
		{"defaults", iaNA(0, 0, iaAddr("fd00::1", 120, 180)), true, "fd00::1", 60 * time.Second, 96 * time.Second, 180 * time.Second},
		{"as given", iaNA(30, 50, iaAddr("fd00::1", 120, 180)), true, "fd00::1", 30 * time.Second, 50 * time.Second, 180 * time.Second},
		{"T2 over valid", iaNA(30, 500, iaAddr("fd00::1", 120, 180)), true, "fd00::1", 30 * time.Second, 180 * time.Second, 180 * time.Second},
		{"T1 > T2", iaNA(60, 50, iaAddr("fd00::1", 120, 180)), false, "", 0, 0, 0},
		{"T1 only, over T2 default", iaNA(200, 0, iaAddr("fd00::1", 120, 180)), true, "fd00::1", 96 * time.Second, 96 * time.Second, 180 * time.Second},
		{"preferred over valid", iaNA(0, 0, iaAddr("fd00::1", 200, 100)), false, "", 0, 0, 0},
		{"valid 0", iaNA(0, 0, iaAddr("fd00::1", 0, 0)), false, "", 0, 0, 0},
		{"status skips one", iaNA(0, 0, iaAddr("fd00::1", 120, 180, 2), iaAddr("fd00::2", 120, 180)), true, "fd00::2", 60 * time.Second, 96 * time.Second, 180 * time.Second},
	} {
		var l Lease
		ok := l.ia(tt.ia.data, now)
		if ok != tt.ok || (ok && (!l.Addr.Equal(net.ParseIP(tt.addr)) || l.Renew.Sub(now) != tt.t1 || l.Rebind.Sub(now) != tt.t2 || l.Expire.Sub(now) != tt.expire)) {
			t.Errorf("%s: ok %v addr %v T1 %s T2 %s expire %s", tt.name, ok, l.Addr, l.Renew.Sub(now), l.Rebind.Sub(now), l.Expire.Sub(now))
		}
	}
}

// RFC 8415 16.3 and 16.10: an ADVERTISE or REPLY without a server
// identifier is discarded; the top-level status is decoded.
func TestDecodeChecks(t *testing.T) {
	client := option{optClientID, testDUID}
	if _, err := decode(encode(msgReply, 1, []option{client, iaNA(0, 0, iaAddr("fd00::1", 120, 180))}), 7, time.Now()); err == nil {
		t.Error("a REPLY without server ID was accepted")
	}
	if _, err := decode(encode(msgAdvertise, 1, []option{client}), 7, time.Now()); err == nil {
		t.Error("an ADVERTISE without server ID was accepted")
	}
	r, err := decode(encode(msgAdvertise, 1, []option{{optServerID, testServer}, client, {optStatusCode, []byte{0, 2}}}), 7, time.Now())
	if err != nil || r.status != 2 {
		t.Errorf("status = %d, %v", r.status, err)
	}
}

// A REQUEST is sent ten times without a REPLY, then SOLICIT starts over;
// an ADVERTISE with a status other than success is ignored.
func TestRequestRetriesAndAdvertiseStatus(t *testing.T) {
	f := newFakeNet()
	advertised := 0
	f.answer = func(msg []byte) []byte {
		if msg[0] != msgSolicit {
			return nil
		}
		if advertised++; advertised == 1 {
			r := serverReply(msg, msgAdvertise, false)
			return append(r, 0, byte(optStatusCode), 0, 2, 0, 2) // NoAddrsAvail
		}
		if advertised == 2 {
			return serverReply(msg, msgAdvertise, false)
		}
		return nil
	}
	c := f.client(true)
	c.Start()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		msgs := f.messages()
		if n := slices.IndexFunc(msgs, func(m []byte) bool { return m[0] == msgRequest }); n >= 0 &&
			slices.ContainsFunc(msgs[n:], func(m []byte) bool { return m[0] == msgSolicit }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no SOLICIT after the REQUESTs")
		}
	}
	close(f.stopped)
	c.Stop()
	msgs := f.messages()
	first := slices.IndexFunc(msgs, func(m []byte) bool { return m[0] == msgRequest })
	reqs := 0
	for _, m := range msgs[first:] {
		if m[0] == msgSolicit {
			break
		}
		reqs++
	}
	if reqs != 10 {
		t.Errorf("%d REQUESTs, want 10", reqs)
	}
	// The SOLICIT before the first REQUEST: the one that got the second
	// ADVERTISE, so the first ADVERTISE (status 2) was ignored.
	if sol := slices.IndexFunc(msgs, func(m []byte) bool { return m[0] == msgSolicit }); first-sol != 2 {
		t.Errorf("REQUEST after %d SOLICITs, want after the second", first-sol)
	}
}

// RENEW accepts only a REPLY that binds the address.
func TestRenewNeedsAddress(t *testing.T) {
	f := newFakeNet()
	f.answer = func(msg []byte) []byte {
		switch msg[0] {
		case msgSolicit:
			return serverReply(msg, msgReply, true)
		case msgRenew:
			clientID, _ := find(parseOptions(msg[4:]), optClientID)
			return encode(msgReply, xidOf(msg), []option{{optServerID, testServer}, {optClientID, clientID}})
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client(true)
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	f.tick(t)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		renews := 0
		for _, m := range f.messages() {
			if m[0] == msgRenew {
				renews++
			}
		}
		if renews >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("RENEW stopped after a REPLY without an address")
		}
	}
	close(f.stopped)
	c.Stop()
	if len(bound) != 0 {
		t.Fatal("a REPLY without an address bound")
	}
}

// Option 32 sets the wait before the next INFORMATION-REQUEST, at least
// 600 s; without it the wait is a day.
func TestInfoRefresh(t *testing.T) {
	client := option{optClientID, testDUID}
	r, err := decode(encode(msgReply, 1, []option{{optServerID, testServer}, client, {optInfoRefresh, []byte{0, 0, 0x0e, 0x10}}}), 7, time.Now())
	if err != nil || r.lease.InfoRefresh != time.Hour {
		t.Fatalf("InfoRefresh = %s, %v", r.lease.InfoRefresh, err)
	}
	r, err = decode(encode(msgReply, 1, []option{{optServerID, testServer}, client, {optInfoRefresh, []byte{0, 0, 0, 0}}}), 7, time.Now())
	if err != nil || r.lease.InfoRefresh != 600*time.Second {
		t.Fatalf("InfoRefresh of 0 = %s, %v; want 600s (IRT_MINIMUM)", r.lease.InfoRefresh, err)
	}
	for in, want := range map[time.Duration]time.Duration{0: 24 * time.Hour, time.Minute: 600 * time.Second, time.Hour: time.Hour} {
		if got := infoRefresh(in); got != want {
			t.Errorf("infoRefresh(%s) = %s, want %s", in, got, want)
		}
	}
}
