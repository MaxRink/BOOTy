package logging

import "regexp"

// The value rules of RedactText. pkg/provision uses them for debug dumps and
// machine commands; the CAPRF log sink applies them to each line before it
// buffers the line.
var (
	pemPrivateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----(?:.*?-----END [A-Z ]*PRIVATE KEY-----|.*)`)
	jwtPattern           = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}`)
	// luksKeyDumpPattern matches the hex key bytes that cryptsetup prints
	// for luksDump --dump-master-key or --dump-volume-key.
	luksKeyDumpPattern     = regexp.MustCompile(`(?i)\b(MK dump|master key|volume key)\s*:\s*(?:[0-9a-f]{2}\s+)*[0-9a-f]{2}\b`)
	authorizationPattern   = regexp.MustCompile(`(?i)(^|[\s,;&?"])(authorization)\s*:\s*([^",;]+)`)
	bearerPattern          = regexp.MustCompile(`(?i)\bBearer\s+["']?[^"'\s,;]+["']?`)
	urlCredentialPattern   = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^@\s/]+@`)
	sensitiveAssignPattern = regexp.MustCompile(
		`(?i)(^|[\s,;&?=])` +
			`([A-Za-z0-9_.-]*(?:password|passwd|passphrase|token|secret|credential|authorization|auth|private)[A-Za-z0-9_.-]*|` +
			`(?:api[-_]?key|secret[-_]?key|private[-_]?key|access[-_]?key|luks[-_]?key|master[-_]?key|volume[-_]?key|key))` +
			`\s*([=:])\s*("[^"]*"|'[^']*'|[^\s,;&]+)`)
)

// RedactText replaces known secret shapes in free text with [REDACTED]:
// PEM private key blocks, JWTs, LUKS key dumps, Authorization headers,
// bearer tokens, URL user info, and values of sensitive keys such as
// password= or token:. It leaves prose such as "token can not be renewed"
// alone, so it is safe on every log line; pkg/provision adds the
// "password <value>" word rule for command lines.
func RedactText(s string) string {
	s = pemPrivateKeyPattern.ReplaceAllString(s, `[REDACTED PRIVATE KEY]`)
	s = jwtPattern.ReplaceAllString(s, `[REDACTED]`)
	s = luksKeyDumpPattern.ReplaceAllString(s, `${1}: [REDACTED]`)
	s = authorizationPattern.ReplaceAllString(s, `${1}${2}: [REDACTED]`)
	s = bearerPattern.ReplaceAllString(s, `Bearer [REDACTED]`)
	s = urlCredentialPattern.ReplaceAllString(s, `${1}[REDACTED]@`)
	return sensitiveAssignPattern.ReplaceAllString(s, `${1}${2}${3}[REDACTED]`)
}
