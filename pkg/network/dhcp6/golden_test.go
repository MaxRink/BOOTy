package dhcp6

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
)

var update = flag.Bool("update", false, "rewrite testdata/golden.txt")

// TestGolden pins the bytes of every message the client sends (the
// transaction ID zeroed), so a change of the encoder cannot change the wire.
func TestGolden(t *testing.T) {
	var got []string
	add := func(name string, msg []byte) {
		if msg == nil {
			t.Fatalf("no %s", name)
		}
		b := append([]byte(nil), msg...)
		b[1], b[2], b[3] = 0, 0, 0
		got = append(got, name+" "+hex.EncodeToString(b))
	}
	first := func(msgs [][]byte, typ dhcpv6.MessageType) []byte {
		for _, m := range msgs {
			if dhcpv6.MessageType(m[0]) == typ {
				return m
			}
		}
		return nil
	}

	// SOLICIT with elapsed times that round up, and over 655.35 s.
	for _, tt := range []struct {
		host    string
		elapsed time.Duration
	}{{"e2e-test-rfm-0", 0}, {"a.b..example.", 1235 * time.Millisecond}, {strings.Repeat("x", 64) + ".y", 700 * time.Second}, {"", time.Second}} {
		c := newFakeNet().client(true)
		c.Hostname = tt.host
		now := c.now()
		add(fmt.Sprintf("solicit/%q/%s", tt.host, tt.elapsed), c.solicitMsg(0, now.Add(-tt.elapsed)))
	}

	// SOLICIT, ADVERTISE, REQUEST, REPLY, RELEASE.
	f := newFakeNet()
	f.answer = func(msg []byte) []byte {
		switch dhcpv6.MessageType(msg[0]) {
		case msgSolicit:
			return serverReply(msg, msgAdvertise, false)
		case msgRequest, msgRelease:
			return serverReply(msg, msgReply, false)
		}
		return nil
	}
	bound := make(chan *Lease, 1)
	c := f.client(true)
	c.SendRelease = true
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	close(f.stopped)
	c.Stop()
	add("request", first(f.messages(), msgRequest))
	add("release", first(f.messages(), msgRelease))

	// RENEW and REBIND without answers.
	f = newFakeNet()
	f.answer = func(msg []byte) []byte {
		if dhcpv6.MessageType(msg[0]) == msgSolicit {
			return serverReply(msg, msgReply, true)
		}
		return nil
	}
	expired := make(chan *Lease, 1)
	c = f.client(true)
	c.OnBound = func(*Lease) {}
	c.OnExpire = func(l *Lease) { expired <- l }
	c.Start()
	f.tick(t)
	<-expired
	close(f.stopped)
	c.Stop()
	add("renew", first(f.messages(), msgRenew))
	add("rebind", first(f.messages(), msgRebind))

	// INFORMATION-REQUEST.
	f = newFakeNet()
	f.answer = func(msg []byte) []byte { return serverReply(msg, msgReply, false) }
	c = f.client(false)
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	close(f.stopped)
	c.Stop()
	add("information-request", first(f.messages(), msgInformationRequest))

	compareGolden(t, got)
}

func compareGolden(t *testing.T, got []string) {
	t.Helper()
	path := filepath.Join("testdata", "golden.txt")
	if *update {
		if err := os.WriteFile(path, []byte(strings.Join(got, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var want []string
	for s := bufio.NewScanner(file); s.Scan(); {
		want = append(want, s.Text())
	}
	if len(got) != len(want) {
		t.Fatalf("%d messages, golden has %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("message %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}
