package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestReviewerDocsPointAtProvisionStepCountTest(t *testing.T) {
	for _, path := range []string{
		"CONTRIBUTING.md",
		".github/AGENTS.md",
		".github/agents/provisioning-reviewer.agent.md",
		".github/copilot-instructions.md",
		".github/instructions/review.instructions.md",
		".github/prompts/proposal.prompt.md",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(data)
		if strings.Contains(text, "currently 42 steps") || strings.Contains(text, "42-step") {
			t.Fatalf("%s still contains stale step-count guidance", path)
		}
		if !strings.Contains(text, "TestProvisionStepCount") {
			t.Fatalf("%s must point reviewers at TestProvisionStepCount", path)
		}
	}
}

func TestREADMEClarifiesBondLabDoesNotProveLACP(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"not real LACP negotiation",
		"does not prove 802.3ad/LACP negotiation",
		"nightly smoke coverage for DHCP, static, multi-NIC, and bond topologies",
		"path-filtered `linux/arm64` flavor packaging",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("README missing %q", want)
		}
	}
}

// varsKeyPattern matches the vars keys that pkg/caprf/client.go accepts: map
// entries such as `"KEY": &cfg...` or `"KEY": "shim"`, single-key `case "KEY":`
// lines and `key == "KEY"` comparisons.
var varsKeyPattern = regexp.MustCompile(`(?m)^\s*(?:case "([A-Za-z0-9_]+)":|"([A-Za-z_][A-Za-z0-9_]*)":\s+(?:&cfg|")|.*key == "([A-Za-z0-9_]+)")`)

func TestConfigurationDocListsEveryVarsKey(t *testing.T) {
	src, err := os.ReadFile("pkg/caprf/client.go")
	if err != nil {
		t.Fatalf("read vars parser: %v", err)
	}
	doc, err := os.ReadFile("docs/configuration.md")
	if err != nil {
		t.Fatalf("read configuration reference: %v", err)
	}
	// Only table rows count, so a key named in prose or in the Planned list
	// does not satisfy the check.
	var rows strings.Builder
	for line := range strings.Lines(string(doc)) {
		if strings.HasPrefix(line, "| ") {
			rows.WriteString(line)
		}
	}
	keys := map[string]struct{}{}
	for _, m := range varsKeyPattern.FindAllStringSubmatch(string(src), -1) {
		keys[m[1]+m[2]+m[3]] = struct{}{}
	}
	// One key per parser handler, so a regexp that misses a handler fails.
	for _, sentinel := range []string{"IMAGE", "OCI_PREPULL_CACHE_DIR", "SECUREBOOT_SHIM_SHA256", "EXTRA_KERNEL_PARAMS", "RESCUE_TIMEOUT", "vrf_table_id", "asn_server"} {
		if _, ok := keys[sentinel]; !ok {
			t.Fatalf("varsKeyPattern does not find %q in pkg/caprf/client.go; update the pattern", sentinel)
		}
	}
	for key := range keys {
		if !strings.Contains(rows.String(), "`"+key+"`") {
			t.Errorf("docs/configuration.md has no table row for vars key %q", key)
		}
	}
}
