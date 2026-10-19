package dhcp6

import (
	"encoding/binary"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
)

func iaAddr(ip string, pref, valid uint32, status ...uint16) []byte {
	b := append([]byte(nil), net.ParseIP(ip).To16()...)
	b = binary.BigEndian.AppendUint32(b, pref)
	b = binary.BigEndian.AppendUint32(b, valid)
	for _, st := range status {
		b = binary.BigEndian.AppendUint16(b, uint16(optStatusCode))
		b = binary.BigEndian.AppendUint16(b, 2)
		b = binary.BigEndian.AppendUint16(b, st)
	}
	return b
}

func iaNAOpt(t1, t2 uint32, addrs ...[]byte) dhcpv6.Option {
	b := binary.BigEndian.AppendUint32(nil, 7)
	b = binary.BigEndian.AppendUint32(b, t1)
	b = binary.BigEndian.AppendUint32(b, t2)
	for _, a := range addrs {
		b = binary.BigEndian.AppendUint16(b, uint16(optIAAddr))
		b = binary.BigEndian.AppendUint16(b, uint16(len(a))) //nolint:gosec // short
		b = append(b, a...)
	}
	return raw(optIANA, b)
}

// RFC 8415 21.4 and 21.6, T1 and T2 defaults and clamps.
func TestIAChecks(t *testing.T) {
	now := time.Unix(0, 0)
	for _, tt := range []struct {
		name           string
		ia             dhcpv6.Option
		ok             bool
		addr           string
		t1, t2, expire time.Duration
	}{
		{"defaults", iaNAOpt(0, 0, iaAddr("fd00::1", 120, 180)), true, "fd00::1", 60 * time.Second, 96 * time.Second, 180 * time.Second},
		{"as given", iaNAOpt(30, 50, iaAddr("fd00::1", 120, 180)), true, "fd00::1", 30 * time.Second, 50 * time.Second, 180 * time.Second},
		{"T2 over valid", iaNAOpt(30, 500, iaAddr("fd00::1", 120, 180)), true, "fd00::1", 30 * time.Second, 180 * time.Second, 180 * time.Second},
		{"T1 > T2", iaNAOpt(60, 50, iaAddr("fd00::1", 120, 180)), false, "", 0, 0, 0},
		{"T1 only, over T2 default", iaNAOpt(200, 0, iaAddr("fd00::1", 120, 180)), true, "fd00::1", 96 * time.Second, 96 * time.Second, 180 * time.Second},
		{"preferred over valid", iaNAOpt(0, 0, iaAddr("fd00::1", 200, 100)), false, "", 0, 0, 0},
		{"valid 0", iaNAOpt(0, 0, iaAddr("fd00::1", 0, 0)), false, "", 0, 0, 0},
		{"status skips one", iaNAOpt(0, 0, iaAddr("fd00::1", 120, 180, 2), iaAddr("fd00::2", 120, 180)), true, "fd00::2", 60 * time.Second, 96 * time.Second, 180 * time.Second},
	} {
		var l Lease
		var ia dhcpv6.OptIANA
		if err := ia.FromBytes(tt.ia.ToBytes()); err != nil {
			t.Fatal(err)
		}
		ok := l.ia(&ia, now)
		if ok != tt.ok || (ok && (!l.Addr.Equal(net.ParseIP(tt.addr)) || l.Renew.Sub(now) != tt.t1 || l.Rebind.Sub(now) != tt.t2 || l.Expire.Sub(now) != tt.expire)) {
			t.Errorf("%s: ok %v addr %v T1 %s T2 %s expire %s", tt.name, ok, l.Addr, l.Renew.Sub(now), l.Rebind.Sub(now), l.Expire.Sub(now))
		}
	}
}

// RFC 8415 16.3 and 16.10: an ADVERTISE or REPLY without a server
// identifier is discarded; the top-level status is decoded.
func TestDecodeChecks(t *testing.T) {
	client := raw(optClientID, testDUID)
	if _, err := decode(encode(msgReply, 1, []dhcpv6.Option{client, iaNAOpt(0, 0, iaAddr("fd00::1", 120, 180))}), 7, time.Now()); err == nil {
		t.Error("a REPLY without server ID was accepted")
	}
	if _, err := decode(encode(msgAdvertise, 1, []dhcpv6.Option{client}), 7, time.Now()); err == nil {
		t.Error("an ADVERTISE without server ID was accepted")
	}
	if _, err := decode(encode(msgAdvertise, 1, []dhcpv6.Option{raw(optServerID, testServer), client, raw(optStatusCode, []byte{0, 2})}), 7, time.Now()); err == nil {
		t.Error("an ADVERTISE with status NoAddrsAvail was accepted")
	}
}

