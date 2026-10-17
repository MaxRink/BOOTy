package kexec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ErrUnsupported marks a boot configuration that BOOTy can not resolve the
// way GRUB would. The caller reboots through the firmware instead of
// starting a kernel that GRUB might not start: any doubt, reboot.
var ErrUnsupported = errors.New("boot entry can not be resolved without GRUB")

// grubConfigPaths are the GRUB configurations below a root, in order:
// Debian and Ubuntu, then RHEL-likes and SUSE.
var grubConfigPaths = []string{filepath.Join("boot", "grub", "grub.cfg"), filepath.Join("boot", "grub2", "grub.cfg")}

// DefaultEntry returns the entry that GRUB boots by default from the
// installed root. It does not emulate GRUB: it accepts only the shapes that
// grub-mkconfig generates on Debian and Ubuntu (see docs/caprf-integration.md
// "kexec boot entry") and returns ErrUnsupported for everything else, among
// them BLS (RHEL-likes) and Flatcar.
//
// Accepted: an unconditional "set default", or the generated
// `if [ "${next_entry}" ] ; then ... else set default=X fi` block with
// next_entry unset in grubenv; ${saved_entry} from grubenv; a numeric,
// id or title path through submenus; entries made of the generated
// commands (insmod, echo, recordfail, load_video, gfxmode, set root,
// search --set=root, one linux and at most one initrd, outside conditions);
// variables set once and unconditionally at the top level; $vt_handoff.
func DefaultEntry(root string) (*BootEntry, error) {
	cfgPath, data, err := readGrubConfig(root)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Stat(filepath.Join(filepath.Dir(cfgPath), "custom.cfg"))
	cfg := parseGrubScript(string(data), statErr == nil)
	if cfg.problem != "" {
		return nil, fmt.Errorf("%w: %s: %s", ErrUnsupported, cfgPath, cfg.problem)
	}
	env := readGrubenv(filepath.Join(filepath.Dir(cfgPath), "grubenv"))
	if err := cfg.applyGrubenv(env); err != nil {
		return nil, err
	}
	spec, err := cfg.defaultSpec(env)
	if err != nil {
		return nil, err
	}
	item, err := cfg.resolve(spec)
	if err != nil {
		return nil, err
	}
	return cfg.bootEntry(item)
}

func readGrubConfig(root string) (path string, data []byte, err error) {
	for _, rel := range grubConfigPaths {
		p := filepath.Join(root, rel)
		data, err = os.ReadFile(p)
		if err == nil {
			return p, data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("read %s: %w", p, err)
		}
	}
	if entries, _ := filepath.Glob(filepath.Join(root, "boot", "loader", "entries", "*.conf")); len(entries) > 0 {
		return "", nil, fmt.Errorf("%w: BLS entries without grub.cfg", ErrUnsupported)
	}
	return "", nil, fmt.Errorf("no grub.cfg in %s", strings.Join(grubConfigPaths, " or "))
}

// grubItem is a menuentry or a submenu.
type grubItem struct {
	title, id string
	submenu   bool
	children  []*grubItem
	linux     []string // raw commands
	initrd    []string
	// fallback holds the linux and initrd commands of the two branches of
	// the generated initrdless boot fallback (initrdfailCond): [0] the
	// then branch (with initrd), [1] the else branch (initrdless).
	fallback [2]struct{ linux, initrd []string }
	// problem is the first command of the entry that is not in the
	// allow-list, or a kernel line inside a condition.
	problem string
	// conditional is an item inside an if or function block: GRUB may or
	// may not add it.
	conditional bool
}

type blockKind int

const (
	blockMenu blockKind = iota
	blockSubmenu
	blockIf
	blockFunction
	blockLoop
)

type block struct {
	kind   blockKind
	cond   string // normalized condition of an if
	inElse bool
}

type defaultSet struct {
	value string
	// nextEntryElse: the else branch of the generated next_entry block.
	nextEntryElse bool
}

type grubScript struct {
	items    []*grubItem
	defaults []defaultSet
	vars     map[string]string // variables set once, unconditionally, at the top level
	unsure   map[string]bool   // variables set in blocks, more than once, or by --set
	problem  string            // the first script-level reason to reboot
}

// nextEntryCond is the condition of the generated next_entry block.
const nextEntryCond = `[ "${next_entry}" ]`

