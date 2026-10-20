//go:build linux

package network

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/telekom/BOOTy/pkg/network/dhcp6"
	"github.com/telekom/BOOTy/pkg/network/lldp"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// DHCPv6 in DHCP mode, like systemd-networkd with the legacy netplan
// (DHCPv6Client=yes in [IPv6AcceptRA]): a router advertisement with the M
// flag starts a stateful client (an address), one with only the O flag an
// INFORMATION-REQUEST client (DNS servers). The address goes on the NIC as
// /128 with its lifetimes, the DNS servers into resolv.conf after those of
// DHCPv4. The kernel keeps doing SLAAC.

// dhcp6Client is the part of a dhcp6 client that DHCPMode uses.
type dhcp6Client interface {
	Start()
	Stop()
}

// nic6 is the DHCPv6 state of one NIC.
type nic6 struct {
	link   netlink.Link
	stopRA context.CancelFunc
	client dhcp6Client
	// stateful is set when client asks for an address (the RA had M).
	stateful bool
	addr     *netlink.Addr
	dns      []string
	// domains is the domain search list (option 24).
	domains []string
}

// Seams for tests.
var (
	dhcp6WatchRA = watchRA
	dhcp6New     = func(c *dhcp6.Client) dhcp6Client { return c }
	dhcp6AddrAdd = netlink.AddrReplace
	dhcp6AddrDel = netlink.AddrDel
	dhcp6SendRS  = sendRS
	dhcp6RSWait  = 4 * time.Second // RTR_SOLICITATION_INTERVAL, RFC 4861
	// dhcp6RSDelay is the wait before the first router solicitation:
	// sd-ndisc sends it 0.5 s after the start with 1 s timer accuracy, and
	// RFC 4861 6.3.7 asks for a random delay of up to
	// MAX_RTR_SOLICITATION_DELAY (1 s): 0.5 s to 1 s, random.
	dhcp6RSDelay = func() time.Duration {
		return 500*time.Millisecond + rand.N(500*time.Millisecond) //nolint:gosec // timing jitter
	}
)

// ListenICMPv6 opens an ICMPv6 socket bound to the NIC (SO_BINDTODEVICE).
func ListenICMPv6(ctx context.Context, ifname string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		err := rc.Control(func(fd uintptr) { serr = unix.BindToDevice(int(fd), ifname) })
		return errors.Join(err, serr)
	}}
	conn, err := lc.ListenPacket(ctx, "ip6:ipv6-icmp", "::")
	if err != nil {
		return nil, fmt.Errorf("ICMPv6 socket on %s: %w", ifname, err)
	}
	return conn, nil
}

// sendRS sends a router solicitation to all routers with the source
// link-layer address option, as networkd's NDisc client does (the kernel
// fills in the checksum).
func sendRS(ifname string, mac net.HardwareAddr) error {
	conn, err := ListenICMPv6(context.Background(), ifname)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := ipv6.NewPacketConn(conn).SetMulticastHopLimit(255); err != nil {
		return fmt.Errorf("hop limit on %s: %w", ifname, err)
	}
	msg := []byte{byte(ipv6.ICMPTypeRouterSolicitation), 0, 0, 0, 0, 0, 0, 0}
	if len(mac) == 6 {
		msg = append(append(msg, 1, 1), mac...) // option 1, 8 bytes
	}
	if _, err := conn.WriteTo(msg, &net.IPAddr{IP: net.IPv6linklocalallrouters, Zone: ifname}); err != nil {
		return fmt.Errorf("router solicitation on %s: %w", ifname, err)
	}
	return nil
}

