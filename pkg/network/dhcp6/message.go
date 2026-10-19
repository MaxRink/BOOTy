package dhcp6

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"time"
)

// Message types (RFC 8415 section 7.3).
const (
	msgSolicit            byte = 1
	msgAdvertise          byte = 2
	msgRequest            byte = 3
	msgRenew              byte = 5
	msgRebind             byte = 6
	msgReply              byte = 7
	msgRelease            byte = 8
	msgInformationRequest byte = 11
)

// Option codes.
const (
	optClientID      uint16 = 1
	optServerID      uint16 = 2
	optIANA          uint16 = 3
	optIAAddr        uint16 = 5
	optORO           uint16 = 6
	optPreference    uint16 = 7
	optElapsedTime   uint16 = 8
	optStatusCode    uint16 = 13
	optRapidCommit   uint16 = 14
	optDNSServers    uint16 = 23
	optDomainList    uint16 = 24
	optInfoRefresh   uint16 = 32
	optIAPD          uint16 = 25
	optSNTPServers   uint16 = 31
	optClientFQDN    uint16 = 39
	optNTPServer     uint16 = 56
	optSolMaxRT      uint16 = 82
	optCaptivePortal uint16 = 103
)

// statusNoBinding is the status code of an IA the server has no binding
// for (RFC 8415 section 21.13).
const statusNoBinding uint16 = 3

// Lease is the address a REPLY assigns (IA_NA) and its options.
type Lease struct {
	Addr      net.IP
	Preferred time.Duration
	Valid     time.Duration
	ServerID  []byte
	DNS       []net.IP
	Domains   []string
	// InfoRefresh is the information refresh time (option 32, at least
	// 600 s) of a stateless REPLY, 0 when absent.
	InfoRefresh time.Duration

	Bound  time.Time
	Renew  time.Time // T1
	Rebind time.Time // T2
	Expire time.Time // end of the valid lifetime
}

// option is one TLV.
type option struct {
	code uint16
	data []byte
}

func encode(typ byte, xid uint32, opts []option) []byte {
	b := []byte{typ, byte(xid >> 16), byte(xid >> 8), byte(xid)}
	for _, o := range opts {
		b = binary.BigEndian.AppendUint16(b, o.code)
		b = binary.BigEndian.AppendUint16(b, uint16(len(o.data))) //nolint:gosec // short options
		b = append(b, o.data...)
	}
	return b
}

// parseOptions splits TLVs; a truncated option ends the list.
func parseOptions(b []byte) []option {
	var out []option
	for len(b) >= 4 {
		code, n := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+n {
			break
		}
		out = append(out, option{code: code, data: b[4 : 4+n]})
		b = b[4+n:]
	}
	return out
}

func find(opts []option, code uint16) ([]byte, bool) {
	for _, o := range opts {
		if o.code == code {
			return o.data, true
		}
	}
	return nil, false
}

// reply is a decoded ADVERTISE or REPLY.
type reply struct {
	typ         byte
	xid         uint32
	clientID    []byte
	rapidCommit bool
	status      uint16 // top-level status code, 0 = success
	iaStatus    uint16 // status code of our IA_NA, 0 = success
	pref        byte   // server preference (option 7), 0 when absent
	lease       Lease
	hasAddr     bool
}

var (
	errShort    = errors.New("short DHCPv6 message")
	errNoServer = errors.New("DHCPv6 reply without server identifier")
)

// decode parses a server message for the IA with iaid.
func decode(b []byte, iaid uint32, now time.Time) (reply, error) {
	if len(b) < 4 {
		return reply{}, errShort
	}
	r := reply{typ: b[0], xid: uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])}
	opts := parseOptions(b[4:])
	r.clientID, _ = find(opts, optClientID)
	r.lease.ServerID, _ = find(opts, optServerID)
	// RFC 8415 16.3 and 16.10: discard an ADVERTISE or REPLY without one.
	if (r.typ == msgAdvertise || r.typ == msgReply) && len(r.lease.ServerID) == 0 {
		return reply{}, errNoServer
	}
	_, r.rapidCommit = find(opts, optRapidCommit)
	if p, ok := find(opts, optPreference); ok && len(p) == 1 {
		r.pref = p[0]
	}
	if st, ok := find(opts, optStatusCode); ok && len(st) >= 2 {
		r.status = binary.BigEndian.Uint16(st)
	}
	if dns, ok := find(opts, optDNSServers); ok {
		for i := 0; i+16 <= len(dns); i += 16 {
			r.lease.DNS = append(r.lease.DNS, net.IP(append([]byte(nil), dns[i:i+16]...)))
		}
	}
	r.lease.otherOptions(opts)
	for _, o := range opts {
		if o.code == optIANA && len(o.data) >= 12 && binary.BigEndian.Uint32(o.data) == iaid {
			r.iaNA(o.data, now)
		}
	}
	return r, nil
}

