package dhcp4

import (
	"encoding/binary"
	"net"
	"slices"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
	"github.com/insomniacslk/dhcp/rfc1035label"
)

// Lease is what a DHCPACK assigns.
type Lease struct {
	ServerID     net.IP
	FixedAddress net.IP
	Netmask      net.IPMask
	NextServer   net.IP
	Broadcast    net.IP
	Router       []net.IP
	DNS          []net.IP
	TimeServer   []net.IP
	DomainName   string
	MTU          uint16
	// Options are all options of the ACK, for those without a field above.
	Options dhcpv4.Options

	Bound  time.Time
	Renew  time.Time // T1
	Rebind time.Time // T2
	Expire time.Time
}

// minMessageSize is the smallest BOOTP message (RFC 1542 section 2.1);
// shorter messages are padded, as relays may drop them.
const minMessageSize = 300

// message is a DHCP message the client sends.
type message struct {
	typ     dhcpv4.MessageType
	xid     uint32
	secs    uint16
	ciaddr  net.IP
	mac     net.HardwareAddr
	options []dhcpv4.Option
}

// encode returns the BOOTP payload of m: the header of the library, then
// the options in the order of m (networkd's, as captured in kindmetal),
// padded before End to 300 bytes. dhcpv4.Options is a map that marshals
// sorted by code and pads after End, which would change the wire.
func (m *message) encode() []byte {
	d := dhcpv4.DHCPv4{OpCode: dhcpv4.OpcodeBootRequest, HWType: iana.HWTypeEthernet, NumSeconds: m.secs,
		ClientIPAddr: ipOrZero(m.ciaddr), ClientHWAddr: m.mac}
	binary.BigEndian.PutUint32(d.TransactionID[:], m.xid)
	b := d.ToBytes()[:bootpLen]
	for _, o := range append([]dhcpv4.Option{dhcpv4.OptMessageType(m.typ)}, m.options...) {
		v := o.Value.ToBytes()
		b = append(append(b, o.Code.Code(), byte(len(v))), v...)
	}
	for len(b)+1 < minMessageSize {
		b = append(b, byte(dhcpv4.OptionPad))
	}
	return append(b, byte(dhcpv4.OptionEnd))
}

func ipOrZero(ip net.IP) net.IP {
	if ip4 := ip.To4(); ip4 != nil {
		return ip4
	}
	return net.IPv4zero
}

// reply is a decoded DHCP message from a server.
type reply struct {
	typ   dhcpv4.MessageType
	xid   uint32
	mac   net.HardwareAddr
	lease Lease
	// rapidCommit is set when an ACK answers a DISCOVER (RFC 4039).
	rapidCommit bool
	// overload is set with option 52: options in sname or file, which the
	// client does not read, so an OFFER or ACK with it is dropped.
	overload bool
}

// bootpLen is the fixed BOOTP header and the magic cookie (RFC 2131).
const bootpLen = 240

