package caprf

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

type archiveEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func deployArchive(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tar.TypeReg, Linkname: e.link, Uid: 4242, Gid: 4242}
		if e.typ != 0 {
			hdr.Typeflag = e.typ
		}
		if e.mode != 0 {
			hdr.Mode = e.mode
		}
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const baseAgentVars = "TOKEN=agent-token\nMODE=standby\nINIT_URL=https://caprf/status/init\n" +
	"HEARTBEAT_URL=https://caprf/agent/heartbeat\nCOMMANDS_URL=https://caprf/agent/commands\n" +
	"FIRMWARE_URL=https://caprf/firmware\nHOSTNAME=old\n"

// applyArchive stages the archive like CommandConfig and swaps it into
// /deploy like the standby command does after its checks.
func applyArchive(c *Client, payload []byte) (*config.MachineConfig, []string, error) {
	cfg, keys, err := c.CommandConfig(payload)
	if err != nil {
		return nil, nil, err
	}
	_, err = c.ApplyCommandFiles()
	return cfg, keys, err
}

// validVars is a deploy/vars that passes, so in the unsafe-entry tests only
// the guard under test can fail the archive.
var validVars = archiveEntry{name: "deploy/vars", body: "HOSTNAME=worker-7\n"}

// useDeployRoot points deployRoot at a temp /deploy with files of a
// previous job and returns it.
func useDeployRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "deploy")
	prev := deployRoot
	t.Cleanup(func() { deployRoot = prev })
	deployRoot = root
	for _, f := range []string{"machine-files/old", "machine-commands/old.sh", "file-system/etc/old", "vars"} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The archive of the next job replaces the per-job files; the agent keeps
// its token, endpoints and bearer URLs.
func TestDeployArchiveConfigReplacesJobFiles(t *testing.T) {
	root := useDeployRoot(t)
	payload := deployArchive(t,
		archiveEntry{name: "deploy/", typ: tar.TypeDir},
		archiveEntry{name: "deploy/vars", body: "TOKEN=job-token\nMODE=provision\nFIRMWARE_URL=https://evil/fw\nHOSTNAME=worker-7\n"},
		archiveEntry{name: "deploy/machine-files/etc/cloud/nocloud/user-data", body: "#cloud-config\n", mode: 0o4666},
		archiveEntry{name: "deploy/machine-commands/10-run.sh", body: "#!/bin/sh\n", mode: 0o700},
	)

	c := &Client{vars: []byte(baseAgentVars)}
	cfg, keys, err := applyArchive(c, payload)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "worker-7" || cfg.Transport.Token != "agent-token" || cfg.Mode != "standby" ||
		cfg.Provision.Firmware.URL != "https://caprf/firmware" {
		t.Fatalf("hostname %q token %q mode %q firmware %q, want the archive hostname and the live agent settings",
			cfg.Hostname, cfg.Transport.Token, cfg.Mode, cfg.Provision.Firmware.URL)
	}
	if strings.Join(keys, ",") != "FIRMWARE_URL,HOSTNAME,MODE,TOKEN" {
		t.Fatalf("keys = %v", keys)
	}
	for file, mode := range map[string]os.FileMode{
		"machine-files/etc/cloud/nocloud/user-data": 0o666, // setuid dropped
		"machine-commands/10-run.sh":                0o700,
	} {
		fi, err := os.Lstat(filepath.Join(root, file))
		if err != nil || fi.Mode() != mode {
			t.Fatalf("%s: mode %v, %v; want %v", file, fi.Mode(), err, mode)
		}
	}
	for _, gone := range []string{"machine-files/old", "file-system"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s of the previous job still exists (%v)", gone, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "vars")); string(b) != "old" {
		t.Fatal("the archive changed /deploy/vars")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(root), ".booty-archive-*")); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
}