// iaNA reads our IA_NA: its status code and its address.
func (r *reply) iaNA(b []byte, now time.Time) {
	if st, ok := find(parseOptions(b[12:]), optStatusCode); ok && len(st) >= 2 {
		r.iaStatus = binary.BigEndian.Uint16(st)
	}
	r.hasAddr = r.lease.ia(b, now)
}

// otherOptions reads the domain search list (24) and the information
// refresh time (32).
func (l *Lease) otherOptions(opts []option) {
	if dl, ok := find(opts, optDomainList); ok {
		l.Domains = domainList(dl)
	}
	if v, ok := find(opts, optInfoRefresh); ok && len(v) == 4 {
		// At least IRT_MINIMUM, also for 0 (RFC 8415 21.23).
		l.InfoRefresh = max(time.Duration(binary.BigEndian.Uint32(v))*time.Second, 600*time.Second)
	}
}

// ia reads an IA_NA: T1, T2 and its first usable IA address with
// lifetimes (RFC 8415 21.4 and 21.6): an IA with T1 > T2 (both set) is
// discarded, an address with a preferred lifetime over its valid lifetime,
// a valid lifetime of 0 or a status other than success is skipped. T1 and
// T2 of 0 are left to the client (0.5 and 0.8 of the preferred lifetime);
// T1 <= T2 <= valid.
func (l *Lease) ia(b []byte, now time.Time) bool {
	t1, t2 := binary.BigEndian.Uint32(b[4:]), binary.BigEndian.Uint32(b[8:])
	if t1 > 0 && t2 > 0 && t1 > t2 {
		return false
	}
	for _, o := range parseOptions(b[12:]) {
		if o.code != optIAAddr || len(o.data) < 24 {
			continue
		}
		if st, ok := find(parseOptions(o.data[24:]), optStatusCode); ok && len(st) >= 2 && binary.BigEndian.Uint16(st) != 0 {
			continue
		}
		pref, valid := binary.BigEndian.Uint32(o.data[16:]), binary.BigEndian.Uint32(o.data[20:])
		if valid == 0 || pref > valid {
			continue
		}
		l.Addr = net.IP(append([]byte(nil), o.data[:16]...))
		l.Preferred, l.Valid = time.Duration(pref)*time.Second, time.Duration(valid)*time.Second
		if t1 == 0 {
			t1 = pref / 2
		}
		if t2 == 0 {
			t2 = uint32(uint64(pref) * 4 / 5)
		}
		t2 = min(t2, valid)
		t1 = min(t1, t2)
		l.Bound = now
		l.Renew, l.Rebind = now.Add(time.Duration(t1)*time.Second), now.Add(time.Duration(t2)*time.Second)
		l.Expire = now.Add(l.Valid)
		return true
	}
	return false
}

// domainList decodes DNS wire-format names (RFC 1035, no compression).
func domainList(b []byte) []string {
	var out []string
	var labels []string
	for len(b) > 0 {
		n := int(b[0])
		if n == 0 {
			if len(labels) > 0 {
				out = append(out, strings.Join(labels, "."))
			}
			labels, b = nil, b[1:]
			continue
		}
		if len(b) < 1+n {
			break
		}
		labels, b = append(labels, string(b[1:1+n])), b[1+n:]
	}
	return out
}

// fqdn is option 39 like networkd: flags S (the server updates the AAAA
// record) and the host name as a partial DNS name (no root label).
func fqdn(hostname string) []byte {
	b := []byte{0x01}
	for _, l := range strings.Split(strings.TrimSuffix(hostname, "."), ".") {
		if l == "" || len(l) > 63 {
			continue
		}
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return b
}

// ia returns an IA_NA (or IA_PD) option for iaid with T1 and T2 of 0, and
// the address of a lease in it when addr is set.
func iaOption(code uint16, iaid uint32, addr *Lease) option {
	b := binary.BigEndian.AppendUint32(nil, iaid)
	b = append(b, make([]byte, 8)...)
	if addr != nil && addr.Addr != nil {
		ia := append([]byte(nil), addr.Addr.To16()...)
		ia = append(ia, make([]byte, 8)...)
		b = binary.BigEndian.AppendUint16(b, optIAAddr)
		b = binary.BigEndian.AppendUint16(b, uint16(len(ia))) //nolint:gosec // 24 bytes
		b = append(b, ia...)
	}
	return option{code: code, data: b}
}

func oro(codes ...uint16) option {
	var b []byte
	for _, c := range codes {
		b = binary.BigEndian.AppendUint16(b, c)
	}
	return option{code: optORO, data: b}
}

func elapsed(d time.Duration) option {
	cs := d / (10 * time.Millisecond)
	return option{code: optElapsedTime, data: binary.BigEndian.AppendUint16(nil, uint16(min(max(cs, 0), 0xffff)))} //nolint:gosec // bounded
}