// watchRA calls fn with the M and O flags of every router advertisement
// that the NIC receives, until ctx ends.
func watchRA(ctx context.Context, iface *net.Interface, fn func(managed, other bool)) error {
	conn, err := ListenICMPv6(ctx, iface.Name)
	if err != nil {
		return err
	}
	var f ipv6.ICMPFilter
	f.SetAll(true)
	f.Accept(ipv6.ICMPTypeRouterAdvertisement)
	if err := ipv6.NewPacketConn(conn).SetICMPFilter(&f); err != nil {
		_ = conn.Close()
		return fmt.Errorf("ICMPv6 filter on %s: %w", iface.Name, err)
	}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	pc := ipv6.NewPacketConn(conn)
	if err := pc.SetControlMessage(ipv6.FlagHopLimit, true); err != nil {
		_ = conn.Close()
		return fmt.Errorf("ICMPv6 hop limit on %s: %w", iface.Name, err)
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, cm, src, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if validRA(buf[:n], cm, src) {
				fn(buf[5]&0x80 != 0, buf[5]&0x40 != 0)
			}
		}
	}()
	return nil
}

// validRA checks a router advertisement like RFC 4861 6.1.2: hop limit 255
// (it was not forwarded) and a link-local source.
func validRA(b []byte, cm *ipv6.ControlMessage, src net.Addr) bool {
	a, ok := src.(*net.IPAddr)
	return len(b) >= 16 && b[0] == byte(ipv6.ICMPTypeRouterAdvertisement) && b[1] == 0 &&
		cm != nil && cm.HopLimit == 255 && ok && a.IP.IsLinkLocalUnicast()
}

// startRA watches the router advertisements of a NIC with a DHCPv4 client.
// The caller holds d.mu, so a stop does not miss the watch; watchRA does
// not block, and its callbacks run on their own goroutine.
func (d *DHCPMode) startRA(link netlink.Link, iface *net.Interface, hostname string) {
	ctx, cancel := context.WithCancel(context.Background())
	idx := link.Attrs().Index
	if d.v6 == nil {
		d.v6 = map[int]*nic6{}
	}
	if old := d.v6[idx]; old != nil && old.stopRA != nil {
		old.stopRA()
	}
	d.v6[idx] = &nic6{link: link, stopRA: cancel}
	if err := dhcp6WatchRA(ctx, iface, func(m, o bool) { d.onRA(idx, iface, hostname, m, o) }); err != nil {
		d.log.Warn("no DHCPv6 on this NIC", "interface", iface.Name, "error", err)
	}
}

// onRA starts the DHCPv6 client of a NIC on the first advertisement with
// the M or O flag; a NIC without carrier waits for the next one.
func (d *DHCPMode) onRA(idx int, iface *net.Interface, hostname string, managed, other bool) {
	if !managed && !other {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.v6[idx]
	if d.stopped || n == nil {
		return
	}
	if n.client != nil {
		if !managed || n.stateful {
			return
		}
		// A later RA has the M flag: the stateless client makes room for a
		// stateful one, like networkd. Stop waits for the client, whose
		// callbacks take d.mu.
		old := n.client
		n.client = nil
		d.mu.Unlock()
		old.Stop()
		d.mu.Lock()
		if n = d.v6[idx]; d.stopped || n == nil || n.client != nil {
			return
		}
	}
	if nc := d.clients[idx]; nc != nil && nc.down {
		return
	}
	id := dhcpClientID(iface.Name, lldp.MachineID())
	c := &dhcp6.Client{
		Iface:       iface,
		Hostname:    hostname,
		DUID:        id[5:], // the DUID-EN of DHCPv4 option 61
		IAID:        binary.BigEndian.Uint32(id[1:5]),
		Stateful:    managed,
		OnBound:     d.onBound6(idx),
		OnExpire:    d.onExpire6(idx),
		SendRelease: true,
		Log:         d.log,
	}
	n.client, n.stateful = dhcp6New(c), managed
	d.log.Info("Starting DHCPv6", "interface", iface.Name, "managed", managed)
	n.client.Start()
}

func (d *DHCPMode) onBound6(idx int) dhcp6.Callback {
	return func(l *dhcp6.Lease) {
		d.mu.Lock()
		defer d.mu.Unlock()
		n := d.v6[idx]
		if d.stopped || n == nil {
			return
		}
		if l.Addr != nil {
			addr := &netlink.Addr{IPNet: &net.IPNet{IP: l.Addr, Mask: net.CIDRMask(128, 128)},
				ValidLft: int(l.Valid.Seconds()), PreferedLft: int(l.Preferred.Seconds())}
			if err := dhcp6AddrAdd(n.link, addr); err != nil {
				d.log.Warn("failed to add the DHCPv6 address", "iface", n.link.Attrs().Name, "addr", l.Addr, "error", err)
			} else {
				if n.addr != nil && !n.addr.IP.Equal(l.Addr) {
					d.dropAddr6(n)
				}
				n.addr = addr
			}
		}
		n.dns, n.domains = nil, nil
		for _, ip := range l.DNS {
			s := ip.String()
			if ip.IsLinkLocalUnicast() {
				s += "%" + n.link.Attrs().Name // a link-local server needs its zone
			}
			n.dns = append(n.dns, s)
		}
		n.domains = l.Domains // option 24, like networkd
		d.writeResolvers()
	}
}

func (d *DHCPMode) onExpire6(idx int) dhcp6.Callback {
	return func(*dhcp6.Lease) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if n := d.v6[idx]; n != nil {
			d.dropAddr6(n)
			n.dns, n.domains = nil, nil
			d.writeResolvers()
		}
	}
}