// parseGrubScript reads the structure of a grub.cfg. It does not run it.
func parseGrubScript(text string, customCfg bool) *grubScript {
	s := &grubScript{vars: map[string]string{}, unsure: map[string]bool{}}
	var stack []block
	var menus []*grubItem
	for _, raw := range strings.Split(text, "\n") {
		for _, cmd := range splitCommands(raw) {
			stack, menus = s.command(cmd, stack, menus, customCfg)
		}
	}
	return s
}

func (s *grubScript) fail(format string, args ...any) {
	if s.problem == "" {
		s.problem = fmt.Sprintf(format, args...)
	}
}

// command handles one command of a line. Leading then, else and do are
// keywords of the enclosing block.
func (s *grubScript) command(raw string, stack []block, menus []*grubItem, customCfg bool) ([]block, []*grubItem) {
	raw, words := stripKeywords(raw, stack)
	if len(words) == 0 || strings.HasPrefix(words[0], "#") {
		return stack, menus
	}
	if stack, menus, ok := s.blockCommand(raw, words, stack, menus); ok {
		return stack, menus
	}
	if entry := currentEntry(stack, menus); entry != nil {
		entryCommand(entry, raw, words, stack)
		return stack, menus
	}
	s.topCommand(raw, words, stack, customCfg)
	return stack, menus
}

// stripKeywords drops a leading then, do or else; else switches the
// enclosing if to its else branch.
func stripKeywords(raw string, stack []block) (cmd string, words []string) {
	words = literalWords(raw)
	for len(words) > 0 && slices.Contains([]string{"then", "do", "else"}, words[0]) {
		if n := len(stack); words[0] == "else" && n > 0 && stack[n-1].kind == blockIf {
			stack[n-1].inElse = true
		}
		raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), words[0]))
		words = words[1:]
	}
	return raw, words
}

// blockCommand opens and closes entries, submenus, conditions, loops and
// functions. ok is false for any other command.
func (s *grubScript) blockCommand(raw string, words []string, stack []block, menus []*grubItem) (newStack []block, newMenus []*grubItem, ok bool) {
	n := len(stack)
	switch words[0] {
	case "menuentry", "submenu":
		stack, menus = s.openMenu(words, stack, menus)
	case "}", "fi", "done":
		stack, menus = closeBlock(words[0], stack, menus)
	case "if":
		stack = append(stack, block{kind: blockIf, cond: normalizeCond(raw)})
	case "elif":
		if n > 0 && stack[n-1].kind == blockIf {
			stack[n-1].cond, stack[n-1].inElse = "elif", true
		}
	case "while", "until", "for":
		stack = append(stack, block{kind: blockLoop})
	case "function":
		// grub-mkconfig writes "function name {" alone on its line; commands
		// after the "{" would run unchecked.
		if len(words) != 3 || words[2] != "{" {
			s.fail("function definition with commands on its first line")
		}
		stack = append(stack, block{kind: blockFunction})
	default:
		return stack, menus, false
	}
	return stack, menus, true
}

func (s *grubScript) openMenu(words []string, stack []block, menus []*grubItem) ([]block, []*grubItem) {
	item := newGrubItem(words)
	item.conditional = slices.ContainsFunc(stack, func(b block) bool { return b.kind >= blockIf })
	if len(menus) > 0 {
		menus[len(menus)-1].children = append(menus[len(menus)-1].children, item)
	} else {
		s.items = append(s.items, item)
	}
	kind := blockMenu
	if item.submenu {
		kind = blockSubmenu
	}
	return append(stack, block{kind: kind}), append(menus, item)
}

// closeBlock pops the block that "}" (entry, submenu, function) or fi and
// done (condition, loop) end.
func closeBlock(word string, stack []block, menus []*grubItem) ([]block, []*grubItem) {
	n := len(stack)
	if n == 0 {
		return stack, menus
	}
	top := stack[n-1].kind
	if word != "}" && top < blockIf {
		return stack, menus
	}
	if top <= blockSubmenu {
		menus = menus[:len(menus)-1]
	}
	return stack[:n-1], menus
}

// currentEntry is the menuentry that the command is in, or nil.
func currentEntry(stack []block, menus []*grubItem) *grubItem {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].kind {
		case blockMenu:
			return menus[len(menus)-1]
		case blockSubmenu:
			return nil
		}
	}
	return nil
}

// insideCondition reports an if or loop block inside the current entry.
func insideCondition(stack []block) bool {
	for i := len(stack) - 1; i >= 0 && stack[i].kind > blockSubmenu; i-- {
		if stack[i].kind == blockIf || stack[i].kind == blockLoop {
			return true
		}
	}
	return false
}

