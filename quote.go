package git

import (
	"fmt"
	"strings"
)

// quote applies git's C-style path quoting (core.quotePath=true): paths with
// control characters, '"', '\', or non-ASCII bytes are wrapped in quotes with
// escapes. Short status also quotes paths that contain a space.
func quote(p string, space bool) string {
	needs := space && strings.Contains(p, " ")
	for i := 0; i < len(p) && !needs; i++ {
		c := p[i]
		needs = c < 0x20 || c == '"' || c == '\\' || c >= 0x7f
	}
	if !needs {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 || c >= 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// q quotes a path for most output; qs also quotes spaces (short status).
func q(p string) string  { return quote(p, false) }
func qs(p string) string { return quote(p, true) }
