package timesync

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// putNTPTime writes t as a 64-bit NTP timestamp.
func putNTPTime(b []byte, t time.Time) {
	secs := t.Unix() + ntpEpochOffset
	frac := (int64(t.Nanosecond()) << 32) / int64(time.Second)
	binary.BigEndian.PutUint32(b[0:4], uint32(secs))
	binary.BigEndian.PutUint32(b[4:8], uint32(frac))
}

// fakeNTPServer answers SNTP requests on a local UDP port with the real
// time plus skew. mutate changes each reply before it is sent.
func fakeNTPServer(t *testing.T, skew time.Duration, mutate func([]byte)) string {
	t.Helper()
	conn, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < ntpPacketSize {
				continue
			}
			recv := time.Now().Add(skew)
			resp := make([]byte, ntpPacketSize)
			resp[0] = 0<<6 | 4<<3 | ntpModeServer
			resp[1] = 2 // stratum
			copy(resp[12:16], "GPS\x00")
			copy(resp[24:32], buf[40:48]) // origin = client transmit
			putNTPTime(resp[32:40], recv)
			putNTPTime(resp[40:48], time.Now().Add(skew))
			if mutate != nil {
				mutate(resp)
			}
			_, _ = conn.WriteTo(resp, addr)
		}
	}()
	return conn.LocalAddr().String()
}

func TestNTPQueryMeasuresSkewedClock(t *testing.T) {
	// The local clock is 25 years behind, like a reset CMOS.
	const skew = 25 * 365 * 24 * time.Hour
	server := fakeNTPServer(t, 0, nil)
	c := NTPClient{Now: func() time.Time { return time.Now().Add(-skew) }, Timeout: 2 * time.Second}
	sample, err := c.Query(context.Background(), server)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if diff := (sample.Offset - skew).Abs(); diff > 200*time.Millisecond {
		t.Fatalf("offset = %v, want %v (diff %v)", sample.Offset, skew, diff)
	}
	if sample.Source != "ntp://"+server || sample.Authenticated {
		t.Fatalf("sample = %+v", sample)
	}
}

func TestNTPQueryServerAhead(t *testing.T) {
	server := fakeNTPServer(t, 90*time.Second, nil)
	sample, err := NTPClient{}.Query(context.Background(), server)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if diff := (sample.Offset - 90*time.Second).Abs(); diff > 200*time.Millisecond {
		t.Fatalf("offset = %v, want 90s", sample.Offset)
	}
}

func TestNTPQueryRejectsBadReplies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte)
		want   string
	}{
		{"origin mismatch", func(b []byte) { b[24] ^= 0xff }, errNTPOrigin.Error()},
		{"client mode", func(b []byte) { b[0] = 4<<3 | ntpModeClient }, errNTPMode.Error()},
		{"version 2", func(b []byte) { b[0] = 2<<3 | ntpModeServer }, errNTPMode.Error()},
		{"unsynchronized", func(b []byte) { b[0] |= ntpLeapAlarm << 6 }, errNTPUnsynced.Error()},
		{"stratum 16", func(b []byte) { b[1] = 16 }, errNTPUnsynced.Error()},
		{"kiss of death", func(b []byte) { b[1] = 0; copy(b[12:16], "RATE") }, `kiss-o'-death "RATE"`},
		{"zero transmit", func(b []byte) { clear(b[40:48]) }, errNTPZero.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := fakeNTPServer(t, 0, tt.mutate)
			_, err := NTPClient{}.Query(context.Background(), server)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Query error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNTPQueryTimesOut(t *testing.T) {
	conn, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_, err = NTPClient{Timeout: 100 * time.Millisecond}.Query(context.Background(), conn.LocalAddr().String())
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Query error = %v, want a timeout", err)
	}
}

func TestParseNTPResponseShort(t *testing.T) {
	if _, err := parseNTPResponse(make([]byte, 47), nil, time.Now(), time.Now()); !errors.Is(err, errNTPShort) {
		t.Fatalf("err = %v, want errNTPShort", err)
	}
}

func TestNTPTimeEras(t *testing.T) {
	for _, want := range []time.Time{
		time.Date(2026, 10, 17, 12, 0, 0, 500_000_000, time.UTC),
		time.Date(2036, 2, 7, 6, 28, 15, 0, time.UTC),
		time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC), // era 1
	} {
		b := make([]byte, 8)
		putNTPTime(b, want)
		if got := ntpTime(b); got.Sub(want).Abs() > time.Microsecond {
			t.Errorf("ntpTime(%v) = %v", want, got)
		}
	}
}

func TestNTPAddress(t *testing.T) {
	for in, want := range map[string]string{
		"ntp.example.com":      "ntp.example.com:123",
		"192.0.2.1":            "192.0.2.1:123",
		"192.0.2.1:1123":       "192.0.2.1:1123",
		"2001:db8::1":          "[2001:db8::1]:123",
		"[2001:db8::1]:10123":  "[2001:db8::1]:10123",
		"ntp.example.com:4123": "ntp.example.com:4123",
	} {
		if got := ntpAddress(in); got != want {
			t.Errorf("ntpAddress(%q) = %q, want %q", in, got, want)
		}
	}
}