// entryCommands are the commands that grub-mkconfig puts into an entry
// and that do not change which kernel boots with which arguments.
var entryCommands = []string{"insmod", "echo", "recordfail", "load_video", "gfxmode", "initrdfail"}

func entryCommand(entry *grubItem, raw string, words []string, stack []block) {
	isLinux := slices.Contains([]string{"linux", "linux16", "linuxefi"}, words[0])
	isInitrd := slices.Contains([]string{"initrd", "initrd16", "initrdefi"}, words[0])
	if branch, ok := initrdfailBranch(stack); ok && (isLinux || isInitrd) {
		b := &entry.fallback[branch]
		if isLinux {
			b.linux = append(b.linux, raw)
		} else {
			b.initrd = append(b.initrd, raw)
		}
		return
	}
	switch {
	case isLinux:
		entry.linux = append(entry.linux, raw)
	case isInitrd:
		entry.initrd = append(entry.initrd, raw)
	}
	if problem := entryProblem(words, stack); problem != "" && entry.problem == "" {
		entry.problem = problem
	}
}

// initrdfailCond is the condition of the initrdless boot fallback that
// grub-mkconfig writes with GRUB_FORCE_PARTUUID (Ubuntu cloud images).
const initrdfailCond = `[ "${initrdfail}" = 1 ]`

// initrdfailBranch reports a command whose only condition inside the
// entry is the initrdless boot fallback, and its branch: 0 then, 1 else.
func initrdfailBranch(stack []block) (int, bool) {
	var conds []block
	for i := len(stack) - 1; i >= 0 && stack[i].kind > blockSubmenu; i-- {
		conds = append(conds, stack[i])
	}
	if len(conds) != 1 || conds[0].kind != blockIf || conds[0].cond != initrdfailCond {
		return 0, false
	}
	if conds[0].inElse {
		return 1, true
	}
	return 0, true
}

// entryProblem is why a command of an entry makes BOOTy reboot, or "".
func entryProblem(words []string, stack []block) string {
	switch {
	case slices.Contains([]string{"linux", "linux16", "linuxefi", "initrd", "initrd16", "initrdefi"}, words[0]):
		if insideCondition(stack) {
			return words[0] + " command inside a condition"
		}
		return ""
	case strings.HasPrefix(words[0], "linux$") || strings.HasPrefix(words[0], "linux${"):
		return "kernel command " + words[0]
	case words[0] == "set" && len(words) == 2 && strings.HasPrefix(words[1], "root="),
		words[0] == "search" && setsOnlyRoot(words),
		slices.Contains(entryCommands, words[0]):
		return ""
	default:
		return "command " + words[0]
	}
}

// setsOnlyRoot reports a search command whose --set target is root.
func setsOnlyRoot(words []string) bool {
	target := ""
	for i, w := range words {
		switch {
		case strings.HasPrefix(w, "--set="):
			target = strings.TrimPrefix(w, "--set=")
		case (w == "--set" || w == "-s") && i+1 < len(words):
			target = words[i+1]
		}
	}
	return target == "root"
}

// customSources are the 41_custom lines of grub-mkconfig.
var customSources = []string{"${config_directory}/custom.cfg", "$prefix/custom.cfg"}

// topCommands are the commands that grub-mkconfig puts outside entries
// and that BOOTy accepts. Anything else outside a function body is a
// problem: eval, regexp, probe, load_env -f, configfile, unknown commands.
var topCommands = []string{"set", "export", "insmod", "save_env", "load_env", "terminal_input",
	"terminal_output", "terminal", "serial", "font", "loadfont", "load_video", "background_image",
	"echo", "true", "search"}

// generatedUnsets are the variables that grub-mkconfig unsets
// (01_track_initrdless_boot_fallback on Ubuntu).
var generatedUnsets = []string{"initrdless_boot_fallback_triggered"}

func (s *grubScript) topCommand(raw string, words []string, stack []block, customCfg bool) {
	if slices.ContainsFunc(stack, func(b block) bool { return b.kind == blockFunction }) {
		// A function body runs where it is called, at the top level or in an
		// entry. It gets the top-level allow-list plus the entry helpers, and
		// may not set the default, load files or a kernel. Its sets are
		// unsure.
		if problem := functionProblem(words, customCfg); problem != "" {
			s.fail("function body: %s", problem)
			return
		}
		if name, ok := assignedName(words); ok {
			s.unsure[name] = true
		}
		return
	}
	if problem := topProblem(words, customCfg); problem != "" {
		s.fail("%s", problem)
		return
	}
	switch words[0] {
	case "unset":
		s.unsure[words[1]] = true
	case "search":
		for _, name := range setTargets(words) {
			s.unsure[name] = true
		}
	default:
		s.assignment(raw, words, stack, len(stack) > 0)
	}
}

