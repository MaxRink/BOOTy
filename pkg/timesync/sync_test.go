package timesync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// fakeClock is a settable wall clock.
type fakeClock struct {
	now      time.Time
	setErr   error
	rtc      time.Time
	rtcErr   error
	setCalls int
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Set(t time.Time) error {
	c.setCalls++
	if c.setErr != nil {
		return c.setErr
	}
	c.now = t
	return nil
}

func (c *fakeClock) WriteRTC(t time.Time) error {
	if c.rtcErr != nil {
		return c.rtcErr
	}
	c.rtc = t
	return nil
}

var (
	realNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	// rtcNow is a reset CMOS clock.
	rtcNow = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
)

func testSyncer(clock *fakeClock, ntp, date func(context.Context, string) (Sample, error)) Syncer {
	return Syncer{
		Now:      clock.Now,
		Floor:    minPlausibleTime,
		NTP:      ntp,
		Date:     date,
		SetClock: clock.Set,
		WriteRTC: clock.WriteRTC,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func source(s Sample, err error) func(context.Context, string) (Sample, error) {
	return func(context.Context, string) (Sample, error) { return s, err }
}

func ntpSample(offset time.Duration) Sample {
	return Sample{Source: "ntp://192.0.2.1:123", Offset: offset, Resolution: 500 * time.Millisecond}
}

func dateSample(offset time.Duration, authenticated bool) Sample {
	return Sample{Source: "caprf-date", Offset: offset, Authenticated: authenticated, Resolution: 2 * time.Second}
}

var errDown = errors.New("down")

func TestSyncChoosesSource(t *testing.T) {
	skew := realNow.Sub(rtcNow)
	tests := []struct {
		name       string
		ntp, date  func(context.Context, string) (Sample, error)
		wantSource string
		wantNow    time.Time
	}{
		{"ntp within the date bound", source(ntpSample(skew+time.Second), nil), source(dateSample(skew, true), nil), "ntp://192.0.2.1:123", realNow.Add(time.Second)},
		{"ntp outside the date bound", source(ntpSample(skew+time.Hour), nil), source(dateSample(skew, true), nil), "caprf-date", realNow},
		{"unauthenticated date does not bound ntp", source(ntpSample(skew+time.Hour), nil), source(dateSample(skew, false), nil), "ntp://192.0.2.1:123", realNow.Add(time.Hour)},
		{"date only", source(Sample{}, errDown), source(dateSample(skew, true), nil), "caprf-date", realNow},
		{"ntp only", source(ntpSample(skew), nil), source(Sample{}, errDown), "ntp://192.0.2.1:123", realNow},
		{"ntp before the build floor", source(ntpSample(-time.Hour), nil), source(dateSample(skew, true), nil), "caprf-date", realNow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{now: rtcNow}
			res, err := testSyncer(clock, tt.ntp, tt.date).Sync(context.Background(), Options{
				NTPServers: []string{"192.0.2.1"}, DateURL: "https://caprf.example/status/init",
				MaxSkew: 5 * time.Minute, WriteRTC: true,
			})
			if err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if res.Source != tt.wantSource || !res.Stepped || !res.RTCWritten {
				t.Fatalf("result = %+v, want source %s, stepped, RTC written", res, tt.wantSource)
			}
			if !clock.now.Equal(tt.wantNow) || !clock.rtc.Equal(tt.wantNow) {
				t.Fatalf("clock = %v, rtc = %v, want %v", clock.now, clock.rtc, tt.wantNow)
			}
		})
	}
}

func TestSyncTriesNextNTPServer(t *testing.T) {
	clock := &fakeClock{now: rtcNow}
	var asked []string
	ntp := func(_ context.Context, server string) (Sample, error) {
		asked = append(asked, server)
		if server == "a" {
			return Sample{}, errDown
		}
		return ntpSample(realNow.Sub(rtcNow)), nil
	}
	res, err := testSyncer(clock, ntp, nil).Sync(context.Background(), Options{NTPServers: []string{"a", "b", "c"}})
	if err != nil || !res.Stepped {
		t.Fatalf("Sync = %+v, %v", res, err)
	}
	if len(asked) != 2 || clock.rtc != (time.Time{}) {
		t.Fatalf("asked %v, rtc %v: want a and b, no RTC write", asked, clock.rtc)
	}
}

func TestSyncKeepsClockWithinResolution(t *testing.T) {
	clock := &fakeClock{now: realNow}
	res, err := testSyncer(clock, source(ntpSample(100*time.Millisecond), nil), nil).Sync(context.Background(),
		Options{NTPServers: []string{"x"}, WriteRTC: true})
	if err != nil || res.Stepped || clock.setCalls != 0 || res.Offset != 100*time.Millisecond {
		t.Fatalf("Sync = %+v, %v, set calls %d", res, err, clock.setCalls)
	}
}

func TestSyncWithoutSource(t *testing.T) {
	for _, tt := range []struct {
		name    string
		now     time.Time
		require bool
		wantErr bool
	}{
		{"auto, plausible clock", realNow, false, false},
		{"auto, clock before the build", rtcNow, false, false},
		{"require, plausible clock", realNow, true, true},
		{"require, clock before the build", rtcNow, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{now: tt.now}
			res, err := testSyncer(clock, source(Sample{}, errDown), source(Sample{}, errDown)).Sync(context.Background(),
				Options{Require: tt.require, NTPServers: []string{"x"}, DateURL: "https://caprf.example"})
			if tt.wantErr != errors.Is(err, ErrNoTime) {
				t.Fatalf("err = %v, want ErrNoTime %v", err, tt.wantErr)
			}
			if res.Stepped || clock.setCalls != 0 {
				t.Fatalf("clock changed without a source: %+v", res)
			}
		})
	}
}

func TestSyncSetClockFailure(t *testing.T) {
	for _, require := range []bool{false, true} {
		clock := &fakeClock{now: rtcNow, setErr: errors.New("EPERM")}
		res, err := testSyncer(clock, source(ntpSample(realNow.Sub(rtcNow)), nil), nil).Sync(context.Background(),
			Options{Require: require, NTPServers: []string{"x"}, WriteRTC: true})
		if (err != nil) != require || res.Stepped || res.RTCWritten {
			t.Fatalf("require=%v: Sync = %+v, %v", require, res, err)
		}
	}
}

func TestSyncRTCFailureIsNotFatal(t *testing.T) {
	clock := &fakeClock{now: rtcNow, rtcErr: errors.New("no RTC")}
	res, err := testSyncer(clock, source(ntpSample(realNow.Sub(rtcNow)), nil), nil).Sync(context.Background(),
		Options{Require: true, NTPServers: []string{"x"}, WriteRTC: true})
	if err != nil || !res.Stepped || res.RTCWritten {
		t.Fatalf("Sync = %+v, %v", res, err)
	}
}

func TestSyncFakeNTPServerEndToEnd(t *testing.T) {
	server := fakeNTPServer(t, 0, nil)
	clock := &fakeClock{now: rtcNow}
	s := testSyncer(clock, nil, nil)
	// The fake clock is frozen in 2001; the real query compares the server
	// time against it.
	s.NTP = NTPClient{Now: clock.Now}.Query
	res, err := s.Sync(context.Background(), Options{Require: true, NTPServers: []string{server}})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if diff := time.Until(clock.now).Abs(); !res.Stepped || diff > time.Second {
		t.Fatalf("clock = %v after sync (diff %v), result %+v", clock.now, diff, res)
	}
}

func TestBuildFloorIsPlausible(t *testing.T) {
	if f := BuildFloor(); f.Before(minPlausibleTime) || f.After(time.Now()) {
		t.Fatalf("BuildFloor() = %v", f)
	}
}

func TestBuildFloorPrefersTheBuildStamp(t *testing.T) {
	orig := commitTime
	t.Cleanup(func() { commitTime = orig })

	stamp := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	commitTime = func() (time.Time, bool) { return stamp, true }
	if got := BuildFloor(); !got.Equal(stamp) || !got.After(minPlausibleTime) {
		t.Fatalf("BuildFloor() = %v, want the stamp %v", got, stamp)
	}
	commitTime = func() (time.Time, bool) { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), true }
	if got := BuildFloor(); !got.Equal(minPlausibleTime) {
		t.Fatalf("BuildFloor() with an old stamp = %v, want %v", got, minPlausibleTime)
	}
	commitTime = func() (time.Time, bool) { return time.Time{}, false }
	if got := BuildFloor(); got.Before(minPlausibleTime) {
		t.Fatalf("BuildFloor() without a stamp = %v", got)
	}
}

