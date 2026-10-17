// Package timesync sets the ramdisk clock before the first TLS certificate
// check. It queries SNTP servers (RFC 4330) and the HTTP Date header of the
// CAPRF server, steps the system clock and writes the RTC.
package timesync

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// DefaultNTPPort is the UDP port that NTP_SERVERS entries without a port use.
const DefaultNTPPort = "123"

const (
	ntpPacketSize = 48
	// ntpEpochOffset is the number of seconds from 1900-01-01 (NTP era 0) to
	// 1970-01-01 (Unix epoch).
	ntpEpochOffset = 2208988800
	// ntpEraSeconds is the length of one NTP era (2^32 seconds).
	ntpEraSeconds = 1 << 32
	// ntpEraPivot splits era 0 and era 1: timestamps with the top bit clear
	// are after 2036-02-07, like RFC 4330 section 3 recommends.
	ntpEraPivot = 1 << 31

	ntpModeClient = 3
	ntpModeServer = 4
	ntpVersion    = 4
	// ntpLeapAlarm is the leap indicator of an unsynchronized server.
	ntpLeapAlarm = 3
	// ntpMaxStratum is the highest stratum of a synchronized server.
	ntpMaxStratum = 15
)

// Sample is one time measurement: the offset to add to the local clock.
type Sample struct {
	// Source names the server, for logs ("ntp://192.0.2.1:123", "caprf").
	Source string
	// Offset is the source time minus the local clock.
	Offset time.Duration
	// RTT is the round trip time of the measurement.
	RTT time.Duration
	// Authenticated is true when a verified TLS chain backs the time.
	Authenticated bool
	// Resolution is the precision of the source. BOOTy steps the clock only
	// when the offset is larger.
	Resolution time.Duration
}

// NTPClient queries one SNTP server.
type NTPClient struct {
	// Now returns the local wall clock. Tests fake it to simulate skew.
	Now func() time.Time
	// Timeout bounds one query.
	Timeout time.Duration
}

// Query sends one SNTP request to server (host or host:port) and returns the
// clock offset. The reply must answer this request (origin timestamp), come
// from a synchronized server (leap indicator, stratum) and carry a transmit
// time.
func (c NTPClient) Query(ctx context.Context, server string) (Sample, error) {
	addr := ntpAddress(server)
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return Sample{}, fmt.Errorf("ntp %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return Sample{}, fmt.Errorf("ntp %s: set deadline: %w", addr, err)
		}
	}

	req, nonce, err := ntpRequest()
	if err != nil {
		return Sample{}, err
	}
	now := c.now()
	t1 := now()
	start := time.Now()
	if _, err := conn.Write(req); err != nil {
		return Sample{}, fmt.Errorf("ntp %s: send: %w", addr, err)
	}
	resp := make([]byte, ntpPacketSize+64)
	n, err := conn.Read(resp)
	rtt := time.Since(start)
	if err != nil {
		return Sample{}, fmt.Errorf("ntp %s: receive: %w", addr, err)
	}
	t4 := t1.Add(rtt)
	offset, err := parseNTPResponse(resp[:n], nonce, t1, t4)
	if err != nil {
		return Sample{}, fmt.Errorf("ntp %s: %w", addr, err)
	}
	return Sample{
		Source:     "ntp://" + addr,
		Offset:     offset,
		RTT:        rtt,
		Resolution: 500 * time.Millisecond,
	}, nil
}

func (c NTPClient) now() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

// ntpAddress adds the default port to a server without one. IPv6 literals
// need brackets when they carry a port, like everywhere in Go.
func ntpAddress(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(server, DefaultNTPPort)
}

// ntpRequest builds a client request. The transmit timestamp is random, so
// an off-path attacker can not guess the origin timestamp of the reply, and
// it does not reveal the local clock (RFC 9109 section 3).
func ntpRequest() (req, nonce []byte, err error) {
	req = make([]byte, ntpPacketSize)
	req[0] = ntpVersion<<3 | ntpModeClient
	if _, err := rand.Read(req[40:48]); err != nil {
		return nil, nil, fmt.Errorf("ntp: random transmit timestamp: %w", err)
	}
	return req, req[40:48], nil
}

var (
	errNTPShort    = errors.New("short reply")
	errNTPOrigin   = errors.New("reply does not answer the request (origin timestamp)")
	errNTPMode     = errors.New("reply is not a server reply")
	errNTPUnsynced = errors.New("server is not synchronized")
	errNTPZero     = errors.New("reply has no transmit timestamp")
)

// parseNTPResponse checks a reply and returns the clock offset
// ((T2-T1)+(T3-T4))/2 of RFC 4330 section 5.
func parseNTPResponse(resp, nonce []byte, t1, t4 time.Time) (time.Duration, error) {
	if len(resp) < ntpPacketSize {
		return 0, errNTPShort
	}
	leap := resp[0] >> 6
	version := resp[0] >> 3 & 0x7
	mode := resp[0] & 0x7
	stratum := resp[1]
	switch {
	case mode != ntpModeServer || version < 3 || version > 4:
		return 0, errNTPMode
	case !bytes.Equal(resp[24:32], nonce):
		return 0, errNTPOrigin
	case stratum == 0:
		code := string(resp[12:16])
		return 0, fmt.Errorf("server sent kiss-o'-death %q", code)
	case leap == ntpLeapAlarm || stratum > ntpMaxStratum:
		return 0, errNTPUnsynced
	}
	t2 := ntpTime(resp[32:40])
	t3 := ntpTime(resp[40:48])
	if binary.BigEndian.Uint64(resp[40:48]) == 0 {
		return 0, errNTPZero
	}
	return (t2.Sub(t1) + t3.Sub(t4)) / 2, nil
}

// ntpTime converts a 64-bit NTP timestamp to a time. Timestamps with the top
// bit clear belong to era 1 (from 2036-02-07).
func ntpTime(b []byte) time.Time {
	secs := int64(binary.BigEndian.Uint32(b[0:4]))
	frac := int64(binary.BigEndian.Uint32(b[4:8]))
	if secs < ntpEraPivot {
		secs += ntpEraSeconds
	}
	nanos := frac * int64(time.Second) >> 32
	return time.Unix(secs-ntpEpochOffset, nanos).UTC()
}
