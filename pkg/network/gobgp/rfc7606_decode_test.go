//go:build linux

package gobgp

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// evpnUpdate returns a BGP UPDATE with one MP_REACH_NLRI attribute for
// L2VPN EVPN that carries one EVPN NLRI of the given route type, with the
// length field and data of the NLRI as given.
func evpnUpdate(routeType byte, nlri []byte) []byte {
	mp := make([]byte, 0, 11+len(nlri))
	mp = append(mp, 0, 25, 70, 4, 192, 0, 2, 1, 0, routeType, byte(len(nlri))) // AFI, SAFI, next hop, reserved
	mp = append(mp, nlri...)
	attrs := make([]byte, 0, 11+len(mp))
	attrs = append(attrs, 0x40, 1, 1, 0)                              // ORIGIN IGP
	attrs = append(attrs, 0x40, 2, 0)                                 // AS_PATH, empty
	attrs = append(attrs, 0x90, 14, 0, 0)                             // MP_REACH_NLRI, extended length
	binary.BigEndian.PutUint16(attrs[len(attrs)-2:], uint16(len(mp))) //nolint:gosec // test sizes
	attrs = append(attrs, mp...)
	msg := make([]byte, 23, 23+len(attrs)) // header, withdrawn routes length, path attribute length
	for i := range 16 {
		msg[i] = 0xff
	}
	binary.BigEndian.PutUint16(msg[16:], uint16(23+len(attrs))) //nolint:gosec // test sizes
	msg[18] = bgp.BGP_MSG_UPDATE
	binary.BigEndian.PutUint16(msg[21:], uint16(len(attrs))) //nolint:gosec // test sizes
	return append(msg, attrs...)
}

// evpnMinLen is the shortest encoding of each EVPN route type with an IPv4
// address: Ethernet A-D 25, MAC/IP 33, IMET 17, ES 23 (RFC 7432 section
// 7), IP Prefix 34 (RFC 9136 section 3.1).
var evpnMinLen = [...]int{1: 25, 2: 33, 3: 17, 4: 23, 5: 34}

// TestEVPNDecodeTruncatedNLRIReturnsError is the RFC 7606 evidence of the
// RFC compliance audit (finding RFC7606-EVPN-PANIC): a truncated EVPN NLRI
// from a peer must end in an error (RFC 7606 section 5.3: session reset or
// AFI/SAFI disable), not in a panic of the GoBGP receive goroutine, which
// would end BOOTy, the PID 1 of the ramdisk. GoBGP 3.37.0 panics for
// these inputs (slice bounds in the route type decoders); 4.10.0 checks
// the lengths (upstream commit ac5a8894).
func TestEVPNDecodeTruncatedNLRIReturnsError(t *testing.T) {
	for routeType := byte(1); routeType <= 5; routeType++ {
		for n := range evpnMinLen[routeType] {
			t.Run(fmt.Sprintf("type%d/len%d", routeType, n), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("decode panicked: %v", r)
					}
				}()
				if _, err := bgp.ParseBGPMessage(evpnUpdate(routeType, make([]byte, n))); err == nil {
					t.Fatal("decoded a truncated EVPN NLRI without an error")
				}
			})
		}
	}
}

// The UPDATE of evpnUpdate itself is valid: with the shortest encoding of
// each route type it decodes, so the errors above come from the NLRI.
func TestEVPNUpdateValidAtMinLen(t *testing.T) {
	for routeType := byte(1); routeType <= 5; routeType++ {
		nlri := make([]byte, evpnMinLen[routeType])
		switch routeType {
		case 3:
			nlri[12] = 32 // IPv4 originating router
		case 4:
			nlri[18] = 32 // IPv4 originating router
		}
		if _, err := bgp.ParseBGPMessage(evpnUpdate(routeType, nlri)); err != nil {
			t.Errorf("type %d, %d bytes: %v", routeType, len(nlri), err)
		}
	}
}
