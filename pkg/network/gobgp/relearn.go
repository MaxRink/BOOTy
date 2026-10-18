//go:build linux

package gobgp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	"github.com/telekom/BOOTy/pkg/network/bfd"
)

const (
	// relearnInterval is how often the unnumbered peers are checked.
	relearnInterval = time.Second
	// relearnGrace is how long an unnumbered peer with carrier may stay
	// down before BOOTy looks for a new link-local address of the
	// switch. GoBGP reconnects within 1 to 2 s on its own.
	relearnGrace = 3 * time.Second
)

// relearner finds unnumbered peers whose switch came back with another
// link-local address (a restarted switch or port gets a new MAC, and so
// a new EUI-64 address) and moves the GoBGP peer to it. FRR peers with
// "neighbor IFACE interface", which accepts any address on the interface
// and learns the current one from the RAs; a GoBGP peer has one fixed
// address and rejects the switch's connections from the new one ("Can't
// find configuration for a new passive connection").
type relearner struct {
	peers       func() map[string]string // interface -> peer address
	established func(ctx context.Context, addr string) bool
	carrier     func(iface string) bool
	// moved returns the new address of the switch on iface, or "" while
	// the old one still answers or no other one does.
	moved   func(ctx context.Context, iface, old string) string
	replace func(ctx context.Context, iface, old, addr string) error
	log     interface{ Warn(string, ...any) }

	down map[string]time.Time // interface -> first check without session
}

// check runs one round: a peer that is down for relearnGrace while its
// interface has carrier moves to the switch's new address.
func (r *relearner) check(ctx context.Context, now time.Time) {
	if r.down == nil {
		r.down = map[string]time.Time{}
	}
	for iface, addr := range r.peers() {
		if r.established(ctx, addr) || !r.carrier(iface) {
			delete(r.down, iface)
			continue
		}
		since, ok := r.down[iface]
		if !ok {
			r.down[iface] = now
			continue
		}
		if now.Sub(since) < relearnGrace {
			continue
		}
		next := r.moved(ctx, iface, addr)
		if next == "" {
			continue
		}
		if err := r.replace(ctx, iface, addr, next); err != nil {
			r.log.Warn("move unnumbered peer to the new link-local address", "interface", iface,
				"old", addr, "new", next, "error", err)
			continue
		}
		delete(r.down, iface)
	}
}

// startRelearn checks the unnumbered peers every relearnInterval until
// Teardown.
func (u *UnderlayTier) startRelearn() {
	if len(u.ifacePeerSnapshot()) == 0 {
		return
	}
	r := &relearner{
		peers:       u.ifacePeerSnapshot,
		established: u.peerEstablished,
		carrier:     hasCarrier,
		moved:       movedPeer,
		replace:     u.replaceInterfacePeer,
		log:         u.log,
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.stopRelearn = cancel
	u.relearnDone = make(chan struct{})
	go func() {
		defer close(u.relearnDone)
		t := time.NewTicker(relearnInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				r.check(ctx, now)
			}
		}
	}()
}

func (u *UnderlayTier) peerEstablished(ctx context.Context, addr string) bool {
	up := false
	_ = u.bgp.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
		up = p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_ESTABLISHED
	})
	return up
}

func hasCarrier(iface string) bool {
	l, err := netlink.LinkByName(iface)
	return err == nil && l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
}

// movedPeer returns another link-local neighbor on iface that answers on
// the BGP port, when the old address does not. A switch that connects
// from its new address leaves it in the neighbor table; the RA asks it to.
func movedPeer(ctx context.Context, iface, old string) string {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return ""
	}
	cands := slices.DeleteFunc(findLinkLocalNeighbors(ifi, iface), func(a string) bool { return a == old })
	if len(cands) == 0 {
		sendRouterAdvertisement(iface)
		return ""
	}
	return chooseMoved(ctx, old, cands)
}

// chooseMoved returns the first candidate that answers on the BGP port,
// or "" while the old address still does.
func chooseMoved(ctx context.Context, old string, cands []string) string {
	if firstReachableBGPPeer(ctx, []string{old}) != "" {
		return ""
	}
	return firstReachableBGPPeer(ctx, cands)
}