// topChecks hold the commands that are allowed outside entries only with
// some arguments; each returns the problem or "".
var topChecks = map[string]func(args []string, customCfg bool) string{
	"search": func(args []string, _ bool) string {
		if slices.Contains(setTargets(args), "default") {
			return "search --set=default"
		}
		return ""
	},
	"blscfg": func([]string, bool) string { return "BLS entries (blscfg)" },
	"source": checkSource,
	".":      checkSource,
	"load_env": func(args []string, _ bool) string {
		if slices.ContainsFunc(args, func(w string) bool { return w == "-f" || strings.HasPrefix(w, "--file") }) {
			return "load_env from another file"
		}
		return ""
	},
	"unset": func(args []string, _ bool) string {
		if len(args) == 1 && slices.Contains(generatedUnsets, args[0]) {
			return ""
		}
		return "unset " + strings.Join(args, " ")
	},
	// 30_uefi-firmware checks for the firmware entry; it boots nothing.
	"fwsetup": func(args []string, _ bool) string {
		if len(args) == 1 && args[0] == "--is-supported" {
			return ""
		}
		return "command fwsetup"
	},
}

// checkSource accepts only the 41_custom line, and only without custom.cfg.
func checkSource(args []string, customCfg bool) string {
	if len(args) == 1 && slices.Contains(customSources, args[0]) && !customCfg {
		return ""
	}
	return "source " + strings.Join(args, " ")
}

// topProblem is why a command outside entries makes BOOTy reboot, or "".
func topProblem(words []string, customCfg bool) string {
	if check, ok := topChecks[words[0]]; ok {
		return check(words[1:], customCfg)
	}
	if slices.Contains(topCommands, words[0]) {
		return ""
	}
	if _, ok := assignedName(words); ok {
		return ""
	}
	return "command " + words[0]
}

// functionProblem is why a command of a function body makes BOOTy reboot,
// or "".
func functionProblem(words []string, customCfg bool) string {
	if name, ok := assignedName(words); ok && name == "default" {
		return "set default"
	}
	switch {
	case slices.Contains([]string{"source", ".", "configfile"}, words[0]):
		return words[0]
	case strings.HasPrefix(words[0], "linux") || strings.HasPrefix(words[0], "initrd"):
		return words[0]
	case slices.Contains(entryCommands, words[0]):
		return ""
	}
	return topProblem(words, customCfg)
}

// setTargets returns the variables of the --set options of a command.
func setTargets(words []string) []string {
	var names []string
	for i, w := range words {
		if name, ok := strings.CutPrefix(w, "--set="); ok {
			names = append(names, name)
		} else if (w == "--set" || w == "-s") && i+1 < len(words) {
			names = append(names, words[i+1])
		}
	}
	return names
}

// assignedName returns the variable of "set name=value" or "name=value".
func assignedName(words []string) (string, bool) {
	w := words
	if w[0] == "set" && len(w) > 1 {
		w = w[1:]
	}
	if len(w) != 1 {
		return "", false
	}
	name, _, ok := strings.Cut(w[0], "=")
	if !ok || name == "" || strings.ContainsAny(name, "$ ") {
		return "", false
	}
	return name, true
}

// assignment records "set name=value" and "name=value". The value is
// expanded now, like GRUB does: "set a=$b" takes the current value of b.
// A value that does not expand makes the variable unsure.
func (s *grubScript) assignment(raw string, words []string, stack []block, inBlock bool) {
	name, ok := assignedName(words)
	if !ok {
		return
	}
	// The raw value keeps its quotes for the expansion.
	rawValue := strings.TrimSpace(raw[strings.Index(raw, "=")+1:])
	if name == "default" {
		s.defaultAssignment(rawValue, stack)
		return
	}
	if _, seen := s.vars[name]; seen || inBlock {
		s.unsure[name] = true
		return
	}
	value, err := s.expandWords(rawValue, nil)
	if err != nil {
		s.unsure[name] = true
		return
	}
	s.vars[name] = strings.Join(value, " ")
}