// Each archive has a valid deploy/vars: only the guard can reject it, and
// /deploy stays unchanged.
func TestDeployArchiveRejectsUnsafeEntries(t *testing.T) {
	defer func(b int64, n int) { maxDeployArchive, maxArchiveEntries = b, n }(maxDeployArchive, maxArchiveEntries)
	maxDeployArchive, maxArchiveEntries = 64, 5
	for name, entries := range map[string][]archiveEntry{
		// Proven tar-slip: a symlink, then a file of the same name.
		"symlink then file": {validVars, {name: "deploy/machine-files/x", typ: tar.TypeSymlink, link: "/etc/passwd"}, {name: "deploy/machine-files/x", body: "pwned"}},
		"symlink":           {validVars, {name: "deploy/machine-files/l", typ: tar.TypeSymlink, link: "../"}},
		"top-level symlink": {validVars, {name: "deploy/machine-files", typ: tar.TypeSymlink, link: "/etc"}},
		"vars symlink":      {{name: "deploy/vars", typ: tar.TypeSymlink, link: "/etc/shadow"}},
		"hard link":         {validVars, {name: "deploy/machine-files/h", typ: tar.TypeLink, link: "deploy/vars"}},
		"device":            {validVars, {name: "deploy/machine-files/null", typ: tar.TypeChar}},
		"duplicate file":    {validVars, {name: "deploy/machine-files/a", body: "1"}, {name: "deploy/machine-files/a", body: "2"}},
		"outside deploy":    {validVars, {name: "etc/passwd", body: "x"}},
		"dot dot":           {validVars, {name: "deploy/../etc/passwd", body: "x"}},
		"dot dot inside":    {validVars, {name: "deploy/machine-files/../../etc/passwd", body: "x"}},
		"absolute":          {validVars, {name: "/deploy/machine-files/a", body: "x"}},
		"empty name":        {validVars, {name: "", body: "x"}},
		"too large":         {validVars, {name: "deploy/machine-files/big", body: strings.Repeat("x", 65)}},
		"too many entries":  {validVars, {name: "deploy/a/1", body: ""}, {name: "deploy/a/2", body: ""}, {name: "deploy/a/3", body: ""}, {name: "deploy/a/4", body: ""}, {name: "deploy/a/5", body: ""}},
		"no vars":           {{name: "deploy/machine-files/a", body: "x"}},
		"bad vars line":     {{name: "deploy/vars", body: "HOSTNAME=x\nnot a line with secret-value\n"}},
		// Two entries with long paths would create many directories.
		"directory bomb":  {validVars, {name: "deploy/" + strings.Repeat("d/", 8) + "f", body: ""}},
		"path too long":   {validVars, {name: "deploy/" + strings.Repeat("x", 520), body: ""}},
		"dots in a name":  {validVars, {name: "deploy/machine-files/a..b", body: ""}},
		"path too deep":   {validVars, {name: "deploy/" + strings.Repeat("d/", 16) + "f", body: ""}},
		"job dir is file": {validVars, {name: "deploy/machine-files", body: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			root := useDeployRoot(t)
			c := &Client{vars: []byte(baseAgentVars)}
			_, _, err := c.CommandConfig(deployArchive(t, entries...))
			if err == nil {
				t.Fatal("unsafe archive applied")
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error leaks a value: %v", err)
			}
			if b, err := os.ReadFile(filepath.Join(root, "machine-files/old")); err != nil || string(b) != "old" {
				t.Fatalf("/deploy changed by a rejected archive: %q, %v", b, err)
			}
		})
	}
}

// At the limits the archive still applies (the guards are not off by one).
func TestDeployArchiveAtTheLimits(t *testing.T) {
	defer func(b int64, n int) { maxDeployArchive, maxArchiveEntries = b, n }(maxDeployArchive, maxArchiveEntries)
	// Three entries and the implied directory machine-files: four nodes.
	maxDeployArchive, maxArchiveEntries = 64, 4
	useDeployRoot(t)
	vars := "HOSTNAME=worker-7\n"
	payload := deployArchive(t,
		archiveEntry{name: "deploy/vars", body: vars},
		archiveEntry{name: "deploy/machine-files/a", body: strings.Repeat("x", 64-len(vars))},
		archiveEntry{name: "deploy/", typ: tar.TypeDir},
	)
	if _, _, err := (&Client{vars: []byte(baseAgentVars)}).CommandConfig(payload); err != nil {
		t.Fatalf("archive at the limits rejected: %v", err)
	}
	maxArchiveEntries = 3
	useDeployRoot(t)
	if _, _, err := (&Client{vars: []byte(baseAgentVars)}).CommandConfig(payload); err == nil {
		t.Fatal("the implied directory did not count")
	}
}

func TestIsDeployArchive(t *testing.T) {
	if !isDeployArchive([]byte{0x1f, 0x8b, 8}) || isDeployArchive([]byte("HOSTNAME=x\n")) {
		t.Fatal("isDeployArchive does not tell gzip from vars lines")
	}
}

