package kexec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyFixture copies testdata/<name> into a temp root and writes grubenv
// (when env is not nil) next to its grub.cfg.
func copyFixture(t *testing.T, name, grubDir string, env map[string]string) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join("testdata", name)
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if env != nil {
		var sb strings.Builder
		sb.WriteString("# GRUB Environment Block\n")
		for k, v := range env {
			sb.WriteString(k + "=" + v + "\n")
		}
		sb.WriteString(strings.Repeat("#", 900))
		if err := os.WriteFile(filepath.Join(root, grubDir, "grubenv"), []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDefaultEntryUbuntuCloudImage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        map[string]string
		wantTitle  string
		wantArgs   string
		wantKernel string
	}{
		{
			name:      "default 0",
			wantTitle: "Ubuntu", wantKernel: "/boot/vmlinuz-5.15.0-194-generic",
			wantArgs: "root=LABEL=cloudimg-rootfs ro console=tty1 console=ttyS0",
		},
		{
			// GRUB_DEFAULT=0 ignores saved_entry; only next_entry wins.
			name:      "saved_entry without GRUB_DEFAULT=saved",
			env:       map[string]string{"saved_entry": "1>1"},
			wantTitle: "Ubuntu", wantKernel: "/boot/vmlinuz-5.15.0-194-generic",
			wantArgs: "root=LABEL=cloudimg-rootfs ro console=tty1 console=ttyS0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyFixture(t, "ubuntu-2204-cloud", "boot/grub", tc.env)
			e, err := DefaultEntry(root)
			if err != nil {
				t.Fatal(err)
			}
			if e.Name != tc.wantTitle || e.Kernel != tc.wantKernel || e.KernelArgs != tc.wantArgs ||
				e.Initramfs != "/boot/initrd.img-5.15.0-194-generic" {
				t.Fatalf("entry = %+v", e)
			}
		})
	}
}

func TestDefaultEntryRefusesWhatOnlyGRUBCanResolve(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, grubDir string
		env                    map[string]string
		want                   string
	}{
		{name: "BLS", fixture: "centos-stream-9", grubDir: "boot/grub2", want: "BLS"},
		// linux$suf $gptprio_kernel: the kernel comes from gptprio.next.
		{name: "Flatcar", fixture: "flatcar", grubDir: "boot/grub", want: "command smbios"},
		// GRUB would boot next_entry once and clear it; BOOTy can not.
		{name: "next_entry", fixture: "ubuntu-2204-cloud", grubDir: "boot/grub", env: map[string]string{"next_entry": "1>1"}, want: "next_entry"},
		// The Ubuntu initrdless fallback and saved-entry logic.
		{name: "initrdfail", fixture: "ubuntu-2204-cloud", grubDir: "boot/grub", env: map[string]string{"initrdfail": "1"}, want: "initrdfail"},
		{name: "prev_entry", fixture: "ubuntu-2204-cloud", grubDir: "boot/grub", env: map[string]string{"prev_entry": "0"}, want: "prev_entry"},
		{name: "prev_saved_entry", fixture: "ubuntu-2204-cloud", grubDir: "boot/grub", env: map[string]string{"prev_saved_entry": "0"}, want: "prev_saved_entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DefaultEntry(copyFixture(t, tc.fixture, tc.grubDir, tc.env))
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrUnsupported with %q", err, tc.want)
			}
		})
	}
}

