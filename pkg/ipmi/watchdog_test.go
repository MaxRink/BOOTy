//go:build linux

package ipmi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestBMCWatchdogCommands(t *testing.T) {
	var calls []string
	m := &Manager{send: func(_ context.Context, netFn, cmd byte, data []byte) ([]byte, error) {
		calls = append(calls, fmt.Sprintf("%#02x %#02x % x", netFn, cmd, data))
		return nil, nil
	}}
	ctx := context.Background()
	if err := m.SetWatchdog(ctx, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := m.ResetWatchdog(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.StopWatchdog(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		// 300 s = 3000 * 100 ms = 0x0bb8, LSB first.
		"0x06 0x24 04 01 00 10 b8 0b",
		"0x06 0x22 ",
		"0x06 0x22 ",
		"0x06 0x24 04 00 00 10 ff ff",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	for _, bad := range []time.Duration{0, MaxWatchdogTimeout + time.Second} {
		if err := m.SetWatchdog(ctx, bad); err == nil {
			t.Errorf("SetWatchdog(%s) succeeded", bad)
		}
	}
}

func TestBMCWatchdogError(t *testing.T) {
	m := &Manager{send: func(context.Context, byte, byte, []byte) ([]byte, error) {
		return nil, errors.New("IPMI completion code 0x80")
	}}
	if err := m.ResetWatchdog(context.Background()); err == nil || !strings.Contains(err.Error(), "0x80") {
		t.Fatalf("err = %v, want the completion code", err)
	}
}

func TestCompletion(t *testing.T) {
	if _, err := completion(nil); err == nil {
		t.Error("empty response accepted")
	}
	if _, err := completion([]byte{0xc1}); err == nil || !strings.Contains(err.Error(), "0xc1") {
		t.Errorf("err = %v, want completion code 0xc1", err)
	}
	data, err := completion([]byte{0, 1, 2})
	if err != nil || string(data) != "\x01\x02" {
		t.Errorf("completion = % x, %v", data, err)
	}
}

// The structs and request numbers must match <linux/ipmi.h> on the 64-bit
// targets of BOOTy (amd64, arm64).
func TestKernelABI(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("BOOTy targets 64-bit Linux")
	}
	if got := unsafe.Sizeof(req{}); got != 40 {
		t.Errorf("sizeof(struct ipmi_req) = %d, want 40", got)
	}
	if got := unsafe.Sizeof(recv{}); got != 48 {
		t.Errorf("sizeof(struct ipmi_recv) = %d, want 48", got)
	}
	if got := unsafe.Offsetof(req{}.msg); got != 24 {
		t.Errorf("offsetof(ipmi_req, msg) = %d, want 24", got)
	}
	if got := unsafe.Offsetof(recv{}.msg); got != 32 {
		t.Errorf("offsetof(ipmi_recv, msg) = %d, want 32", got)
	}
	if got := unsafe.Sizeof(systemInterfaceAddr{}); got != 8 {
		t.Errorf("sizeof(struct ipmi_system_interface_addr) = %d, want 8", got)
	}
	if ipmictlSendCommand != 0x8028690d || ipmictlReceiveMsgTrunc != 0xc030690b {
		t.Errorf("ioctls = %#x, %#x, want 0x8028690d, 0xc030690b", ipmictlSendCommand, ipmictlReceiveMsgTrunc)
	}
}

func TestSendDeviceMissing(t *testing.T) {
	_, err := sendDevice(context.Background(), t.TempDir()+"/ipmi0", netFnApp, cmdResetWatchdog, nil)
	if err == nil || !strings.Contains(err.Error(), "open IPMI device") {
		t.Fatalf("err = %v, want open error", err)
	}
}
