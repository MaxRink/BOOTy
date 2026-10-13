package network

import (
	"reflect"
	"testing"
)

func TestLinkMatchMatches(t *testing.T) {
	links := []LinkCandidate{
		{Name: "eno1", MAC: "52:54:00:12:34:56"},
		{Name: "enp1s4", MAC: "52:54:00:aa:bb:cc"},
		{Name: "eth0", MAC: "52:54:00:00:00:01"},
	}
	tests := []struct {
		name  string
		match LinkMatch
		want  []string
	}{
		{name: "glob", match: LinkMatch{Name: "en*"}, want: []string{"eno1", "enp1s4"}},
		{name: "pci slot glob", match: LinkMatch{Name: "enp*s4"}, want: []string{"enp1s4"}},
		{name: "mac ignores case", match: LinkMatch{MAC: "52:54:00:AA:BB:CC"}, want: []string{"enp1s4"}},
		{name: "name and mac", match: LinkMatch{Name: "eth*", MAC: "52:54:00:12:34:56"}},
		{name: "bad glob", match: LinkMatch{Name: "en["}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.match.Matches(links); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}
