//go:build linux

// Package ipmi sends IPMI commands to the local BMC in process, through the
// Linux IPMI device interface (/dev/ipmi0 of ipmi_devintf), like
// `ipmitool -I open`.
package ipmi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DevicePath is the IPMI device of the first BMC.
const DevicePath = "/dev/ipmi0"

// Manager sends IPMI commands to the local BMC.
type Manager struct {
	send func(ctx context.Context, netFn, cmd byte, data []byte) ([]byte, error)
}

// New returns a Manager for the BMC at DevicePath.
func New() *Manager {
	return &Manager{send: func(ctx context.Context, netFn, cmd byte, data []byte) ([]byte, error) {
		return sendDevice(ctx, DevicePath, netFn, cmd, data)
	}}
}

// The kernel ABI of <linux/ipmi.h>, with the natural alignment of C.
const (
	systemInterfaceAddrType = 0x0c // IPMI_SYSTEM_INTERFACE_ADDR_TYPE
	bmcChannel              = 0x0f // IPMI_BMC_CHANNEL
	responseRecvType        = 1    // IPMI_RESPONSE_RECV_TYPE
	maxAddrSize             = 32   // IPMI_MAX_ADDR_SIZE
	maxMsgLength            = 272  // IPMI_MAX_MSG_LENGTH
	ipmiIOC                 = 'i'
	// defaultTimeout bounds a command without a context deadline.
	defaultTimeout = 5 * time.Second
)

type systemInterfaceAddr struct {
	addrType int32
	channel  int16
	lun      uint8
}

type msg struct {
	netFn   uint8
	cmd     uint8
	dataLen uint16
	data    unsafe.Pointer
}

type req struct {
	addr    unsafe.Pointer
	addrLen uint32
	msgID   int64
	msg     msg
}

type recv struct {
	recvType int32
	addr     unsafe.Pointer
	addrLen  uint32
	msgID    int64
	msg      msg
}

// ioctl request numbers: _IOR('i', 13, struct ipmi_req) and
// _IOWR('i', 11, struct ipmi_recv).
var (
	ipmictlSendCommand     = ioc(2, 13, unsafe.Sizeof(req{}))
	ipmictlReceiveMsgTrunc = ioc(3, 11, unsafe.Sizeof(recv{}))
)

func ioc(dir, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | ipmiIOC<<8 | nr
}

var msgIDs atomic.Int64

// sendDevice sends one command to the BMC and returns the response data
// after the completion code.
func sendDevice(ctx context.Context, path string, netFn, cmd byte, data []byte) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // fixed device path
	if err != nil {
		return nil, fmt.Errorf("open IPMI device: %w", err)
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd()) //nolint:gosec // a file descriptor fits in int

	addr := systemInterfaceAddr{addrType: systemInterfaceAddrType, channel: bmcChannel}
	id := msgIDs.Add(1)
	r := req{
		addr:    unsafe.Pointer(&addr), //nolint:gosec // struct ipmi_req of <linux/ipmi.h>
		addrLen: uint32(unsafe.Sizeof(addr)),
		msgID:   id,
		msg:     msg{netFn: netFn, cmd: cmd, dataLen: uint16(len(data))}, //nolint:gosec // IPMI requests are short
	}
	if len(data) > 0 {
		r.msg.data = unsafe.Pointer(&data[0]) //nolint:gosec // struct ipmi_msg of <linux/ipmi.h>
	}
	err = ioctl(fd, ipmictlSendCommand, unsafe.Pointer(&r)) //nolint:gosec // IPMICTL_SEND_COMMAND
	runtime.KeepAlive(&addr)
	runtime.KeepAlive(data)
	if err != nil {
		return nil, fmt.Errorf("IPMI send netfn %#02x cmd %#02x: %w", netFn, cmd, err)
	}
	return receive(ctx, fd, id)
}

// receive waits for the response to message id. Other messages, such as
// events, are dropped.
func receive(ctx context.Context, fd int, id int64) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTimeout)
	}
	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, fmt.Errorf("IPMI response: %w", context.DeadlineExceeded)
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // fd fits in int32
		n, err := unix.Poll(fds, int(wait.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("IPMI poll: %w", err)
		}
		if n == 0 {
			continue // the deadline check above ends the loop
		}
		var addr [maxAddrSize]byte
		buf := make([]byte, maxMsgLength)
		rv := recv{
			addr:    unsafe.Pointer(&addr[0]), //nolint:gosec // struct ipmi_recv of <linux/ipmi.h>
			addrLen: maxAddrSize,
			msg:     msg{dataLen: maxMsgLength, data: unsafe.Pointer(&buf[0])}, //nolint:gosec // struct ipmi_msg
		}
		err = ioctl(fd, ipmictlReceiveMsgTrunc, unsafe.Pointer(&rv)) //nolint:gosec // IPMICTL_RECEIVE_MSG_TRUNC
		runtime.KeepAlive(&addr)
		runtime.KeepAlive(buf)
		// EMSGSIZE: the message was truncated to buf; the completion code
		// is still the first byte.
		if err != nil && !errors.Is(err, unix.EMSGSIZE) {
			return nil, fmt.Errorf("IPMI receive: %w", err)
		}
		if rv.recvType != responseRecvType || rv.msgID != id {
			continue
		}
		return completion(buf[:min(int(rv.msg.dataLen), len(buf))])
	}
}

// completion checks the completion code, the first byte of a response.
func completion(resp []byte) ([]byte, error) {
	if len(resp) == 0 {
		return nil, fmt.Errorf("IPMI response without a completion code")
	}
	if resp[0] != 0 {
		return nil, fmt.Errorf("IPMI completion code %#02x", resp[0])
	}
	return resp[1:], nil
}

func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
