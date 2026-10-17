package kexec

import (
	"fmt"
	"strings"
)

// The GRUB script lexing that DefaultEntry needs: commands of a line,
// words with GRUB quoting (single quotes are literal, double quotes expand
// variables, a backslash escapes the next character) and the expansion of
// $name and ${name}.

// cosmeticVars are GRUB variables that only change the console handover.
// GRUB sets them in functions, so BOOTy can not evaluate them; an empty
// value is what GRUB uses in text mode.
var cosmeticVars = map[string]bool{"vt_handoff": true}

// splitCommands splits a line at the unquoted ";" and drops a comment.
func splitCommands(line string) []string {
	var cmds []string
	var cur strings.Builder
	quote, escaped, wordStart := rune(0), false, true
	for _, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && wordStart:
			return appendCommand(cmds, cur.String())
		case r == ';':
			cmds = appendCommand(cmds, cur.String())
			cur.Reset()
			wordStart = true
			continue
		}
		wordStart = quote == 0 && !escaped && (r == ' ' || r == '\t')
		cur.WriteRune(r)
	}
	return appendCommand(cmds, cur.String())
}

func appendCommand(cmds []string, cmd string) []string {
	if cmd = strings.TrimSpace(cmd); cmd != "" {
		return append(cmds, cmd)
	}
	return cmds
}

// literalWords returns the words of a command without expansion: quotes and
// escapes are removed, "$name" stays as it is.
func literalWords(cmd string) []string {
	words, _ := lexWords(cmd, nil, 0)
	return words
}

// expandWords returns the words of a command like GRUB: variables in
// unquoted and double-quoted text are expanded, an unquoted expansion is
// split into fields. extra wins over the variables of the script. A
// variable that grub.cfg does not set once and unconditionally at the top
// level returns ErrUnsupported.
func (s *grubScript) expandWords(cmd string, extra map[string]string) ([]string, error) {
	lookup := func(name string, _ int) ([]string, error) {
		if v, ok := extra[name]; ok {
			return strings.Fields(v), nil
		}
		if cosmeticVars[name] {
			return nil, nil
		}
		value, ok := s.vars[name]
		if !ok || s.unsure[name] {
			return nil, fmt.Errorf("%w: variable $%s is not set once and unconditionally in grub.cfg", ErrUnsupported, name)
		}
		// The value was expanded when it was set; it is not expanded again.
		return strings.Fields(value), nil
	}
	return lexWords(cmd, lookup, 0)
}

// lexer collects the words of a command.
type lexer struct {
	words   []string
	cur     strings.Builder
	started bool
	quote   rune
	lookup  func(string, int) ([]string, error)
	depth   int
}

// lexWords splits a command into words. With lookup nil, "$..." stays
// literal.
func lexWords(cmd string, lookup func(string, int) ([]string, error), depth int) ([]string, error) {
	if depth > 5 {
		return nil, fmt.Errorf("%w: variables nest too deep", ErrUnsupported)
	}
	l := &lexer{lookup: lookup, depth: depth}
	runes := []rune(cmd)
	for i := 0; i < len(runes); i++ {
		n, err := l.step(runes, i)
		if err != nil {
			return nil, err
		}
		i += n
	}
	if l.quote != 0 {
		return nil, fmt.Errorf("%w: unterminated quote", ErrUnsupported)
	}
	l.flush()
	return l.words, nil
}

// step reads the rune at i and returns how many more runes it consumed.
func (l *lexer) step(runes []rune, i int) (int, error) {
	r := runes[i]
	switch {
	case r == '\\' && l.quote != '\'' && i+1 < len(runes):
		l.add(string(runes[i+1]))
		return 1, nil
	case l.quoteRune(r):
	case r == '$' && l.quote != '\'' && l.lookup != nil:
		return l.expand(runes[i+1:], l.quote == '"', l.lookup, l.depth)
	case l.quote == 0 && (r == ' ' || r == '\t'):
		l.flush()
	default:
		l.add(string(r))
	}
	return 0, nil
}

// quoteRune opens or closes a quote and reports whether r was one.
func (l *lexer) quoteRune(r rune) bool {
	switch {
	case l.quote != 0 && r == l.quote:
		l.quote = 0
	case l.quote == 0 && (r == '\'' || r == '"'):
		l.quote, l.started = r, true
	default:
		return false
	}
	return true
}

func (l *lexer) add(s string) {
	l.cur.WriteString(s)
	l.started = true
}

func (l *lexer) flush() {
	if l.started {
		l.words = append(l.words, l.cur.String())
	}
	l.cur.Reset()
	l.started = false
}

// expand expands the variable after a "$" and returns the runes it read.
// Inside double quotes the value is one word part; unquoted it splits.
func (l *lexer) expand(rest []rune, quoted bool, lookup func(string, int) ([]string, error), depth int) (int, error) {
	name, n := varName(rest)
	if name == "" {
		l.add("$")
		return 0, nil
	}
	fields, err := lookup(name, depth)
	if err != nil {
		return 0, err
	}
	if quoted {
		l.add(strings.Join(fields, " "))
		return n, nil
	}
	for j, f := range fields {
		if j > 0 {
			l.flush()
		}
		l.add(f)
	}
	return n, nil
}

// varName reads a variable name after "$": "{name}" or name characters.
// It returns the name and the number of runes read.
func varName(rs []rune) (name string, n int) {
	if len(rs) > 0 && rs[0] == '{' {
		for i := 1; i < len(rs); i++ {
			if rs[i] == '}' {
				return string(rs[1:i]), i + 1
			}
		}
		return "", 0
	}
	for n < len(rs) && (rs[n] == '_' || rs[n] >= 'a' && rs[n] <= 'z' || rs[n] >= 'A' && rs[n] <= 'Z' || rs[n] >= '0' && rs[n] <= '9') {
		n++
	}
	return string(rs[:n]), n
}