// Sources outside [floor, floor+5y] never set the clock: with nothing else
// left, require fails and auto keeps the RTC.
func TestSyncRejectsSourcesOutsideTheWindow(t *testing.T) {
	belowFloor := minPlausibleTime.Add(-time.Hour).Sub(rtcNow)
	aboveCeiling := minPlausibleTime.Add(maxAheadOfFloor + time.Hour).Sub(rtcNow)
	tests := []struct {
		name      string
		ntp, date func(context.Context, string) (Sample, error)
	}{
		{"ntp below the floor, date fails", source(ntpSample(belowFloor), nil), source(Sample{}, errDown)},
		{"ntp below the floor, unauthenticated date below the floor", source(ntpSample(belowFloor), nil), source(dateSample(belowFloor, false), nil)},
		{"date only, below the floor", source(Sample{}, errDown), source(dateSample(belowFloor, true), nil)},
		{"ntp more than five years ahead", source(ntpSample(aboveCeiling), nil), source(Sample{}, errDown)},
		{"date more than five years ahead", source(Sample{}, errDown), source(dateSample(aboveCeiling, true), nil)},
	}
	for _, tt := range tests {
		for _, require := range []bool{true, false} {
			clock := &fakeClock{now: rtcNow}
			res, err := testSyncer(clock, tt.ntp, tt.date).Sync(context.Background(), Options{
				Require: require, NTPServers: []string{"x"}, DateURL: "https://caprf.example", MaxSkew: 5 * time.Minute, WriteRTC: true,
			})
			if require != errors.Is(err, ErrNoTime) || (!require && err != nil) {
				t.Errorf("%s, require=%v: err = %v", tt.name, require, err)
			}
			if res.Stepped || clock.setCalls != 0 || !clock.now.Equal(rtcNow) || !clock.rtc.IsZero() {
				t.Errorf("%s, require=%v: the clock changed: %+v", tt.name, require, res)
			}
		}
	}
}