// defaultAssignment accepts an unconditional "set default" and the
// generated next_entry block. Any other conditional default is a problem.
func (s *grubScript) defaultAssignment(rawValue string, stack []block) {
	value := strings.Join(literalWords(rawValue), " ")
	switch {
	case len(stack) == 0:
		s.defaults = append(s.defaults, defaultSet{value: rawValue})
	case len(stack) == 1 && stack[0].kind == blockIf && stack[0].cond == nextEntryCond && !stack[0].inElse && value == "${next_entry}":
		// The then branch only runs with next_entry set, which is refused.
	case len(stack) == 1 && stack[0].kind == blockIf && stack[0].cond == nextEntryCond && stack[0].inElse:
		s.defaults = append(s.defaults, defaultSet{value: rawValue, nextEntryElse: true})
	default:
		s.fail("conditional default %s", value)
	}
}

func normalizeCond(raw string) string {
	cond := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "if"))
	cond = strings.TrimSpace(strings.TrimSuffix(cond, "then"))
	return strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(cond), ";")), " ")
}

func newGrubItem(words []string) *grubItem {
	item := &grubItem{submenu: words[0] == "submenu"}
	for i := 1; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "{":
		case item.title == "" && !strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "$"):
			item.title = w
		case strings.HasPrefix(w, "--id="):
			item.id = strings.TrimPrefix(w, "--id=")
		case (w == "--id" || w == "$menuentry_id_option" || w == "${menuentry_id_option}") && i+1 < len(words):
			item.id = words[i+1]
			i++
		}
	}
	return item
}

// applyGrubenv accounts for load_env: GRUB imports every grubenv variable
// at the load_env line, also default. BOOTy does not track where that line
// is, so a grubenv default is refused and every other grubenv variable is
// unsure (saved_entry and next_entry are handled by defaultSpec).
func (s *grubScript) applyGrubenv(env map[string]string) error {
	if _, ok := env["default"]; ok {
		return fmt.Errorf("%w: grubenv sets default", ErrUnsupported)
	}
	for key := range env {
		s.unsure[key] = true
	}
	return nil
}

// defaultSpec is the default that GRUB uses. Every grubenv state that the
// generated script handles with logic BOOTy does not run is refused:
// next_entry (GRUB would consume it), prev_saved_entry, and the Ubuntu
// initrdless fallback (initrdfail, prev_entry).
func (s *grubScript) defaultSpec(env map[string]string) (string, error) {
	for _, key := range []string{"next_entry", "prev_saved_entry", "initrdfail", "prev_entry"} {
		if env[key] != "" {
			return "", fmt.Errorf("%w: grubenv sets %s", ErrUnsupported, key)
		}
	}
	spec := "0"
	for _, d := range s.defaults {
		words, err := s.expandWords(d.value, map[string]string{"saved_entry": env["saved_entry"]})
		if err != nil {
			return "", fmt.Errorf("default: %w", err)
		}
		spec = strings.Join(words, " ")
	}
	if spec == "" {
		spec = "0"
	}
	return spec, nil
}

// resolve finds the item of a default like "0", "1>2", a title or an id.
// An item inside a condition, or a number after one, is refused.
func (s *grubScript) resolve(spec string) (*grubItem, error) {
	items := s.items
	var item *grubItem
	for _, part := range strings.Split(spec, ">") {
		item = findItem(items, part)
		if item == nil {
			return nil, fmt.Errorf("%w: no unconditional entry %q of default %q", ErrUnsupported, part, spec)
		}
		items = item.children
	}
	if item == nil || item.submenu {
		return nil, fmt.Errorf("%w: default %q is a submenu", ErrUnsupported, spec)
	}
	return item, nil
}

func findItem(items []*grubItem, part string) *grubItem {
	if isDecimal(part) {
		n, err := strconv.Atoi(part)
		if err != nil || n >= len(items) || slices.ContainsFunc(items[:n+1], func(it *grubItem) bool { return it.conditional }) {
			return nil
		}
		return items[n]
	}
	if strings.TrimLeft(part, "+-0123456789") == "" {
		return nil // a sign or a leading zero: GRUB parses it differently
	}
	// Like GRUB: the first entry whose id or title matches, in order.
	i := slices.IndexFunc(items, func(it *grubItem) bool { return it.id == part || it.title == part })
	if i < 0 || items[i].conditional {
		return nil
	}
	return items[i]
}