// A REQUEST is sent ten times without a REPLY, then SOLICIT starts over;
// an ADVERTISE with a status other than success is ignored.
func TestRequestRetriesAndAdvertiseStatus(t *testing.T) {
	f := newFakeNet()
	advertised := 0
	f.answer = func(msg []byte) []byte {
		if dhcpv6.MessageType(msg[0]) != msgSolicit {
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
		if n := slices.IndexFunc(msgs, func(m []byte) bool { return dhcpv6.MessageType(m[0]) == msgRequest }); n >= 0 &&
			slices.ContainsFunc(msgs[n:], func(m []byte) bool { return dhcpv6.MessageType(m[0]) == msgSolicit }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no SOLICIT after the REQUESTs")
		}
	}
	close(f.stopped)
	c.Stop()
	msgs := f.messages()
	first := slices.IndexFunc(msgs, func(m []byte) bool { return dhcpv6.MessageType(m[0]) == msgRequest })
	reqs := 0
	for _, m := range msgs[first:] {
		if dhcpv6.MessageType(m[0]) == msgSolicit {
			break
		}
		reqs++
	}
	if reqs != 10 {
		t.Errorf("%d REQUESTs, want 10", reqs)
	}
	// The SOLICIT before the first REQUEST: the one that got the second
	// ADVERTISE, so the first ADVERTISE (status 2) was ignored.
	if sol := slices.IndexFunc(msgs, func(m []byte) bool { return dhcpv6.MessageType(m[0]) == msgSolicit }); first-sol != 2 {
		t.Errorf("REQUEST after %d SOLICITs, want after the second", first-sol)
	}
}

// RENEW accepts only a REPLY that binds the address.
func TestRenewNeedsAddress(t *testing.T) {
	f := newFakeNet()
	f.answer = func(msg []byte) []byte {
		switch dhcpv6.MessageType(msg[0]) {
		case msgSolicit:
			return serverReply(msg, msgReply, true)
		case msgRenew:
			clientID, _ := find(msg, optClientID)
			return encode(msgReply, xidOf(msg), []dhcpv6.Option{raw(optServerID, testServer), raw(optClientID, clientID)})
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
			if dhcpv6.MessageType(m[0]) == msgRenew {
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
	client := raw(optClientID, testDUID)
	r, err := decode(encode(msgReply, 1, []dhcpv6.Option{raw(optServerID, testServer), client, raw(optInfoRefresh, []byte{0, 0, 0x0e, 0x10})}), 7, time.Now())
	if err != nil || r.lease.InfoRefresh != time.Hour {
		t.Fatalf("InfoRefresh = %s, %v", r.lease.InfoRefresh, err)
	}
	r, err = decode(encode(msgReply, 1, []dhcpv6.Option{raw(optServerID, testServer), client, raw(optInfoRefresh, []byte{0, 0, 0, 0})}), 7, time.Now())
	if err != nil || r.lease.InfoRefresh != 600*time.Second {
		t.Fatalf("InfoRefresh of 0 = %s, %v; want 600s (IRT_MINIMUM)", r.lease.InfoRefresh, err)
	}
	for in, want := range map[time.Duration]time.Duration{0: 24 * time.Hour, time.Minute: 600 * time.Second, time.Hour: time.Hour} {
		if got := infoRefresh(in); got != want {
			t.Errorf("infoRefresh(%s) = %s, want %s", in, got, want)
		}
	}
}

// Like networkd: a malformed option is ignored, not the reply; in an IA a
// malformed sub-option of the address is ignored, a truncated one drops
// the address, and a truncated option drops the reply.
func TestDecodeMalformedOptions(t *testing.T) {
	addr := iaAddr("fd00::1", 120, 180)
	for _, tt := range []struct {
		name    string
		opts    []dhcpv6.Option
		ok      bool
		hasAddr bool
	}{
		{"DNS of 17 bytes", []dhcpv6.Option{iaNAOpt(0, 0, addr), raw(optDNSServers, make([]byte, 17))}, true, true},
		{"malformed status in the address", []dhcpv6.Option{iaNAOpt(0, 0, append(addr, 0, byte(optStatusCode), 0, 1, 0))}, true, true},
		{"truncated option in the address", []dhcpv6.Option{iaNAOpt(0, 0, append(addr, 0, 1, 0, 9))}, true, false},
		{"truncated option in the IA", []dhcpv6.Option{raw(optIANA, append(iaNAOpt(0, 0, addr).ToBytes(), 0, 1))}, true, false},
	} {
		msg := encode(msgReply, 1, append([]dhcpv6.Option{raw(optServerID, testServer)}, tt.opts...))
		r, err := decode(msg, 7, time.Now())
		if (err == nil) != tt.ok || r.hasAddr != tt.hasAddr {
			t.Errorf("%s: err %v, hasAddr %v", tt.name, err, r.hasAddr)
		}
	}
	if _, err := decode(append(encode(msgReply, 1, []dhcpv6.Option{raw(optServerID, testServer)}), 0, 23, 0, 16), 7, time.Now()); err == nil {
		t.Error("a truncated option was accepted")
	}
	// 1 to 3 bytes after the last option are ignored, like networkd.
	for n := 1; n <= 3; n++ {
		msg := append(encode(msgReply, 1, []dhcpv6.Option{raw(optServerID, testServer), raw(optDNSServers, net.ParseIP("fd00::53").To16())}), make([]byte, n)...)
		if r, err := decode(msg, 7, time.Now()); err != nil || len(r.lease.DNS) != 1 {
			t.Errorf("%d bytes after the options: %v, DNS %v", n, err, r.lease.DNS)
		}
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(serverReply(encode(msgSolicit, 1, nil), msgReply, true))
	f.Fuzz(func(_ *testing.T, b []byte) { _, _ = decode(b, 7, time.Now()) })
}

// What drops the message in networkd 255 (dhcp6_lease_parse_message): a
// second client or server ID, a preference other than 1 byte, a status
// code under 2 bytes, an information refresh time other than 4 bytes.
func TestDecodeStrictLikeNetworkd(t *testing.T) {
	base := []dhcpv6.Option{raw(optServerID, testServer), raw(optClientID, testDUID)}
	for _, tt := range []struct {
		name string
		opt  dhcpv6.Option
		ok   bool
	}{
		{"valid", raw(optPreference, []byte{7}), true},
		{"second server ID", raw(optServerID, testServer), false},
		{"second client ID", raw(optClientID, testDUID), false},
		{"preference of 2 bytes", raw(optPreference, []byte{0, 7}), false},
		{"preference of 0 bytes", raw(optPreference, nil), false},
		{"status of 1 byte", raw(optStatusCode, []byte{0}), false},
		{"status success", raw(optStatusCode, []byte{0, 0}), true},
		{"status UnspecFail", raw(optStatusCode, []byte{0, 1}), false},
		{"status UnspecFail with a message", raw(optStatusCode, append([]byte{0, 1}, "busy"...)), false},
		{"information refresh time of 3 bytes", raw(optInfoRefresh, []byte{0, 0, 9}), false},
		{"information refresh time of 4 bytes", raw(optInfoRefresh, []byte{0, 0, 0, 9}), true},
		{"information refresh time of 5 bytes", raw(optInfoRefresh, []byte{0, 0, 0, 0, 9}), false},
	} {
		_, err := decode(encode(msgReply, 1, append(slices.Clone(base), tt.opt)), 7, time.Now())
		if (err == nil) != tt.ok {
			t.Errorf("%s: err %v, want ok %v", tt.name, err, tt.ok)
		}
	}
	// An empty first server ID is no server ID for networkd: a second one
	// counts.
	if _, err := decode(encode(msgReply, 1, []dhcpv6.Option{raw(optServerID, nil), raw(optServerID, testServer), raw(optClientID, testDUID)}), 7, time.Now()); err != nil {
		t.Errorf("a server ID after an empty one: %v", err)
	}
}

// A REPLY with a top-level status other than success does not bind, also
// with an IA address: rapid commit and REPLY to REQUEST (networkd drops
// it). NoBinding inside the IA is handled by TestRenewNoBindingRequests.
func TestReplyWithErrorStatusDoesNotBind(t *testing.T) {
	unspecFail := []byte{0, byte(optStatusCode), 0, 2, 0, 1}
	for _, rapid := range []bool{true, false} {
		f := newFakeNet()
		f.answer = func(msg []byte) []byte {
			switch dhcpv6.MessageType(msg[0]) {
			case msgSolicit:
				if rapid {
					return append(serverReply(msg, msgReply, true), unspecFail...)
				}
				return serverReply(msg, msgAdvertise, false)
			case msgRequest:
				return append(serverReply(msg, msgReply, false), unspecFail...)
			}
			return nil
		}
		bound := make(chan *Lease, 1)
		c := f.client(true)
		c.OnBound = func(l *Lease) { bound <- l }
		c.Start()
		want := msgSolicit
		if !rapid {
			want = msgRequest
		}
		for deadline := time.Now().Add(5 * time.Second); f.countType(want) < 3; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("rapid %v: fewer than 3 messages of type %d", rapid, want)
			}
		}
		close(f.stopped)
		c.Stop()
		if len(bound) != 0 {
			t.Errorf("rapid %v: a REPLY with status UnspecFail bound", rapid)
		}
	}
}