// A failure in the second rename loop moves the old directories back.
func TestDeployArchiveSwapRollsBack(t *testing.T) {
	root := useDeployRoot(t)
	prev := renameFunc
	t.Cleanup(func() { renameFunc = prev })
	renameFunc = func(from, to string) error {
		if strings.Contains(from, ".booty-archive-") && strings.HasSuffix(from, filepath.Join("deploy", "machine-commands")) {
			return os.ErrPermission
		}
		return os.Rename(from, to)
	}
	payload := deployArchive(t, validVars,
		archiveEntry{name: "deploy/machine-files/new", body: "new"},
		archiveEntry{name: "deploy/machine-commands/new.sh", body: "new"})
	if _, _, err := applyArchive(&Client{vars: []byte(baseAgentVars)}, payload); err == nil {
		t.Fatal("swap error lost")
	}
	for _, f := range []string{"machine-files/old", "machine-commands/old.sh", "file-system/etc/old"} {
		if b, err := os.ReadFile(filepath.Join(root, f)); err != nil || string(b) != "old" {
			t.Fatalf("%s not restored: %q, %v", f, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "machine-files/new")); !os.IsNotExist(err) {
		t.Fatalf("new file stayed after the rollback (%v)", err)
	}
}

// The mode is set after the create, so the umask does not change it, and
// a private file stays private.
func TestDeployArchiveModeIgnoresUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	root := useDeployRoot(t)
	payload := deployArchive(t, validVars,
		archiveEntry{name: "deploy/machine-commands/run.sh", body: "#!/bin/sh\n", mode: 0o700},
		archiveEntry{name: "deploy/machine-files/data", body: "x", mode: 0o600},
		archiveEntry{name: "deploy/machine-files/shared", body: "x", mode: 0o2755})
	if _, _, err := applyArchive(&Client{vars: []byte(baseAgentVars)}, payload); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]os.FileMode{"machine-commands/run.sh": 0o700, "machine-files/data": 0o600, "machine-files/shared": 0o755} {
		if fi, err := os.Stat(filepath.Join(root, f)); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s mode = %v, %v; want %v", f, fi.Mode(), err, want)
		}
	}
}

// The path caps hold on their own: the entry limit is high, so only the
// length or depth check can reject these archives, and paths at the caps
// pass.
func TestDeployArchivePathCaps(t *testing.T) {
	defer func(n int) { maxArchiveEntries = n }(maxArchiveEntries)
	maxArchiveEntries = 1000
	atDepth := "deploy/" + strings.Repeat("d/", 15) + "f" // 16 levels below deploy/
	tooDeep := "deploy/" + strings.Repeat("d/", 16) + "f" // 17 levels
	// Components below the 255 byte name limit of the file system.
	long := "deploy/" + strings.Repeat(strings.Repeat("x", 99)+"/", 4)
	atLength := long + strings.Repeat("y", 512-len(long))
	tooLong := atLength + "x"
	for name, tc := range map[string]struct {
		path string
		ok   bool
	}{
		"at the depth":  {atDepth, true},
		"too deep":      {tooDeep, false},
		"at the length": {atLength, true},
		"too long":      {tooLong, false},
	} {
		t.Run(name, func(t *testing.T) {
			useDeployRoot(t)
			_, _, err := (&Client{vars: []byte(baseAgentVars)}).CommandConfig(
				deployArchive(t, validVars, archiveEntry{name: tc.path, body: "x"}))
			if (err == nil) != tc.ok {
				t.Fatalf("path of %d bytes: err = %v, want ok=%t", len(tc.path), err, tc.ok)
			}
		})
	}
}

// /deploy changes only with ApplyCommandFiles: a command rejected after
// CommandConfig leaves the files of the previous job, and the staging
// directory goes.
func TestDeployArchiveAppliesOnlyAfterTheChecks(t *testing.T) {
	root := useDeployRoot(t)
	c := &Client{vars: []byte(baseAgentVars)}
	if _, _, err := c.CommandConfig(deployArchive(t, validVars, archiveEntry{name: "deploy/machine-files/new", body: "new"})); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "machine-files/old")); err != nil || string(b) != "old" {
		t.Fatalf("/deploy changed before the apply: %q, %v", b, err)
	}
	staging := c.stagedDeploy
	c.DiscardCommandFiles()
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging directory left after the discard (%v)", err)
	}
	if applied, err := c.ApplyCommandFiles(); err != nil || applied {
		t.Fatalf("apply after the discard: %t, %v", applied, err)
	}
	if _, err := os.Stat(filepath.Join(root, "machine-files/new")); !os.IsNotExist(err) {
		t.Fatalf("a discarded archive was applied (%v)", err)
	}
}