// TestDefaultEntryCustomCfg: the 41_custom source line of the Ubuntu
// fixture loads nothing without a custom.cfg, and anything with one.
func TestDefaultEntryCustomCfg(t *testing.T) {
	root := copyFixture(t, "ubuntu-2204-cloud", "boot/grub", nil)
	if _, err := DefaultEntry(root); err != nil {
		t.Fatalf("without custom.cfg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "boot/grub/custom.cfg"), []byte("set default=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DefaultEntry(root); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "custom.cfg") {
		t.Fatalf("with custom.cfg: err = %v, want ErrUnsupported", err)
	}
}

func writeGrub(t *testing.T, cfg, env string) string {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, "boot", "grub2")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "grub.cfg"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if env != "" {
		if err := os.WriteFile(filepath.Join(p, "grubenv"), []byte(env), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The generated GRUB_DEFAULT=saved header of Debian and Ubuntu.
const savedHeader = `load_env
if [ "${next_entry}" ] ; then
   set default="${next_entry}"
   set next_entry=
   save_env next_entry
   set boot_once=true
else
   set default="${saved_entry}"
fi
`

const generatedEntries = `set console_args="console=ttyS0,115200n8"
function gfxmode {
	set vt_handoff=vt.handoff=7
}
menuentry 'Ubuntu' $menuentry_id_option 'gnulinux-simple' {
	recordfail
	load_video
	gfxmode $linux_gfx_mode
	insmod gzio
	if [ x$grub_platform = xxen ]; then insmod xzio; insmod lzopio; fi
	set root='hd0,gpt2'
	if [ x$feature_platform_search_hint = xy ]; then
	  search --no-floppy --fs-uuid --set=root --hint-bios=hd0,gpt2 abc
	else
	  search --no-floppy --fs-uuid --set=root abc
	fi
	echo 'Loading Linux 6.8 ...'
	linux	/boot/vmlinuz-6.8 root=UUID=abc ro quiet splash $vt_handoff ${console_args} 'lit=$console_args' "quoted arg" esc\ aped
	initrd	/boot/initrd.img-6.8
}
submenu 'Advanced options' $menuentry_id_option 'gnulinux-advanced' {
	menuentry 'Ubuntu, with Linux 6.5' --id gnulinux-6.5 {
		linux	/boot/vmlinuz-6.5 root=UUID=abc ro
		initrd	/boot/initrd.img-6.5
	}
}
`

func TestDefaultEntryGeneratedShapes(t *testing.T) {
	cfg := savedHeader + generatedEntries
	for _, tc := range []struct{ env, kernel, args string }{
		{"saved_entry=gnulinux-simple\n", "/boot/vmlinuz-6.8",
			`root=UUID=abc ro quiet splash console=ttyS0,115200n8 lit=$console_args "quoted arg" "esc aped"`},
		{"saved_entry=gnulinux-advanced>gnulinux-6.5\n", "/boot/vmlinuz-6.5", "root=UUID=abc ro"},
		{"saved_entry=Advanced options>Ubuntu, with Linux 6.5\n", "/boot/vmlinuz-6.5", "root=UUID=abc ro"},
		{"saved_entry=1>0\n", "/boot/vmlinuz-6.5", "root=UUID=abc ro"},
		{"", "/boot/vmlinuz-6.8", ""}, // empty saved_entry is entry 0
	} {
		e, err := DefaultEntry(writeGrub(t, cfg, tc.env))
		if err != nil {
			t.Fatalf("%q: %v", tc.env, err)
		}
		if e.Kernel != tc.kernel || tc.args != "" && e.KernelArgs != tc.args {
			t.Fatalf("%q: entry = %+v, want %s %s", tc.env, e, tc.kernel, tc.args)
		}
	}
}

// Every shape outside the allow-list reboots.
func TestDefaultEntryRefusesDoubt(t *testing.T) {
	entry := func(body string) string {
		return "set default=0\nmenuentry 'x' {\n" + body + "\n linux /vmlinuz root=/dev/sda2\n}\n"
	}
	for name, tc := range map[string]struct{ cfg, env, want string }{
		"one-line conditional default":   {cfg: "if [ -s $prefix/grubenv ]; then set default=1; fi\n" + entry(""), want: "conditional default"},
		"multi-line conditional default": {cfg: "if [ x$feature = xy ]; then\n set default=1\nfi\n" + entry(""), want: "conditional default"},
		"elif default":                   {cfg: "if [ \"${next_entry}\" ] ; then\n set default=\"${next_entry}\"\nelif [ x = y ]; then\n set default=1\nfi\n" + entry(""), want: "conditional default"},
		"set in entry":                   {cfg: entry(" set kopts=quiet"), want: "command set"},
		"search other var":               {cfg: entry(" search --set=boot --fs-uuid abc"), want: "command search"},
		"search -s other var":            {cfg: entry(" search -s foo abc"), want: "command search"},
		"probe in entry":                 {cfg: entry(" probe --set=uuid --fs-uuid hd0,gpt2"), want: "command probe"},
		"regexp in entry":                {cfg: entry(" regexp --set=x a b"), want: "command regexp"},
		"source in entry":                {cfg: entry(" source /boot/extra.cfg"), want: "command source"},
		"configfile top level":           {cfg: "configfile /boot/other.cfg\n" + entry(""), want: "configfile"},
		"source top level":               {cfg: "source /boot/x.cfg\n" + entry(""), want: "source"},
		"linux inside if":                {cfg: "set default=0\nmenuentry 'x' {\n if [ x = y ]; then\n linux /a\n fi\n}\n", want: "inside a condition"},
		"one-line linux if":              {cfg: "set default=0\nmenuentry 'x' {\n if [ x = y ]; then linux /a; fi\n}\n", want: "inside a condition"},
		"two linux":                      {cfg: "set default=0\nmenuentry 'x' {\n linux /a\n linux /b\n}\n", want: "2 linux"},
		"two initrd":                     {cfg: entry(" initrd /a\n initrd /b"), want: "2 initrd"},
		"initrd two files":               {cfg: entry(" initrd /ucode.img /initrd.img"), want: "exactly one file"},
		"unknown variable":               {cfg: "set default=0\nmenuentry 'x' {\n linux /a $extra\n}\n", want: "$extra"},
		"variable set twice":             {cfg: "set a=1\nset a=2\nset default=0\nmenuentry 'x' {\n linux /a $a\n}\n", want: "$a"},
		"variable set by search":         {cfg: "search --set=kp abc\nset default=0\nmenuentry 'x' {\n linux /a $kp\n}\n", want: "$kp"},
		"double quote in arg":            {cfg: "set default=0\nmenuentry 'x' {\n linux /a 'x=\"y\"'\n}\n", want: "double quote"},
		"conditional id":                 {cfg: "set default=fw\nif [ x = y ]; then\nmenuentry 'x' --id fw {\n linux /a\n}\nfi\n", want: "no unconditional entry"},
		"conditional title":              {cfg: "set default=x\nif [ x = y ]; then\nmenuentry 'x' {\n linux /a\n}\nfi\n", want: "no unconditional entry"},
		"number after conditional":       {cfg: "set default=1\nif [ x = y ]; then\nmenuentry 'a' {\n linux /a\n}\nfi\nmenuentry 'b' {\n linux /b\n}\n", want: "no unconditional entry"},
		"missing default":                {cfg: "set default=7\nmenuentry 'x' {\n linux /vmlinuz\n}\n", want: "no unconditional entry"},
		"submenu default":                {cfg: "set default=0\nsubmenu 's' {\nmenuentry 'x' {\n linux /a\n}\n}\n", want: "submenu"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DefaultEntry(writeGrub(t, tc.cfg, tc.env))
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrUnsupported with %q", err, tc.want)
			}
		})
	}
}

func TestDefaultEntryNoConfig(t *testing.T) {
	if _, err := DefaultEntry(t.TempDir()); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want a missing grub.cfg error", err)
	}
}

