package dhcp4

import (
	"bufio"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden.txt")

// TestGolden pins the BOOTP bytes of every message the client sends, so a
// change of the encoder cannot change the wire.
func TestGolden(t *testing.T) {
	var got []string
	add := func(name string, m *message, secs uint16) {
		m.secs = secs
		got = append(got, name+" "+hex.EncodeToString(m.encode()))
	}
	const xid = 0x01020304
	c := newFakeNet().client()
	add("discover", c.discover(xid), 0)
	c.lastAddr = testAddr
	add("discover/last-address", c.discover(xid), 3)
	add("request/selecting", c.request(xid, nil, testServer, testAddr), 4)
	add("request/init-reboot", c.request(xid, nil, nil, testAddr), 0)
	add("request/renewing", c.request(xid, testAddr, nil, nil), 65535)

	bare := newFakeNet().client()
	bare.Hostname, bare.ClientID, bare.Params, bare.Iface.MTU = "", nil, nil, 500
	add("discover/bare", bare.discover(xid), 0)
	add("request/bare", bare.request(xid, testAddr, nil, nil), 0)

	big := newFakeNet().client()
	big.Iface.MTU = 9000
	big.Hostname = strings.Repeat("h", 63)
	big.ClientID = append([]byte{255}, make([]byte, 64)...)
	add("discover/big", big.discover(xid), 1)

	f := newFakeNet()
	c = f.client()
	c.lease = &Lease{FixedAddress: testAddr, ServerID: testServer}
	c.release()
	msgs := f.messages()
	if len(msgs) != 1 {
		t.Fatalf("release sent %d messages", len(msgs))
	}
	msgs[0].m.xid = xid
	add("release", msgs[0].m, 0)

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