// The archive of a command that did not run never applies to the next
// command: a vars payload gets no files.
func TestDeployArchiveNotAppliedToTheNextCommand(t *testing.T) {
	root := useDeployRoot(t)
	c := &Client{vars: []byte(baseAgentVars)}
	if _, _, err := c.CommandConfig(deployArchive(t, validVars, archiveEntry{name: "deploy/machine-files/new", body: "job A"})); err != nil {
		t.Fatal(err)
	}
	staging := c.stagedDeploy
	if _, _, err := c.CommandConfig([]byte("HOSTNAME=job-b\n")); err != nil {
		t.Fatal(err)
	}
	if applied, err := c.ApplyCommandFiles(); err != nil || applied {
		t.Fatalf("apply for the vars payload: %t, %v; want nothing", applied, err)
	}
	if _, err := os.Stat(filepath.Join(root, "machine-files/new")); !os.IsNotExist(err) {
		t.Fatalf("the files of job A reached job B (%v)", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging directory of job A left (%v)", err)
	}
}

// An archive whose vars have no known key is rejected: standby would run
// the command with the base config and the files of another job.
func TestDeployArchiveWithoutKnownVarsIsRejected(t *testing.T) {
	root := useDeployRoot(t)
	c := &Client{vars: []byte(baseAgentVars)}
	_, _, err := c.CommandConfig(deployArchive(t,
		archiveEntry{name: "deploy/vars", body: "# only a comment\nNOT_A_BOOTY_KEY=v\n"},
		archiveEntry{name: "deploy/machine-files/new", body: "x"}))
	if err == nil || !strings.Contains(err.Error(), "no known key") {
		t.Fatalf("err = %v, want no known key", err)
	}
	if c.stagedDeploy != "" {
		t.Fatal("rejected archive left staged")
	}
	if _, err := os.Stat(filepath.Join(root, "machine-files/new")); !os.IsNotExist(err) {
		t.Fatalf("rejected archive applied (%v)", err)
	}
}

// Unknown keys are ignored like in the vars of an ISO, and the legacy
// presets follow CAPRF_LEGACY_COMPAT of the running ramdisk, not of the
// archive.
func TestDeployArchiveVarsLikeTheISO(t *testing.T) {
	for _, tc := range []struct {
		name, base, archive string
	}{
		{"base legacy, archive not", baseAgentVars + "CAPRF_LEGACY_COMPAT=true\n", "CAPRF_LEGACY_COMPAT=false\n"},
		{"archive legacy, base not", baseAgentVars, "CAPRF_LEGACY_COMPAT=true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useDeployRoot(t)
			ref, err := ParseVars(strings.NewReader(tc.base))
			if err != nil {
				t.Fatal(err)
			}
			c := &Client{vars: []byte(tc.base)}
			cfg, keys, err := c.CommandConfig(deployArchive(t,
				archiveEntry{name: "deploy/vars", body: "HOSTNAME=x\nNOT_A_BOOTY_KEY=v\n" + tc.archive}))
			if err != nil {
				t.Fatalf("unknown key failed the archive: %v", err)
			}
			if slices.Contains(keys, "NOT_A_BOOTY_KEY") {
				t.Fatalf("keys = %v, want the unknown key left out", keys)
			}
			if cfg.LegacyCompat != ref.LegacyCompat || cfg.PowerOffOnSuccess() != ref.PowerOffOnSuccess() ||
				cfg.ProvisionerFilesInRamdisk() != ref.ProvisionerFilesInRamdisk() {
				t.Fatalf("legacy %v poweroff %t ramdisk files %t, want those of the base vars (%v %t %t)",
					cfg.LegacyCompat, cfg.PowerOffOnSuccess(), cfg.ProvisionerFilesInRamdisk(),
					ref.LegacyCompat, ref.PowerOffOnSuccess(), ref.ProvisionerFilesInRamdisk())
			}
			c.DiscardCommandFiles()
		})
	}
}
