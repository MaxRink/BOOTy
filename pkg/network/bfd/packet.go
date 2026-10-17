// Package bfd implements the part of Bidirectional Forwarding Detection
// that BOOTy needs to replace FRR bfdd for its BGP sessions: asynchronous
// mode (RFC 5880) for single-hop sessions over IPv4 and IPv6 link-local
// (RFC 5881) and for multihop sessions (RFC 5883), with the Poll/Final
// sequence. Demand mode, echo mode and authentication are not implemented;
// BOOTy never asks for them and drops packets that use authentication.
package bfd

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// State is a BFD session state (RFC 5880 4.1).
type State uint8

// BFD session states.
const (
	StateAdminDown State = 0
	StateDown      State = 1
	StateInit      State = 2
	StateUp        State = 3
)

func (s State) String() string {
	switch s {
	case StateAdminDown:
		return "AdminDown"
	case StateDown:
		return "Down"
	case StateInit:
		return "Init"
	case StateUp:
		return "Up"
	}
	return fmt.Sprintf("State(%d)", uint8(s))
}

// Diag is a BFD diagnostic code (RFC 5880 4.1).
type Diag uint8

// Diagnostic codes BOOTy sets.
const (
	DiagNone             Diag = 0
	DiagTimeExpired      Diag = 1
	DiagNeighborSaysDown Diag = 3
	DiagAdminDown        Diag = 7
)

// packetLen is the length of a control packet without authentication.
const packetLen = 24

// Packet is a BFD control packet (RFC 5880 4.1). Intervals are in
// microseconds, as on the wire.
type Packet struct {
	Diag                Diag
	State               State
	Poll                bool
	Final               bool
	ControlPlaneIndep   bool
	AuthPresent         bool
	Demand              bool
	Multipoint          bool
	DetectMult          uint8
	MyDiscriminator     uint32
	YourDiscriminator   uint32
	DesiredMinTxUS      uint32
	RequiredMinRxUS     uint32
	RequiredMinEchoRxUS uint32
}

// Marshal encodes the packet. BOOTy never sends authentication.
func (p *Packet) Marshal() []byte {
	b := make([]byte, packetLen)
	b[0] = 1<<5 | byte(p.Diag)&0x1f
	b[1] = byte(p.State)<<6 | flag(p.Poll, 0x20) | flag(p.Final, 0x10) |
		flag(p.ControlPlaneIndep, 0x08) | flag(p.Demand, 0x02) | flag(p.Multipoint, 0x01)
	b[2] = p.DetectMult
	b[3] = packetLen
	binary.BigEndian.PutUint32(b[4:], p.MyDiscriminator)
	binary.BigEndian.PutUint32(b[8:], p.YourDiscriminator)
	binary.BigEndian.PutUint32(b[12:], p.DesiredMinTxUS)
	binary.BigEndian.PutUint32(b[16:], p.RequiredMinRxUS)
	binary.BigEndian.PutUint32(b[20:], p.RequiredMinEchoRxUS)
	return b
}

func flag(set bool, bit byte) byte {
	if set {
		return bit
	}
	return 0
}

// Errors of Parse. Each one means "discard the packet" (RFC 5880 6.8.6).
var (
	ErrShort         = errors.New("bfd: packet too short")
	ErrVersion       = errors.New("bfd: version is not 1")
	ErrLength        = errors.New("bfd: bad length field")
	ErrDetectMult    = errors.New("bfd: detect multiplier is zero")
	ErrMultipoint    = errors.New("bfd: multipoint bit set")
	ErrMyDiscr       = errors.New("bfd: my discriminator is zero")
	ErrYourDiscr     = errors.New("bfd: your discriminator is zero in state Init or Up")
	ErrAuthNotWanted = errors.New("bfd: authentication present but not configured")
)

// Parse decodes a control packet and applies the checks of RFC 5880 6.8.6
// that do not need session state.
func Parse(b []byte) (Packet, error) {
	if len(b) < packetLen {
		return Packet{}, ErrShort
	}
	if b[0]>>5 != 1 {
		return Packet{}, ErrVersion
	}
	p := Packet{
		Diag:                Diag(b[0] & 0x1f),
		State:               State(b[1] >> 6),
		Poll:                b[1]&0x20 != 0,
		Final:               b[1]&0x10 != 0,
		ControlPlaneIndep:   b[1]&0x08 != 0,
		AuthPresent:         b[1]&0x04 != 0,
		Demand:              b[1]&0x02 != 0,
		Multipoint:          b[1]&0x01 != 0,
		DetectMult:          b[2],
		MyDiscriminator:     binary.BigEndian.Uint32(b[4:]),
		YourDiscriminator:   binary.BigEndian.Uint32(b[8:]),
		DesiredMinTxUS:      binary.BigEndian.Uint32(b[12:]),
		RequiredMinRxUS:     binary.BigEndian.Uint32(b[16:]),
		RequiredMinEchoRxUS: binary.BigEndian.Uint32(b[20:]),
	}
	minLen := packetLen
	if p.AuthPresent {
		minLen = 26
	}
	if l := int(b[3]); l < minLen || l > len(b) {
		return Packet{}, ErrLength
	}
	switch {
	case p.DetectMult == 0:
		return Packet{}, ErrDetectMult
	case p.Multipoint:
		return Packet{}, ErrMultipoint
	case p.MyDiscriminator == 0:
		return Packet{}, ErrMyDiscr
	case p.YourDiscriminator == 0 && p.State != StateDown && p.State != StateAdminDown:
		return Packet{}, ErrYourDiscr
	case p.AuthPresent:
		return Packet{}, ErrAuthNotWanted
	}
	return p, nil
}
