package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/payload"
)

// The split script, the Dockerfile and pkg/payload name the same payloads,
// and every payload file of the script is a file of the default tree.
func TestSplitBuildPayloadsMatchTheDefaultTree(t *testing.T) {
	script, err := os.ReadFile("hack/split-initramfs.sh")
	if err != nil {
		t.Fatal(err)
	}
	want := payload.FRR
	if !strings.Contains(string(script), `payloads="`+want+`"`) {
		t.Fatalf("hack/split-initramfs.sh does not split %q", want)
	}
	dockerfile, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	defaultTree := dockerfileStageBlock(t, string(dockerfile), "busybox")
	files := regexp.MustCompile(`(?m)^\t(?:frr)\) echo (.+) ;;$`).FindAllStringSubmatch(string(script), -1)
	if len(files) != 1 {
		t.Fatalf("found %d payload file lists in the script, want 1", len(files))
	}
	for _, m := range files {
		for f := range strings.FieldsSeq(m[1]) {
			if !strings.Contains(defaultTree, " "+f+"\n") {
				t.Errorf("payload file %s is not copied into the default tree", f)
			}
		}
	}
	split := dockerfileStageBlock(t, string(dockerfile), "split-builder")
	for _, want := range []string{
		`RUN split-initramfs.sh /build/initramfs /split "$BOOTY_BUILD"`,
		"COPY --from=kernel /vmlinuz /split/vmlinuz",
		"sha256sum base.cpio.zst frr.cpio.zst vmlinuz > SHA256SUMS",
	} {
		if !strings.Contains(split, want) {
			t.Errorf("split-builder lacks %q:\n%s", want, split)
		}
	}
}

// PR artifacts and releases publish the split archives.
func TestWorkflowsPublishSplitRamdisk(t *testing.T) {
	pr := loadWorkflow(t, ".github/workflows/pr-artifacts.yml")
	job := pr.Jobs["publish-pr-iso"]
	build := requireWorkflowStepRunContains(t, job, "Build ISO artifacts", "--target split")
	if !strings.Contains(build.Run, "sha256sum -c --quiet SHA256SUMS") {
		t.Error("PR artifacts do not check the SHA256SUMS of the split archives")
	}
	upload := requireWorkflowStep(t, job, "Publish split ramdisk artifact")
	assertWorkflowWith(t, upload, "name", "booty-pr-split")
	for _, f := range []string{"base.cpio.zst", "frr.cpio.zst", "vmlinuz", "SHA256SUMS"} {
		assertStepWithContains(t, upload, "path", "out-split/"+f)
	}

	release, err := os.ReadFile(".github/workflows/release-v2.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"flavor: split",
		"for part in base frr; do",
		"split-${{ matrix.arch }}-*.cpio.zst",
		"split-${{ matrix.arch }}-vmlinuz",
		"split-${{ matrix.arch }}-SHA256SUMS",
		`-o -name "split-*"`,
	} {
		if !strings.Contains(string(release), want) {
			t.Errorf("release workflow lacks %q", want)
		}
	}
}