// decodeReply parses a BOOTP payload; ok is false for anything that is
// not a usable server reply. Like networkd it drops an OFFER or ACK
// without an address, a server identifier (54) or a lease time (51), and a
// NAK without a server identifier.
func decodeReply(payload []byte, now time.Time) (reply, bool) {
	// Ethernet only (htype 1, hlen 6).
	if len(payload) < bootpLen || payload[1] != 1 || payload[2] != 6 {
		return reply{}, false
	}
	// The library reads the header; the options are read like networkd.
	d, err := dhcpv4.FromBytes(append(payload[:bootpLen:bootpLen], byte(dhcpv4.OptionEnd)))
	if err != nil || d.OpCode != dhcpv4.OpcodeBootReply {
		return reply{}, false
	}
	opts, ok := options(payload[bootpLen:])
	if !ok {
		return reply{}, false
	}
	d.Options = opts
	r := reply{
		typ: d.MessageType(), xid: binary.BigEndian.Uint32(d.TransactionID[:]), mac: d.ClientHWAddr,
		rapidCommit: d.Options.Has(dhcpv4.OptionRapidCommit), overload: d.Options.Has(dhcpv4.OptionOptionOverload),
	}
	l := &r.lease
	l.FixedAddress, l.Bound, l.Options = d.YourIPAddr.To4(), now, d.Options
	if !d.ServerIPAddr.IsUnspecified() {
		l.NextServer = d.ServerIPAddr.To4()
	}
	l.ServerID, l.Netmask, l.Broadcast, l.DomainName = d.ServerIdentifier(), d.SubnetMask(), d.BroadcastAddress(), d.DomainName()
	l.Router, l.DNS, l.TimeServer = d.Router(), d.DNS(), dhcpv4.GetIPs(dhcpv4.OptionTimeServer, d.Options)
	if mtu, err := dhcpv4.GetUint16(dhcpv4.OptionInterfaceMTU, d.Options); err == nil {
		// networkd raises a value below 576 (also 0) to 576; 0 in MTU
		// means no option 26.
		l.MTU = max(mtu, 576)
	}
	t := leaseTimes{lease: seconds(d.IPAddressLeaseTime(0)), t1: seconds(d.IPAddressRenewalTime(0)), t2: seconds(d.IPAddressRebindingTime(0))}
	t.set(l, now)
	ok = r.usable(t.lease > 0) // networkd drops a lease time of 0
	return r, ok
}

// valid checks the options that networkd checks per copy
// (sd-dhcp-lease.c); a copy that fails is ignored, an earlier one stays.
// networkd also ignores a copy of 12 or 15 that is no valid DNS name;
// BOOTy keeps the last copy of these and checks the domain name when it
// uses it (validSearchDomain).
var valid = map[uint8]func([]byte) bool{
	dhcpv4.OptionSubnetMask.Code():           length(4),
	dhcpv4.OptionBroadcastAddress.Code():     length(4),
	dhcpv4.OptionServerIdentifier.Code():     length(4),
	dhcpv4.OptionIPAddressLeaseTime.Code():   length(4),
	dhcpv4.OptionRenewTimeValue.Code():       length(4),
	dhcpv4.OptionRebindingTimeValue.Code():   length(4),
	dhcpv4.OptionInterfaceMTU.Code():         length(2),
	dhcpv4.OptionRouter.Code():               multiple(4),
	dhcpv4.OptionTimeServer.Code():           multiple(4),
	dhcpv4.OptionDomainNameServer.Code():     multiple(4),
	dhcpv4.OptionNTPServers.Code():           multiple(4),
	dhcpv4.OptionStaticRoutingTable.Code():   multiple(8),
	dhcpv4.OptionClasslessStaticRoute.Code(): func(b []byte) bool { return len(b) > 0 }, // cut by classless
	dhcpv4.OptionDNSDomainSearchList.Code():  func(b []byte) bool { return len(b) > 0 && SearchDomains(b) != nil },
}

// classless cuts option 121 after its last whole route, like networkd
// (lease_parse_classless_routes keeps the routes before an error).
func classless(b []byte) []byte {
	n := 0
	for n < len(b) {
		width := int(b[n])
		next := n + 1 + (width+7)/8 + 4
		if width > 32 || next > len(b) {
			break
		}
		n = next
	}
	return b[:n]
}

func length(n int) func([]byte) bool   { return func(b []byte) bool { return len(b) == n } }
func multiple(n int) func([]byte) bool { return func(b []byte) bool { return len(b)%n == 0 } }

