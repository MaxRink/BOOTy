//go:build linux

package ipmi

import (
	"context"
	"fmt"
	"time"
)

// BMC watchdog commands (IPMI v2.0 section 27, NetFn App).
const (
	netFnApp            = "0x06"
	cmdResetWatchdog    = "0x22"
	cmdSetWatchdog      = "0x24"
	watchdogUseSMSOS    = 0x04 // timer use SMS/OS, the timer stops on Set
	watchdogActionNone  = 0x00
	watchdogActionReset = 0x01 // hard reset at expiry
	watchdogClearSMSOS  = 0x10 // clear the SMS/OS expiration flag
	// MaxWatchdogTimeout is the largest BMC countdown (0xffff * 100 ms).
	MaxWatchdogTimeout = 0xffff * 100 * time.Millisecond
)

// SetWatchdog programs the BMC watchdog for a hard reset after timeout and
// starts it. The BMC resets the host even when the kernel hangs.
func (m *Manager) SetWatchdog(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 || timeout > MaxWatchdogTimeout {
		return fmt.Errorf("BMC watchdog timeout %s out of range (0, %s]", timeout, MaxWatchdogTimeout)
	}
	if err := m.setWatchdog(ctx, watchdogActionReset, timeout); err != nil {
		return err
	}
	return m.ResetWatchdog(ctx)
}

// ResetWatchdog restarts the BMC watchdog countdown (pets it).
func (m *Manager) ResetWatchdog(ctx context.Context) error {
	return m.run(ctx, "raw", netFnApp, cmdResetWatchdog)
}

// StopWatchdog stops the BMC watchdog: a Set without the "don't stop" bit
// stops the timer, and no action is armed.
func (m *Manager) StopWatchdog(ctx context.Context) error {
	return m.setWatchdog(ctx, watchdogActionNone, MaxWatchdogTimeout)
}

func (m *Manager) setWatchdog(ctx context.Context, action byte, timeout time.Duration) error {
	count := uint16(timeout / (100 * time.Millisecond))
	return m.run(ctx, "raw", netFnApp, cmdSetWatchdog,
		hexByte(watchdogUseSMSOS), hexByte(action), hexByte(0), hexByte(watchdogClearSMSOS),
		hexByte(byte(count)), hexByte(byte(count>>8)))
}

func hexByte(b byte) string { return fmt.Sprintf("0x%02x", b) }
