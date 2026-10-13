package disk

import "strings"

// ParseBlkidExport parses `blkid -o export`: KEY=value lines, one block per
// device, blocks separated by empty lines. Values keep trailing spaces:
// blkid escapes a label ending in a space as "LABEL=BOOT\ ".
func ParseBlkidExport(out string) []map[string]string {
	var devices []map[string]string
	cur := map[string]string{}
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(cur) > 0 {
				devices = append(devices, cur)
				cur = map[string]string{}
			}
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			cur[key] = unescapeBlkidValue(value)
		}
	}
	if len(cur) > 0 {
		devices = append(devices, cur)
	}
	return devices
}

// unescapeBlkidValue removes the backslash escaping of `blkid -o export`.
func unescapeBlkidValue(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String()
}