// dropAddr6 removes the DHCPv6 address of a NIC. The caller holds d.mu.
func (d *DHCPMode) dropAddr6(n *nic6) {
	if n.addr == nil {
		return
	}
	if err := dhcp6AddrDel(n.link, n.addr); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
		d.log.Warn("failed to remove the DHCPv6 address", "iface", n.link.Attrs().Name, "error", err)
	}
	n.addr = nil
}

// stop6 stops the DHCPv6 client of a NIC (it releases its address) and
// removes the address and DNS servers; the RA watch stays unless all is
// set. The caller does not hold d.mu: Stop waits for the client, whose
// callbacks take d.mu.
func (d *DHCPMode) stop6(idx int, all bool) {
	d.mu.Lock()
	n := d.v6[idx]
	if n == nil {
		d.mu.Unlock()
		return
	}
	c := n.client
	n.client = nil
	if all {
		if n.stopRA != nil {
			n.stopRA()
		}
		delete(d.v6, idx)
	}
	d.mu.Unlock()
	if c != nil {
		c.Stop()
	}
	d.mu.Lock()
	d.dropAddr6(n)
	n.dns, n.domains = nil, nil
	d.writeResolvers()
	d.mu.Unlock()
}

// solicitRA asks the routers of a NIC whose carrier came back for an RA,
// which starts DHCPv6 again: networkd restarts its NDisc client with the
// carrier and sends router solicitations (captured in the CAPRF L2 fault
// windows); the kernel sends them only when the link comes up. It tries
// three times, 4 s apart (RFC 4861), until a client runs, the first time
// after dhcp6RSDelay.
func (d *DHCPMode) solicitRA(idx int) {
	d.mu.Lock()
	done := d.watchDone
	d.mu.Unlock()
	select {
	case <-done:
		return
	case <-time.After(dhcp6RSDelay()):
	}
	for range 3 {
		d.mu.Lock()
		n, nc, done := d.v6[idx], d.clients[idx], d.watchDone
		if d.stopped || n == nil || n.client != nil || nc == nil || nc.down {
			d.mu.Unlock()
			return
		}
		link := n.link
		d.mu.Unlock()
		if err := dhcp6SendRS(link.Attrs().Name, link.Attrs().HardwareAddr); err != nil {
			d.log.Debug("router solicitation", "interface", link.Attrs().Name, "error", err)
		}
		select {
		case <-done:
			return
		case <-time.After(dhcp6RSWait):
		}
	}
}

// stopAll6 stops DHCPv6 on every NIC, at once.
func (d *DHCPMode) stopAll6() {
	d.mu.Lock()
	idxs := slices.Collect(maps.Keys(d.v6))
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, idx := range idxs {
		wg.Go(func() { d.stop6(idx, true) })
	}
	wg.Wait()
}

// dns6 returns the DHCPv6 DNS servers by NIC. The caller holds d.mu.
func (d *DHCPMode) dns6() []string {
	var out []string
	for _, idx := range slices.Sorted(maps.Keys(d.v6)) {
		out = append(out, d.v6[idx].dns...)
	}
	return out
}
