package dhcp6

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/iana"
)

// Message types (RFC 8415 section 7.3).
const (
	msgSolicit            = dhcpv6.MessageTypeSolicit
	msgAdvertise          = dhcpv6.MessageTypeAdvertise
	msgRequest            = dhcpv6.MessageTypeRequest
	msgRenew              = dhcpv6.MessageTypeRenew
	msgRebind             = dhcpv6.MessageTypeRebind
	msgReply              = dhcpv6.MessageTypeReply
	msgRelease            = dhcpv6.MessageTypeRelease
	msgInformationRequest = dhcpv6.MessageTypeInformationRequest
)

// Option codes.
const (
	optClientID      = dhcpv6.OptionClientID
	optServerID      = dhcpv6.OptionServerID
	optIANA          = dhcpv6.OptionIANA
	optIAAddr        = dhcpv6.OptionIAAddr
	optPreference    = dhcpv6.OptionPreference
	optStatusCode    = dhcpv6.OptionStatusCode
	optRapidCommit   = dhcpv6.OptionRapidCommit
	optDNSServers    = dhcpv6.OptionDNSRecursiveNameServer
	optDomainList    = dhcpv6.OptionDomainSearchList
	optInfoRefresh   = dhcpv6.OptionInformationRefreshTime
	optSNTPServers   = dhcpv6.OptionSNTPServerList
	optClientFQDN    = dhcpv6.OptionFQDN
	optNTPServer     = dhcpv6.OptionNTPServer
	optSolMaxRT      = dhcpv6.OptionSolMaxRT
	optCaptivePortal = dhcpv6.OptionCaptivePortal
)

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

func encode(typ dhcpv6.MessageType, xid uint32, opts []dhcpv6.Option) []byte {
	m := dhcpv6.Message{MessageType: typ, TransactionID: dhcpv6.TransactionID{byte(xid >> 16), byte(xid >> 8), byte(xid)}}
	m.Options.Options = opts
	return m.ToBytes()
}

// raw is an option sent as it is: a DUID, opaque to the client (RFC 8415
// section 11), or the FQDN of fqdn.
func raw(code dhcpv6.OptionCode, data []byte) dhcpv6.Option {
	return &dhcpv6.OptionGeneric{OptionCode: code, OptionData: data}
}

// lenient parses an option like the library, but keeps one that does not
// parse as raw bytes: like networkd, a malformed option is ignored, not
// the message, and a malformed option in an IA_NA or an IA address is
// ignored, not the IA or the address. A truncated option still fails the
// message, the IA or the address, as in networkd.
func lenient(code dhcpv6.OptionCode, data []byte) (dhcpv6.Option, error) {
	o, err := dhcpv6.ParseOption(code, data)
	if err == nil {
		return o, nil
	}
	switch {
	case code == optIANA && len(data) >= 12:
		var ia dhcpv6.OptIANA
		if ia.FromBytes(data[:12]) == nil && ia.Options.FromBytesWithParser(data[12:], lenient) == nil {
			return &ia, nil
		}
	case code == optIAAddr && len(data) >= 24:
		var a dhcpv6.OptIAAddress
		if a.FromBytes(data[:24]) == nil && a.Options.FromBytesWithParser(data[24:], lenient) == nil {
			return &a, nil
		}
	}
	return raw(code, append([]byte(nil), data...)), nil
}

// strict checks what drops the whole message in networkd 255
// (dhcp6_lease_parse_message): a client or server identifier after a
// non-empty one, a preference other than 1 byte, a status code under 2
// bytes or other than success, and an information refresh time other
// than 4 bytes. A status inside an IA (NoBinding) is no top-level status.
func strict(opts dhcpv6.Options) bool {
	seen := map[dhcpv6.OptionCode]bool{}
	for _, o := range opts {
		b := o.ToBytes()
		switch o.Code() {
		case optClientID, optServerID:
			if seen[o.Code()] {
				return false
			}
			seen[o.Code()] = len(b) > 0
		case optPreference:
			if len(b) != 1 {
				return false
			}
		case optStatusCode:
			if len(b) < 2 || iana.StatusCode(binary.BigEndian.Uint16(b)) != iana.StatusSuccess {
				return false
			}
		case optInfoRefresh:
			if len(b) != 4 {
				return false
			}
		}
	}
	return true
}

// optionData returns the payload of the first option with code that is
// not empty (networkd sets an identifier only from a non-empty one).
func optionData(opts dhcpv6.Options, code dhcpv6.OptionCode) []byte {
	for _, o := range opts.Get(code) {
		if b := o.ToBytes(); len(b) > 0 {
			return b
		}
	}
	return nil
}

// reply is a decoded ADVERTISE or REPLY.
type reply struct {
	typ         dhcpv6.MessageType
	xid         uint32
	clientID    []byte
	rapidCommit bool
	iaStatus    iana.StatusCode // status code of our IA_NA, 0 = success
	pref        byte            // server preference (option 7), 0 when absent
	lease       Lease
	hasAddr     bool
}

var (
	errShort    = errors.New("short DHCPv6 message")
	errNoServer = errors.New("DHCPv6 reply without server identifier")
	errInvalid  = errors.New("DHCPv6 reply that networkd drops")
)