// options reads the options like networkd (parse_options): up to End or
// the end of the message, a later valid copy of an option replaces an
// earlier one, the routes of valid copies of 33 and 121 add up, and a
// truncated option or a message type or overload option of another
// length than 1 fails the message. The library concatenates all
// copies (RFC 3396).
func options(b []byte) (dhcpv4.Options, bool) {
	opts, malformed := dhcpv4.Options{}, map[uint8]bool{}
	for len(b) > 0 {
		code := b[0]
		switch {
		case code == dhcpv4.OptionPad.Code():
			b = b[1:]
			continue
		case code == dhcpv4.OptionEnd.Code():
			return opts, true
		case len(b) < 2 || len(b) < 2+int(b[1]):
			return nil, false
		}
		data := b[2 : 2+int(b[1])]
		b = b[2+int(b[1]):]
		switch code {
		case dhcpv4.OptionDHCPMessageType.Code(), dhcpv4.OptionOptionOverload.Code():
			if len(data) != 1 {
				return nil, false // networkd: -EINVAL for the message
			}
		case dhcpv4.OptionClasslessStaticRoute.Code():
			data = classless(data)
		}
		if ok, checked := valid[code]; checked && !ok(data) {
			if _, seen := opts[code]; !seen {
				// Kept so that the option counts as present; its decoder
				// rejects it, as before. A valid copy replaces it.
				opts[code], malformed[code] = append([]byte(nil), data...), true
			}
			continue
		}
		if malformed[code] {
			delete(opts, code)
			delete(malformed, code)
		}
		if code == dhcpv4.OptionStaticRoutingTable.Code() || code == dhcpv4.OptionClasslessStaticRoute.Code() {
			opts[code] = append(opts[code], data...)
		} else {
			opts[code] = append([]byte(nil), data...)
		}
	}
	return opts, true
}

// SearchDomains decodes option 119 (RFC 3397): DNS names in wire format,
// with compression pointers into the option. A malformed option is
// ignored as a whole, like networkd (-EBADMSG): rfc1035label rejects a
// truncated name, a pointer that does not point back and a reserved label
// type; a last name without its root label, which rfc1035label takes as a
// partial name (RFC 4704), is checked here.
func SearchDomains(data []byte) []string {
	l, err := rfc1035label.FromBytes(data)
	if err != nil || !terminated(data) {
		return nil
	}
	return slices.DeleteFunc(l.Labels, func(s string) bool { return s == "" })
}

// terminated tells whether every name in data ends with the root label or
// a compression pointer.
func terminated(b []byte) bool {
	inName := false
	for len(b) > 0 {
		switch n := int(b[0]); {
		case n == 0:
			b, inName = b[1:], false
		case n&0xc0 == 0xc0 && len(b) >= 2:
			b, inName = b[2:], false
		case len(b) > n:
			b, inName = b[1+n:], true
		default:
			return false
		}
	}
	return !inName
}

func seconds(d time.Duration) uint32 { return uint32(d / time.Second) } //nolint:gosec // from 32 bits

// usable tells whether a reply has what networkd requires: an OFFER or
// ACK an address, a server identifier and a lease time (hasLease), a NAK
// a server identifier.
func (r *reply) usable(hasLease bool) bool {
	switch r.typ {
	case dhcpv4.MessageTypeOffer, dhcpv4.MessageTypeAck:
		yi := r.lease.FixedAddress
		sid := r.lease.ServerID // networkd: server_address == 0 is no server
		return yi != nil && !yi.IsUnspecified() && sid != nil && !sid.IsUnspecified() && hasLease && !r.overload
	case dhcpv4.MessageTypeNak:
		return r.lease.ServerID != nil
	default:
		return true
	}
}

// leaseTimes are options 51, 58 and 59 in seconds, 0 when absent.
type leaseTimes struct {
	lease, t1, t2 uint32
}

// set derives the lease times like networkd (sd-dhcp-client.c): T2 defaults
// to 7/8 and T1 to 1/2 of the lease time (RFC 2131 section 4.4.5), a T2 at
// or past the lease time and a T1 at or past T2 too.
func (t leaseTimes) set(l *Lease, now time.Time) {
	if t.lease == 0 {
		return
	}
	if t.t2 == 0 || t.t2 >= t.lease {
		t.t2 = uint32(uint64(t.lease) * 7 / 8)
	}
	if t.t1 == 0 || t.t1 >= t.t2 {
		t.t1 = t.lease / 2
		if t.t1 >= t.t2 {
			t.t2 = uint32(uint64(t.lease) * 7 / 8)
		}
	}
	l.Expire = now.Add(time.Duration(t.lease) * time.Second)
	l.Renew = now.Add(time.Duration(t.t1) * time.Second)
	l.Rebind = now.Add(time.Duration(t.t2) * time.Second)
}