func TestLexWords(t *testing.T) {
	s := &grubScript{vars: map[string]string{"a": "x y"}, unsure: map[string]bool{}}
	for in, want := range map[string]string{
		`linux /k $a`:       "linux|/k|x|y",    // unquoted expansion splits
		`linux /k "$a"`:     "linux|/k|x y",    // double quotes keep one word
		`linux /k '$a'`:     "linux|/k|$a",     // no expansion in single quotes
		`linux /k a\ b \$a`: "linux|/k|a b|$a", // backslash escapes
		`linux /k ${a}z`:    "linux|/k|x|yz",
	} {
		got, err := s.expandWords(in, nil)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if strings.Join(got, "|") != want {
			t.Fatalf("%s: words = %q, want %s", in, got, want)
		}
	}
	if got := strings.Join(literalWords(`menuentry 'A, b' $menuentry_id_option "id 1" {`), "|"); got != "menuentry|A, b|$menuentry_id_option|id 1|{" {
		t.Fatalf("literal words = %q", got)
	}
	if got := splitCommands(`if [ x ]; then insmod a; echo 'x;y'; fi # comment`); len(got) != 4 || got[2] != `echo 'x;y'` {
		t.Fatalf("commands = %q", got)
	}
}

func TestDefaultEntryTopLevelAllowList(t *testing.T) {
	body := "menuentry 'x' {\n linux /vmlinuz root=/dev/sda2\n}\n"
	for name, top := range map[string]string{
		"eval":          "eval 'set default=1'",
		"regexp":        "regexp --set=x a b",
		"probe":         "probe --set=uuid --fs-uuid hd0,gpt2",
		"load_env -f":   "load_env -f /boot/other.env",
		"unset":         "unset default",
		"unknown":       "chainloader /x.efi",
		"configfile":    "configfile /boot/other.cfg",
		"fwsetup other": "fwsetup",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DefaultEntry(writeGrub(t, "set default=0\n"+top+"\n"+body, ""))
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("err = %v, want ErrUnsupported", err)
			}
		})
	}
	// The generated commands pass.
	ok := "set default=0\nload_env\ninsmod part_gpt\nterminal_output console\nexport a\nfwsetup --is-supported\n" +
		"if [ -n x ]; then\n unset initrdless_boot_fallback_triggered\nfi\n" + body
	if _, err := DefaultEntry(writeGrub(t, ok, "")); err != nil {
		t.Fatalf("generated commands refused: %v", err)
	}
}