// decode parses a server message for the IA with iaid.
func decode(b []byte, iaid uint32, now time.Time) (reply, error) {
	if len(b) < dhcpv6.MessageHeaderSize {
		return reply{}, errShort
	}
	var opts dhcpv6.Options
	if err := opts.FromBytesWithParser(withoutTail(b[dhcpv6.MessageHeaderSize:]), lenient); err != nil {
		return reply{}, err //nolint:wrapcheck // the caller drops it
	}
	if !strict(opts) {
		return reply{}, errInvalid
	}
	r := reply{typ: dhcpv6.MessageType(b[0]), xid: uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])}
	r.clientID = optionData(opts, optClientID)
	r.lease.ServerID = optionData(opts, optServerID)
	// RFC 8415 16.3 and 16.10: discard an ADVERTISE or REPLY without one.
	if (r.typ == msgAdvertise || r.typ == msgReply) && len(r.lease.ServerID) == 0 {
		return reply{}, errNoServer
	}
	r.rapidCommit = opts.GetOne(optRapidCommit) != nil
	if p := optionData(opts, optPreference); len(p) == 1 {
		r.pref = p[0]
	}
	mo := dhcpv6.MessageOptions{Options: opts}
	r.lease.DNS = mo.DNS()
	if dl := optionData(opts, optDomainList); dl != nil {
		r.lease.Domains = domainList(dl)
	}
	if irt := mo.InformationRefreshTime(-1); irt >= 0 {
		// At least IRT_MINIMUM, also for 0 (RFC 8415 21.23).
		r.lease.InfoRefresh = max(irt, 600*time.Second)
	}
	for _, o := range opts.Get(optIANA) {
		if ia, ok := o.(*dhcpv6.OptIANA); ok && binary.BigEndian.Uint32(ia.IaId[:]) == iaid {
			if st := ia.Options.Status(); st != nil {
				r.iaStatus = st.StatusCode
			}
			r.hasAddr = r.lease.ia(ia, now)
		}
	}
	return r, nil
}

// ia reads an IA_NA: T1, T2 and its first usable IA address with
// lifetimes (RFC 8415 21.4 and 21.6): an IA with T1 > T2 (both set) is
// discarded, an address with a preferred lifetime over its valid lifetime,
// a valid lifetime of 0 or a status other than success is skipped. T1 and
// T2 of 0 are left to the client (0.5 and 0.8 of the preferred lifetime);
// T1 <= T2 <= valid.
func (l *Lease) ia(ia *dhcpv6.OptIANA, now time.Time) bool {
	t1, t2 := seconds(ia.T1), seconds(ia.T2)
	if t1 > 0 && t2 > 0 && t1 > t2 {
		return false
	}
	for _, o := range ia.Options.Get(optIAAddr) {
		a, ok := o.(*dhcpv6.OptIAAddress)
		if !ok {
			continue // malformed
		}
		if st := a.Options.Status(); st != nil && st.StatusCode != iana.StatusSuccess {
			continue
		}
		pref, valid := seconds(a.PreferredLifetime), seconds(a.ValidLifetime)
		if valid == 0 || pref > valid {
			continue
		}
		l.Addr = a.IPv6Addr
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

func seconds(d time.Duration) uint32 { return uint32(d / time.Second) } //nolint:gosec // from 32 bits

// withoutTail cuts 1 to 3 bytes after the last option, which networkd
// ignores ("Ignoring %zu invalid byte(s) at the end of the packet"); the
// library fails the message. A truncated option stays and fails it.
func withoutTail(b []byte) []byte {
	n := 0
	for len(b)-n >= 4 {
		next := n + 4 + int(binary.BigEndian.Uint16(b[n+2:]))
		if next > len(b) {
			return b
		}
		n = next
	}
	return b[:n]
}

// domainList decodes option 24 like networkd 255
// (dhcp6_option_parse_domainname_list): the last byte is 0, every label
// has at most 63 bytes and lies inside the option, and a name may end at
// the end of the option without the root label. Anything else ignores
// the option as a whole. The names are checked later (validSearchDomain).
func domainList(b []byte) []string {
	if len(b) <= 1 || b[len(b)-1] != 0 {
		return nil
	}
	var out []string
	for len(b) > 0 {
		if len(b) <= 1 {
			return nil // parse_domain: -ENODATA
		}
		var labels []string
		for len(b) > 0 {
			n := int(b[0])
			b = b[1:]
			if n == 0 {
				break
			}
			if n > 63 || n > len(b) {
				return nil
			}
			labels, b = append(labels, string(b[:n])), b[n:]
		}
		if len(labels) > 0 {
			out = append(out, strings.Join(labels, "."))
		}
	}
	return out
}

// fqdn is option 39 like networkd: flags S (the server updates the AAAA
// record) and the host name as a partial DNS name (no root label). The
// library always ends a name with the root label.
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

// iaNA returns an IA_NA for iaid with T1 and T2 of 0, and the address of a
// lease in it when addr is set.
func iaNA(iaid uint32, addr *Lease) dhcpv6.Option {
	ia := &dhcpv6.OptIANA{}
	binary.BigEndian.PutUint32(ia.IaId[:], iaid)
	if addr != nil && addr.Addr != nil {
		ia.Options.Add(&dhcpv6.OptIAAddress{IPv6Addr: addr.Addr})
	}
	return ia
}

// iaPD returns an empty IA_PD for iaid.
func iaPD(iaid uint32) dhcpv6.Option {
	ia := &dhcpv6.OptIAPD{}
	binary.BigEndian.PutUint32(ia.IaId[:], iaid)
	return ia
}

// elapsed is option 8 in hundredths of a second, cut like networkd; the
// library rounds.
func elapsed(d time.Duration) dhcpv6.Option {
	return dhcpv6.OptElapsedTime(d.Truncate(10 * time.Millisecond))
}
