package dhcp4

import (
	"encoding/binary"
	"net"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// Option is a DHCP option that the Lease does not decode.
type Option struct {
	Type layers.DHCPOpt
	Data []byte
}

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
	// OtherOptions are the options that have no field above.
	OtherOptions []Option

	Bound  time.Time
	Renew  time.Time // T1
	Rebind time.Time // T2
	Expire time.Time
}

// optRapidCommit is option 80 (RFC 4039), optOverload option 52.
const (
	optRapidCommit layers.DHCPOpt = 80
	optOverload    layers.DHCPOpt = 52
)

// minMessageSize is the smallest BOOTP message (RFC 1542 section 2.1);
// shorter messages are padded, as relays may drop them.
const minMessageSize = 300

// message is a DHCP message the client sends.
type message struct {
	typ     layers.DHCPMsgType
	xid     uint32
	secs    uint16
	ciaddr  net.IP
	mac     net.HardwareAddr
	options []layers.DHCPOption
}

// encode returns the BOOTP payload of m.
func (m *message) encode() ([]byte, error) {
	d := &layers.DHCPv4{
		Operation:    layers.DHCPOpRequest,
		HardwareType: layers.LinkTypeEthernet,
		HardwareLen:  6,
		Xid:          m.xid,
		Secs:         m.secs,
		ClientIP:     ipOrZero(m.ciaddr),
		YourClientIP: net.IPv4zero,
		NextServerIP: net.IPv4zero,
		RelayAgentIP: net.IPv4zero,
		ClientHWAddr: m.mac,
		Options:      append([]layers.DHCPOption{layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(m.typ)})}, m.options...),
	}
	for d.Len() < minMessageSize {
		d.Options = append(d.Options, layers.NewDHCPOption(layers.DHCPOptPad, nil))
	}
	buf := gopacket.NewSerializeBuffer()
	if err := d.SerializeTo(buf, gopacket.SerializeOptions{}); err != nil {
		return nil, err //nolint:wrapcheck // the caller names the message
	}
	return buf.Bytes(), nil
}

func ipOrZero(ip net.IP) net.IP {
	if ip4 := ip.To4(); ip4 != nil {
		return ip4
	}
	return net.IPv4zero
}

// reply is a decoded DHCP message from a server.
type reply struct {
	typ   layers.DHCPMsgType
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
	// Ethernet only (htype 1, hlen 6): gopacket slices chaddr by hlen
	// unchecked, and one frame with hlen 255 would end the process.
	if len(payload) < bootpLen || payload[1] != 1 || payload[2] != 6 {
		return reply{}, false
	}
	var d layers.DHCPv4
	if err := d.DecodeFromBytes(payload, gopacket.NilDecodeFeedback); err != nil || d.Operation != layers.DHCPOpReply {
		return reply{}, false
	}
	r := reply{xid: d.Xid, mac: d.ClientHWAddr}
	r.lease.FixedAddress = d.YourClientIP.To4()
	r.lease.Bound = now
	if d.NextServerIP != nil && !d.NextServerIP.IsUnspecified() {
		r.lease.NextServer = d.NextServerIP.To4()
	}
	var t leaseTimes
	for _, o := range d.Options {
		r.apply(o, &t)
	}
	t.set(&r.lease, now)
	ok := r.usable(t.has && t.lease > 0) // networkd drops a lease time of 0
	return r, ok
}

// usable tells whether a reply has what networkd requires: an OFFER or
// ACK an address, a server identifier and a lease time (hasLease), a NAK
// a server identifier.
func (r *reply) usable(hasLease bool) bool {
	switch r.typ {
	case layers.DHCPMsgTypeOffer, layers.DHCPMsgTypeAck:
		yi := r.lease.FixedAddress
		return yi != nil && !yi.IsUnspecified() && r.lease.ServerID != nil && hasLease && !r.overload
	case layers.DHCPMsgTypeNak:
		return r.lease.ServerID != nil
	default:
		return true
	}
}

// leaseTimes are options 51, 58 and 59 in seconds; has is set when the
// reply carries option 51.
type leaseTimes struct {
	lease, t1, t2 uint32
	has           bool
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

// apply decodes one option into the reply.
func (r *reply) apply(o layers.DHCPOption, t *leaseTimes) {
	switch o.Type {
	case layers.DHCPOptMessageType:
		if len(o.Data) == 1 {
			r.typ = layers.DHCPMsgType(o.Data[0])
		}
	case optRapidCommit:
		r.rapidCommit = true
	case optOverload:
		r.overload = true
	case layers.DHCPOptLeaseTime:
		t.lease, t.has = u32(o.Data), len(o.Data) == 4
	case layers.DHCPOptT1:
		t.t1 = u32(o.Data)
	case layers.DHCPOptT2:
		t.t2 = u32(o.Data)
	default:
		if !r.lease.apply(o) {
			r.lease.OtherOptions = append(r.lease.OtherOptions, Option{Type: o.Type, Data: append([]byte(nil), o.Data...)})
		}
	}
}

// apply decodes an option that the Lease has a field for.
func (l *Lease) apply(o layers.DHCPOption) bool {
	switch o.Type {
	case layers.DHCPOptSubnetMask:
		l.Netmask = net.IPMask(o.Data)
	case layers.DHCPOptRouter:
		l.Router = ipList(o.Data)
	case layers.DHCPOptDNS:
		l.DNS = ipList(o.Data)
	case layers.DHCPOptTimeServer:
		l.TimeServer = ipList(o.Data)
	case layers.DHCPOptBroadcastAddr:
		l.Broadcast = ipOne(o.Data)
	case layers.DHCPOptServerID:
		l.ServerID = ipOne(o.Data)
	case layers.DHCPOptDomainName:
		l.DomainName = string(o.Data)
	case layers.DHCPOptInterfaceMTU:
		if len(o.Data) == 2 {
			// networkd raises a value below 576 (also 0) to 576; 0 in MTU
			// means no option 26.
			l.MTU = max(binary.BigEndian.Uint16(o.Data), 576)
		}
	default:
		return false
	}
	return true
}

func u32(b []byte) uint32 {
	if len(b) != 4 {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func ipOne(b []byte) net.IP {
	if len(b) != net.IPv4len {
		return nil
	}
	return net.IP(append([]byte(nil), b...))
}

func ipList(b []byte) []net.IP {
	var out []net.IP
	for i := 0; i+net.IPv4len <= len(b); i += net.IPv4len {
		out = append(out, net.IP(append([]byte(nil), b[i:i+net.IPv4len]...)))
	}
	return out
}