// Values expand when they are set, like GRUB, and only once.
func TestDefaultEntryExpandsAtAssignment(t *testing.T) {
	cfg := "set default=0\nset b=one\nset a=\"x $b\"\nset lit='$b'\nmenuentry 'x' {\n linux /vmlinuz $a $lit\n}\n"
	e, err := DefaultEntry(writeGrub(t, cfg, ""))
	if err != nil {
		t.Fatal(err)
	}
	if e.KernelArgs != "x one $b" {
		t.Fatalf("args = %q, want x one $b", e.KernelArgs)
	}
	// A value that uses an unsure variable makes the variable unsure.
	cfg = "set default=0\nif [ x ]; then\n set b=one\nfi\nset a=$b\nmenuentry 'x' {\n linux /vmlinuz $a\n}\n"
	if _, err := DefaultEntry(writeGrub(t, cfg, "")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	// search --set changes a variable that was set before.
	cfg = "set default=0\nset kp=quiet\nsearch --set=kp abc\nmenuentry 'x' {\n linux /vmlinuz $kp\n}\n"
	if _, err := DefaultEntry(writeGrub(t, cfg, "")); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "$kp") {
		t.Fatalf("err = %v, want ErrUnsupported for $kp", err)
	}
}

// GRUB takes the first entry whose id or title matches, in order, and only
// plain decimal numbers.
func TestDefaultEntryMatchesLikeGRUB(t *testing.T) {
	entries := "menuentry 'foo' --id a {\n linux /first\n}\nmenuentry 'b' --id foo {\n linux /second\n}\n"
	e, err := DefaultEntry(writeGrub(t, "set default=foo\n"+entries, ""))
	if err != nil || e.Kernel != "/first" {
		t.Fatalf("entry = %+v, %v; want the first entry whose title or id matches", e, err)
	}
	if e, err := DefaultEntry(writeGrub(t, "set default=1\n"+entries, "")); err != nil || e.Kernel != "/second" {
		t.Fatalf("default 1: %+v, %v", e, err)
	}
	for _, d := range []string{"01", "+1", "-1", "1x"} {
		if _, err := DefaultEntry(writeGrub(t, "set default="+d+"\n"+entries, "")); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("default %q: err = %v, want ErrUnsupported", d, err)
		}
	}
}

// Function bodies run where they are called; they get the allow-lists and
// may not set the default, load files or a kernel.
func TestDefaultEntryChecksFunctionBodies(t *testing.T) {
	entry := "menuentry 'x' {\n load_video\n linux /vmlinuz root=/dev/sda2\n}\n"
	for name, body := range map[string]string{
		"set default": " set default=1",
		"source":      " source /boot/extra.cfg",
		"linux":       " linux /evil",
		"initrd":      " initrd /evil.img",
		"unknown":     " chainloader /x.efi",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := "set default=0\nfunction load_video {\n" + body + "\n}\nload_video\n" + entry
			if _, err := DefaultEntry(writeGrub(t, cfg, "")); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "function body") {
				t.Fatalf("err = %v, want ErrUnsupported for the function body", err)
			}
		})
	}
	// The generated bodies pass.
	ok := "set default=0\nfunction load_video {\n if [ x$feature_all_video_module = xy ]; then\n  insmod all_video\n else\n  insmod efi_gop\n fi\n}\n" +
		"function gfxmode {\n set gfxpayload=\"${1}\"\n if [ \"${1}\" = \"keep\" ]; then\n  set vt_handoff=vt.handoff=7\n else\n  set vt_handoff=\n fi\n}\n" +
		"function recordfail {\n set recordfail=1\n if [ -n \"${have_grubenv}\" ]; then if [ -z \"${boot_once}\" ]; then save_env recordfail; fi; fi\n}\n" + entry
	if _, err := DefaultEntry(writeGrub(t, ok, "")); err != nil {
		t.Fatalf("generated function bodies refused: %v", err)
	}
}

// load_env imports every grubenv variable, also default, where it runs.
func TestDefaultEntryGrubenvImports(t *testing.T) {
	entry := "menuentry 'x' {\n linux /vmlinuz $kp\n}\nmenuentry 'y' {\n linux /other\n}\n"
	cfg := "load_env\nset default=0\nset kp=quiet\n" + entry
	if _, err := DefaultEntry(writeGrub(t, cfg, "default=1\n")); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "grubenv sets default") {
		t.Fatalf("grubenv default: err = %v", err)
	}
	if _, err := DefaultEntry(writeGrub(t, cfg, "kp=other\n")); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "$kp") {
		t.Fatalf("grubenv variable used in the kernel line: err = %v", err)
	}
	e, err := DefaultEntry(writeGrub(t, cfg, "recordfail=1\n"))
	if err != nil || e.KernelArgs != "quiet" {
		t.Fatalf("unrelated grubenv variable: %+v, %v", e, err)
	}
}

