//go:build linux

package timesync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// NewSyncer returns a Syncer for the real system clock and RTC.
func NewSyncer(skipTLSVerify bool) Syncer {
	return Syncer{
		Now:      time.Now,
		Floor:    BuildFloor(),
		NTP:      NTPClient{}.Query,
		Date:     DateProbe{SkipTLSVerify: skipTLSVerify}.Probe,
		SetClock: setSystemClock,
		WriteRTC: writeRTC,
		Log:      slog.Default().With("component", "timesync"),
	}
}

// rtcDevices are tried in order by writeRTC.
var rtcDevices = []string{"/dev/rtc0", "/dev/rtc"}

// setSystemClock steps CLOCK_REALTIME to t. Go timers and deadlines use the
// monotonic clock, so a step does not fire or delay them.
func setSystemClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("clock_settime: %w", err)
	}
	return nil
}

// writeRTC sets the hardware clock to t in UTC, the RTC convention of Linux
// distributions (timedatectl local-rtc no).
func writeRTC(t time.Time) error {
	var errs []error
	for _, dev := range rtcDevices {
		err := writeRTCDevice(dev, t)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func writeRTCDevice(dev string, t time.Time) error {
	f, err := os.Open(dev) // hwclock also sets the time on a read-only descriptor
	if err != nil {
		return fmt.Errorf("open %s: %w", dev, err)
	}
	defer func() { _ = f.Close() }()
	u := t.UTC()
	rtc := unix.RTCTime{
		Sec:  int32(u.Second()),
		Min:  int32(u.Minute()),
		Hour: int32(u.Hour()),
		Mday: int32(u.Day()),
		Mon:  int32(u.Month()) - 1,
		Year: int32(u.Year()) - 1900,
	}
	if err := unix.IoctlSetRTCTime(int(f.Fd()), &rtc); err != nil {
		return fmt.Errorf("RTC_SET_TIME %s: %w", dev, err)
	}
	return nil
}