// isDecimal reports a plain decimal number: digits only, no sign, no
// leading zero.
func isDecimal(s string) bool {
	if s == "" || s != "0" && s[0] == '0' {
		return false
	}
	return strings.Trim(s, "0123456789") == ""
}

func (s *grubScript) bootEntry(item *grubItem) (*BootEntry, error) {
	if err := item.useInitrdFallback(); err != nil {
		return nil, err
	}
	switch {
	case item.problem != "":
		return nil, fmt.Errorf("%w: entry %q: %s", ErrUnsupported, item.title, item.problem)
	case len(item.linux) != 1:
		return nil, fmt.Errorf("%w: entry %q has %d linux commands", ErrUnsupported, item.title, len(item.linux))
	case len(item.initrd) > 1:
		return nil, fmt.Errorf("%w: entry %q has %d initrd commands", ErrUnsupported, item.title, len(item.initrd))
	}
	words, err := s.expandWords(item.linux[0], nil)
	if err != nil {
		return nil, fmt.Errorf("entry %q: %w", item.title, err)
	}
	if len(words) < 2 {
		return nil, fmt.Errorf("%w: entry %q has no kernel", ErrUnsupported, item.title)
	}
	args, err := kernelArgs(words[2:])
	if err != nil {
		return nil, fmt.Errorf("entry %q: %w", item.title, err)
	}
	entry := &BootEntry{Name: item.title, Kernel: words[1], KernelArgs: args}
	if len(item.initrd) == 1 {
		initrd, err := s.expandWords(item.initrd[0], nil)
		if err != nil || len(initrd) != 2 {
			return nil, fmt.Errorf("%w: entry %q: initrd needs exactly one file", ErrUnsupported, item.title)
		}
		entry.Initramfs = initrd[1]
	}
	return entry, nil
}

// useInitrdFallback takes the initrd branch of the initrdless boot fallback
// as the kernel and initrd of the entry. GRUB boots the initrdless branch
// while grubenv has no initrdfail (DefaultEntry refuses a set initrdfail),
// but BOOTy kexecs only with an initrd, and the initrd branch is GRUB's
// own fallback for the same kernel and root: it boots on every image,
// also when the initrdless boot would fail. BOOTy leaves grubenv as it is.
// The else line must be the then line plus one trailing panic=-1, and the
// entry must have no other kernel command.
func (item *grubItem) useInitrdFallback() error {
	then, els := item.fallback[0], item.fallback[1]
	if len(then.linux)+len(then.initrd)+len(els.linux)+len(els.initrd) == 0 {
		return nil
	}
	switch {
	case len(item.linux) > 0 || len(item.initrd) > 0:
		return fmt.Errorf("%w: entry %q has kernel commands inside and outside the initrd fallback", ErrUnsupported, item.title)
	case len(then.linux) != 1 || len(then.initrd) != 1 || len(els.linux) != 1 || len(els.initrd) != 0:
		return fmt.Errorf("%w: entry %q: initrd fallback of an unknown shape", ErrUnsupported, item.title)
	}
	// The generated else line is the then line plus panic=-1; anything
	// else (root, init, runlevel) would boot what GRUB never boots.
	thenWords, elseWords := literalWords(then.linux[0]), literalWords(els.linux[0])
	if len(thenWords) < 2 || !slices.Equal(elseWords, append(slices.Clone(thenWords), "panic=-1")) {
		return fmt.Errorf("%w: entry %q: the initrd fallback branches differ in more than panic=-1", ErrUnsupported, item.title)
	}
	item.linux, item.initrd = then.linux, then.initrd
	return nil
}

// kernelArgs joins the words to a kernel command line. A word with blanks
// is quoted like the kernel parses it; a word with a double quote is
// refused, the kernel has no escape for it.
func kernelArgs(words []string) (string, error) {
	out := make([]string, 0, len(words))
	for _, w := range words {
		switch {
		case strings.Contains(w, `"`):
			return "", fmt.Errorf("%w: kernel argument with a double quote", ErrUnsupported)
		case strings.ContainsAny(w, " \t"):
			out = append(out, `"`+w+`"`)
		default:
			out = append(out, w)
		}
	}
	return strings.Join(out, " "), nil
}

// readGrubenv reads a GRUB environment block: name=value lines, "#"
// padding and comments. A missing file is an empty environment.
func readGrubenv(path string) map[string]string {
	env := map[string]string{}
	data, err := os.ReadFile(path) //nolint:gosec // path below the installed root
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			env[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	return env
}
