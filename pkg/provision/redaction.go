//go:build linux

package provision

import (
	"regexp"

	"github.com/telekom/BOOTy/pkg/logging"
)

// wordValuePattern redacts the word after a sensitive keyword, as in the
// FRR "neighbor x password y" directive. It runs only on commands and
// debug dumps: on log prose it would hide words like "token can not".
var wordValuePattern = regexp.MustCompile(`(?i)\b(password|passwd|passphrase|token|secret|credential|authorization)\s+("[^"]*"|'[^']*'|[^\s,;]+)`)

func redactCommand(line string) string {
	redacted := logging.RedactText(redactCommandValues(line))
	return wordValuePattern.ReplaceAllString(redacted, `${1} [REDACTED]`)
}

func redactDebugData(line string) string {
	return redactCommand(line)
}
