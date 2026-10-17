package caprf

import (
	"strings"
	"testing"
)

func TestParseVarsTimeOptions(t *testing.T) {
	cfg, err := ParseVars(strings.NewReader("TIME_SYNC=Require\nNTP_SERVERS=\"ntp1.example.com, 192.0.2.1:123 2001:db8::1\"\nTIME_MAX_SKEW_SEC=60\nTIME_WRITE_RTC=no\n"))
	if err != nil {
		t.Fatal(err)
	}
	tc := cfg.Time
	if tc.Sync != "require" || strings.Join(tc.NTPServers, " ") != "ntp1.example.com 192.0.2.1:123 2001:db8::1" ||
		tc.MaxSkewSeconds != 60 || tc.WriteRTC == nil || *tc.WriteRTC {
		t.Fatalf("time = %+v", tc)
	}

	cfg, err = ParseVars(strings.NewReader("TIME_WRITE_RTC=\n"))
	if err != nil || cfg.Time.WriteRTC != nil || cfg.Time.SyncMode() != "auto" {
		t.Fatalf("empty TIME_WRITE_RTC: %+v, %v", cfg.Time, err)
	}

	for _, bad := range []string{
		"TIME_SYNC=sometimes",
		"TIME_WRITE_RTC=maybe",
		"TIME_MAX_SKEW_SEC=x",
		"TIME_MAX_SKEW_SEC=-5",
		"TIME_MAX_SKEW_SEC=100000",
		"NTP_SERVERS=ntp://ntp.example.com",
		"NTP_SERVERS=ntp.example.com:99999",
		"NTP_SERVERS=a,b,c,d,e,f,g,h,i",
	} {
		if _, err := ParseVars(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}