func TestSyncRefusesLargeDateBackwardStep(t *testing.T) {
	tests := []struct {
		name      string
		now       time.Time
		ntp, date func(context.Context, string) (Sample, error)
		wantStep  bool
	}{
		{"date sets a plausible clock back by an hour", realNow, source(Sample{}, errDown), source(dateSample(-time.Hour, true), nil), false},
		{"date wins over NTP and sets a plausible clock back", realNow, source(ntpSample(time.Hour), nil), source(dateSample(-time.Hour, true), nil), false},
		{"date sets the clock back within max skew", realNow, source(Sample{}, errDown), source(dateSample(-time.Minute, true), nil), true},
		{"NTP sets a plausible clock back", realNow, source(ntpSample(-time.Hour), nil), source(Sample{}, errDown), true},
		{"date sets a clock back that is far in the future", realNow.Add(10 * 365 * 24 * time.Hour), source(Sample{}, errDown),
			source(dateSample(-10*365*24*time.Hour, true), nil), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{now: tt.now}
			res, err := testSyncer(clock, tt.ntp, tt.date).Sync(context.Background(), Options{
				NTPServers: []string{"x"}, DateURL: "https://caprf.example", MaxSkew: 5 * time.Minute,
			})
			if err != nil || res.Stepped != tt.wantStep {
				t.Fatalf("Sync = %+v, %v; want stepped %v", res, err, tt.wantStep)
			}
		})
	}
	clock := &fakeClock{now: realNow}
	_, err := testSyncer(clock, source(Sample{}, errDown), source(dateSample(-time.Hour, true), nil)).Sync(context.Background(),
		Options{Require: true, DateURL: "https://caprf.example", MaxSkew: 5 * time.Minute})
	if !errors.Is(err, ErrNoTime) {
		t.Fatalf("require with a refused backward step: err = %v, want ErrNoTime", err)
	}
}