// replaceInterfacePeer moves the unnumbered peer on iface from old to
// addr: the route-map neighbor sets, the GoBGP peer with the same
// configuration, its BFD session and the address maps.
func (u *UnderlayTier) replaceInterfacePeer(ctx context.Context, iface, old, addr string) error {
	u.peerMu.RLock()
	conf := u.ifacePeerConfs[iface]
	shared := false
	for i, a := range u.ifacePeers {
		shared = shared || (i != iface && hostOf(a) == hostOf(old))
	}
	u.peerMu.RUnlock()
	if conf == nil {
		return fmt.Errorf("no peer configuration for %s", iface)
	}
	if err := u.moveNeighborSets(ctx, old, addr, !shared); err != nil {
		return err
	}
	peer, _ := proto.Clone(conf).(*apipb.Peer)
	peer.Conf.NeighborAddress = addr
	if peer.Transport != nil {
		peer.Transport.RemoteAddress = addr
	}
	if err := u.movePeer(ctx, old, peer); err != nil {
		return err
	}
	u.peerMu.Lock()
	u.ifacePeerConfs[iface] = peer
	u.ifacePeers[iface] = addr
	if !shared {
		delete(u.peerDevs, hostOf(old))
	}
	u.peerDevs[hostOf(addr)] = iface
	u.peerMu.Unlock()
	if u.bfd != nil {
		ip, err := netip.ParseAddr(addr)
		if err == nil {
			err = u.bfd.Replace(old, addr, ip)
		}
		if err != nil && !errors.Is(err, bfd.ErrNoSession) {
			u.log.Warn("BFD session not moved; the BGP session runs without it", "peer", addr, "error", err)
		}
	}
	u.log.Info("unnumbered peer moved to the new link-local address of the switch", "interface", iface, "old", old, "new", addr)
	return nil
}

// movePeer replaces the GoBGP peer at old with peer.
func (u *UnderlayTier) movePeer(ctx context.Context, old string, peer *apipb.Peer) error {
	u.peerOps.Lock()
	defer u.peerOps.Unlock()
	if err := u.bgp.DeletePeer(ctx, &apipb.DeletePeerRequest{Address: old}); err != nil {
		return fmt.Errorf("delete peer %s: %w", old, err)
	}
	delete(u.peerConfs, old)
	addr := peer.GetConf().GetNeighborAddress()
	if err := u.bgp.AddPeer(ctx, &apipb.AddPeerRequest{Peer: peer}); err != nil {
		return fmt.Errorf("add peer %s: %w", addr, err)
	}
	if u.peerConfs == nil {
		u.peerConfs = map[string]*apipb.Peer{}
	}
	u.peerConfs[addr] = peer
	return nil
}

// moveNeighborSets puts addr into every neighbor set that holds old, so
// the route-maps and checks of the peer stay; old leaves them unless
// another interface peer has the same address.
func (u *UnderlayTier) moveNeighborSets(ctx context.Context, old, addr string, drop bool) error {
	oldIP, err1 := netip.ParseAddr(old)
	newIP, err2 := netip.ParseAddr(addr)
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("peer address: %w", err)
	}
	from, to := hostPrefix(oldIP), hostPrefix(newIP)
	var names []string
	if err := u.bgp.ListDefinedSet(ctx, &apipb.ListDefinedSetRequest{DefinedType: apipb.DefinedType_DEFINED_TYPE_NEIGHBOR},
		func(d *apipb.DefinedSet) {
			if slices.Contains(d.GetList(), from) {
				names = append(names, d.GetName())
			}
		}); err != nil {
		return fmt.Errorf("list neighbor sets: %w", err)
	}
	for _, name := range names {
		set := &apipb.DefinedSet{DefinedType: apipb.DefinedType_DEFINED_TYPE_NEIGHBOR, Name: name, List: []string{to}}
		if err := u.bgp.AddDefinedSet(ctx, &apipb.AddDefinedSetRequest{DefinedSet: set}); err != nil {
			return fmt.Errorf("add %s to neighbor set %s: %w", to, name, err)
		}
		if !drop {
			continue
		}
		set = &apipb.DefinedSet{DefinedType: apipb.DefinedType_DEFINED_TYPE_NEIGHBOR, Name: name, List: []string{from}}
		if err := u.bgp.DeleteDefinedSet(ctx, &apipb.DeleteDefinedSetRequest{DefinedSet: set}); err != nil {
			return fmt.Errorf("remove %s from neighbor set %s: %w", from, name, err)
		}
	}
	return nil
}

// placeholderPeer is the address of an unnumbered peer whose switch did
// not answer yet. Its interface identifier is a reserved subnet anycast
// identifier (RFC 2526), never the address of a host, so it never
// answers and the relearner moves the peer to the switch's address.
func placeholderPeer(iface string) string {
	return "fe80::fdff:ffff:ffff:ff80%" + iface
}

func hostOf(addr string) string {
	host, _, _ := strings.Cut(addr, "%")
	return host
}

// ifacePeerSnapshot returns a copy of the unnumbered peers.
func (u *UnderlayTier) ifacePeerSnapshot() map[string]string {
	u.peerMu.RLock()
	defer u.peerMu.RUnlock()
	out := make(map[string]string, len(u.ifacePeers))
	for k, v := range u.ifacePeers {
		out[k] = v
	}
	return out
}

// ifacePeer returns the current address of the unnumbered peer on iface.
func (u *UnderlayTier) ifacePeer(iface string) string {
	u.peerMu.RLock()
	defer u.peerMu.RUnlock()
	return u.ifacePeers[iface]
}