// Commands on the first line of a function, and search --set=default, at
// the top level, in a function and in an entry.
func TestDefaultEntryLastGaps(t *testing.T) {
	entry := "menuentry 'x' {\n linux /vmlinuz\n}\nmenuentry 'y' {\n linux /other\n}\n"
	for name, tc := range map[string]struct{ cfg, want string }{
		"one-line function":          {"set default=0\nfunction load_video { set default=1; }\nload_video\n" + entry, "first line"},
		"one-line function no semi":  {"set default=0\nfunction f { insmod x\n}\n" + entry, "first line"},
		"search default top level":   {"set default=0\nsearch --no-floppy --set=default --label y\n" + entry, "search --set=default"},
		"search -s default top":      {"set default=0\nsearch -s default --label y\n" + entry, "search --set=default"},
		"search default in function": {"set default=0\nfunction load_video {\n search --set default --label y\n}\nload_video\n" + entry, "search --set=default"},
		"search default in entry":    {"set default=0\nmenuentry 'x' {\n search --set=default --label y\n linux /vmlinuz\n}\n", "command search"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DefaultEntry(writeGrub(t, tc.cfg, "")); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrUnsupported with %q", err, tc.want)
			}
		})
	}
}

// The initrdless boot fallback of Ubuntu cloud images (GRUB_FORCE_PARTUUID):
// BOOTy kexecs the initrd branch, GRUB's own fallback for the same kernel.
func TestDefaultEntryInitrdlessFallback(t *testing.T) {
	root := copyFixture(t, "ubuntu-2404-gcp", "boot/grub", nil)
	e, err := DefaultEntry(root)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kernel != "/vmlinuz-7.0.0-1014-gcp" || e.Initramfs != "/initrd.img-7.0.0-1014-gcp" ||
		e.KernelArgs != "root=PARTUUID=25738ee1-aeb2-42e1-895b-9a587dbb9157 ro console=ttyS0,115200" {
		t.Fatalf("entry = %+v, want the initrd branch", e)
	}
	// A failed initrdless boot is pending: GRUB would take its own path.
	if _, err := DefaultEntry(copyFixture(t, "ubuntu-2404-gcp", "boot/grub", map[string]string{"initrdfail": "1"})); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("initrdfail set: err = %v, want ErrUnsupported", err)
	}
	fallback := func(then, els string) string {
		return "set default=0\nmenuentry 'x' {\n if [ \"${initrdfail}\" = 1 ]; then\n" + then + "\n else\n" + els + "\n fi\n initrdfail\n}\n"
	}
	for name, cfg := range map[string]string{
		"other kernel":     fallback("  linux /a ro\n  initrd /i", "  linux /b ro panic=-1"),
		"other root":       fallback("  linux /a root=/dev/sda1 ro\n  initrd /i", "  linux /a root=/dev/sda2 ro panic=-1"),
		"single in then":   fallback("  linux /a ro single\n  initrd /i", "  linux /a ro panic=-1"),
		"init in else":     fallback("  linux /a ro\n  initrd /i", "  linux /a ro init=/bin/sh panic=-1"),
		"panic not last":   fallback("  linux /a ro\n  initrd /i", "  linux /a panic=-1 ro"),
		"no panic":         fallback("  linux /a ro\n  initrd /i", "  linux /a ro"),
		"two panics":       fallback("  linux /a ro\n  initrd /i", "  linux /a ro panic=-1 panic=-1"),
		"no initrd":        fallback("  linux /a ro", "  linux /a ro panic=-1"),
		"initrd in else":   fallback("  linux /a ro\n  initrd /i", "  linux /a ro panic=-1\n  initrd /i"),
		"kernel outside":   strings.Replace(fallback("  linux /a ro\n  initrd /i", "  linux /a ro panic=-1"), " initrdfail\n", " initrdfail\n linux /c\n", 1),
		"nested condition": fallback("  if [ x = y ]; then\n  linux /a ro\n  fi\n  initrd /i", "  linux /a ro panic=-1"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DefaultEntry(writeGrub(t, cfg, "")); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("err = %v, want ErrUnsupported", err)
			}
		})
	}
	if e, err := DefaultEntry(writeGrub(t, fallback("  linux /a ro\n  initrd /i", "  linux /a ro panic=-1"), "")); err != nil || e.Kernel != "/a" || e.Initramfs != "/i" {
		t.Fatalf("minimal fallback: %+v, %v", e, err)
	}
}
