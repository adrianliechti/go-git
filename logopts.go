package git

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
)

// logFilter holds the commit-limiting options of git log.
type logFilter struct {
	authors, committers, greps []*regexp.Regexp
	allMatch, invertGrep       bool
	ignoreCase                 bool
	since, until               *time.Time
	sinceAsFilter              bool
	noMerges, merges           bool
	minParents, maxParents     int // -1 when unset
	pickaxe                    string
	pickaxeRegex               bool
	grepDiff                   *regexp.Regexp
	skip                       int
	firstParent                bool
	follow                     bool
}

// parseLogOption handles options beyond the basic ones. It reports whether
// args[*i] was consumed.
func (f *logFormat) parseLogOption(args []string, i *int) (bool, error) {
	a := args[*i]
	value := func(name string) (string, bool) {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, true
		}
		if a == name && *i+1 < len(args) {
			*i++
			return args[*i], true
		}
		return "", false
	}
	compile := func(pat string) (*regexp.Regexp, error) {
		if f.filter.ignoreCase {
			pat = "(?i)" + pat
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fatalf("invalid regular expression: %s", pat)
		}
		return re, nil
	}
	if v, ok := value("--pretty"); ok {
		return true, f.setPretty(v)
	}
	if v, ok := value("--format"); ok {
		return true, f.setPretty(v)
	}
	if v, ok := value("--date"); ok {
		f.dateMode = v
		return true, nil
	}
	for _, name := range []string{"--author", "--committer", "--grep"} {
		if v, ok := value(name); ok {
			re, err := compile(v)
			if err != nil {
				return true, err
			}
			switch name {
			case "--author":
				f.filter.authors = append(f.filter.authors, re)
			case "--committer":
				f.filter.committers = append(f.filter.committers, re)
			default:
				f.filter.greps = append(f.filter.greps, re)
			}
			return true, nil
		}
	}
	for _, name := range []string{"--since", "--after", "--until", "--before", "--since-as-filter"} {
		if v, ok := value(name); ok {
			t, err := approxidate(v)
			if err != nil {
				return true, fatalf("invalid date '%s'", v)
			}
			switch name {
			case "--until", "--before":
				f.filter.until = &t
			case "--since-as-filter":
				f.filter.since, f.filter.sinceAsFilter = &t, true
			default:
				f.filter.since = &t
			}
			return true, nil
		}
	}
	for _, name := range []string{"--skip", "--min-parents", "--max-parents", "--abbrev"} {
		if v, ok := value(name); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return true, fatalf("'%s': not an integer", v)
			}
			switch name {
			case "--skip":
				f.filter.skip = n
			case "--min-parents":
				f.filter.minParents = n
			case "--max-parents":
				f.filter.maxParents = n
			case "--abbrev":
				f.abbrevLen = n
			}
			return true, nil
		}
	}
	switch {
	case strings.HasPrefix(a, "-S") && len(a) > 2:
		f.filter.pickaxe = a[2:]
	case a == "-S" && *i+1 < len(args):
		*i++
		f.filter.pickaxe = args[*i]
	case strings.HasPrefix(a, "-G"):
		pat := a[2:]
		if pat == "" && *i+1 < len(args) {
			*i++
			pat = args[*i]
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return true, fatalf("invalid regular expression: %s", pat)
		}
		f.filter.grepDiff = re
	case a == "--pickaxe-regex":
		f.filter.pickaxeRegex = true
	case a == "--all-match":
		f.filter.allMatch = true
	case a == "--invert-grep":
		f.filter.invertGrep = true
	case a == "-i" || a == "--regexp-ignore-case":
		f.filter.ignoreCase = true
	case a == "--no-merges":
		f.filter.maxParents = 1
	case a == "--merges":
		f.filter.minParents = 2
	case a == "--first-parent":
		f.filter.firstParent = true
	case a == "--follow":
		f.filter.follow = true
	case a == "--abbrev-commit":
		f.abbrev = true
	case a == "--no-abbrev-commit":
		f.abbrev = false
	case a == "--shortstat":
		f.shortstat = true
	case a == "--numstat":
		f.numstat = true
	case a == "--summary":
		f.summary = true
	case a == "--left-right":
		f.leftRight = true
	case a == "--not":
		f.revs = append(f.revs, "--not")
	case a == "--no-renames", a == "-M", a == "--find-renames", a == "--no-expand-tabs", a == "--no-mailmap", a == "--mailmap", a == "--no-show-signature":
	default:
		return false, nil
	}
	return true, nil
}

func (f *logFormat) setPretty(v string) error {
	switch v {
	case "", "medium":
		f.kind = "medium"
	case "oneline":
		f.kind = "full-oneline"
	case "short", "full", "fuller", "raw", "reference", "email":
		f.kind = v
	default:
		switch {
		case strings.HasPrefix(v, "format:"):
			f.kind, f.template = "format", strings.TrimPrefix(v, "format:")
		case strings.HasPrefix(v, "tformat:"):
			f.kind, f.template = "tformat", strings.TrimPrefix(v, "tformat:")
		case strings.Contains(v, "%"):
			f.kind, f.template = "tformat", v
		default:
			return fatalf("invalid --pretty format: %s", v)
		}
	}
	return nil
}

// accept applies the commit-limiting filters that need only the commit.
func (lf *logFilter) accept(c *object.Commit) bool {
	n := len(c.ParentHashes)
	if lf.minParents >= 0 && n < lf.minParents {
		return false
	}
	if lf.maxParents >= 0 && n > lf.maxParents {
		return false
	}
	if lf.since != nil && c.Committer.When.Before(*lf.since) {
		return false
	}
	if lf.until != nil && c.Committer.When.After(*lf.until) {
		return false
	}
	if !matchAnyRegexp(lf.authors, ident(c.Author)) || !matchAnyRegexp(lf.committers, ident(c.Committer)) {
		return false
	}
	if len(lf.greps) > 0 {
		ok := false
		if lf.allMatch {
			ok = true
			for _, re := range lf.greps {
				ok = ok && re.MatchString(c.Message)
			}
		} else {
			ok = matchAnyRegexp(lf.greps, c.Message)
		}
		if ok == lf.invertGrep {
			return false
		}
	}
	return true
}

func matchAnyRegexp(res []*regexp.Regexp, s string) bool {
	if len(res) == 0 {
		return true
	}
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// acceptDiff applies -S and -G, which look at the commit's changes.
func (lf *logFilter) acceptDiff(diffs []*fileDiff) bool {
	if lf.pickaxe == "" && lf.grepDiff == nil {
		return true
	}
	for _, d := range diffs {
		if d.binary {
			continue
		}
		if lf.pickaxe != "" {
			count := func(b []byte) int {
				if lf.pickaxeRegex {
					re, err := regexp.Compile(lf.pickaxe)
					if err != nil {
						return 0
					}
					return len(re.FindAllIndex(b, -1))
				}
				return strings.Count(string(b), lf.pickaxe)
			}
			if count(d.oldData) != count(d.newData) {
				return true
			}
		}
		if lf.grepDiff != nil {
			for _, op := range d.ops {
				if op.kind != ' ' && lf.grepDiff.MatchString(op.line) {
					return true
				}
			}
		}
	}
	return false
}

// approxidate parses the dates git accepts for --since and --until.
func approxidate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	now := time.Now()
	switch s {
	case "now":
		return now, nil
	case "yesterday":
		return now.AddDate(0, 0, -1), nil
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	}
	if t, err := parseDate(s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	f := strings.Fields(strings.ReplaceAll(s, ".", " "))
	if len(f) >= 2 {
		n, err := strconv.Atoi(f[0])
		if err == nil {
			unit := strings.TrimSuffix(f[1], "s")
			switch unit {
			case "second":
				return now.Add(-time.Duration(n) * time.Second), nil
			case "minute":
				return now.Add(-time.Duration(n) * time.Minute), nil
			case "hour":
				return now.Add(-time.Duration(n) * time.Hour), nil
			case "day":
				return now.AddDate(0, 0, -n), nil
			case "week":
				return now.AddDate(0, 0, -7*n), nil
			case "month":
				return now.AddDate(0, -n, 0), nil
			case "year":
				return now.AddDate(-n, 0, 0), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q", s)
}

// formatDate renders t in one of git's --date modes.
func formatDate(t time.Time, mode string) string {
	switch {
	case mode == "" || mode == "default" || mode == "local" || mode == "human":
		return gitDate(t)
	case mode == "iso" || mode == "iso8601":
		return t.Format("2006-01-02 15:04:05 -0700")
	case mode == "iso-strict" || mode == "iso8601-strict":
		return t.Format("2006-01-02T15:04:05Z07:00")
	case mode == "rfc" || mode == "rfc2822":
		return t.Format("Mon, 2 Jan 2006 15:04:05 -0700")
	case mode == "short":
		return t.Format("2006-01-02")
	case mode == "raw":
		return fmt.Sprintf("%d %s", t.Unix(), t.Format("-0700"))
	case mode == "unix":
		return strconv.FormatInt(t.Unix(), 10)
	case mode == "relative":
		return relativeDate(t)
	case strings.HasPrefix(mode, "format:"):
		return strftime(t, strings.TrimPrefix(mode, "format:"))
	}
	return gitDate(t)
}

func relativeDate(t time.Time) string {
	d := time.Since(t)
	secs := int64(d / time.Second)
	unit := func(n int64, name string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s ago", n, name)
		}
		return fmt.Sprintf("%d %ss ago", n, name)
	}
	switch {
	case secs < 0:
		return "in the future"
	case secs < 90:
		return unit(secs, "second")
	case secs < 90*60:
		return unit((secs+30)/60, "minute")
	case secs < 36*3600:
		return unit((secs+1800)/3600, "hour")
	case secs < 14*86400:
		return unit((secs+43200)/86400, "day")
	case secs < 70*86400:
		return unit((secs+302400)/604800, "week")
	case secs < 365*86400:
		return unit((secs+1296000)/2592000, "month")
	}
	return unit((secs+15768000)/31536000, "year")
}

// strftime implements the common conversions of --date=format:.
func strftime(t time.Time, format string) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 == len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		switch format[i] {
		case 'Y':
			b.WriteString(t.Format("2006"))
		case 'y':
			b.WriteString(t.Format("06"))
		case 'm':
			b.WriteString(t.Format("01"))
		case 'd':
			b.WriteString(t.Format("02"))
		case 'e':
			fmt.Fprintf(&b, "%2d", t.Day())
		case 'H':
			b.WriteString(t.Format("15"))
		case 'I':
			b.WriteString(t.Format("03"))
		case 'M':
			b.WriteString(t.Format("04"))
		case 'S':
			b.WriteString(t.Format("05"))
		case 'p':
			b.WriteString(t.Format("PM"))
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'A':
			b.WriteString(t.Format("Monday"))
		case 'b', 'h':
			b.WriteString(t.Format("Jan"))
		case 'B':
			b.WriteString(t.Format("January"))
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'z':
			b.WriteString(t.Format("-0700"))
		case 'Z':
			b.WriteString(t.Format("MST"))
		case 's':
			fmt.Fprintf(&b, "%d", t.Unix())
		case 'F':
			b.WriteString(t.Format("2006-01-02"))
		case 'T':
			b.WriteString(t.Format("15:04:05"))
		case 'R':
			b.WriteString(t.Format("15:04"))
		case 'D':
			b.WriteString(t.Format("01/02/06"))
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

// sanitizedSubject is %f: the subject with runs of characters other than
// letters, digits, '.', and '_' replaced by '-'.
func sanitizedSubject(s string) string {
	var b strings.Builder
	dash := false
	for _, c := range []byte(s) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_'
		if !ok {
			dash = b.Len() > 0
			continue
		}
		if dash {
			b.WriteByte('-')
			dash = false
		}
		b.WriteByte(c)
	}
	out := b.String()
	for strings.HasSuffix(out, ".") {
		out = strings.TrimSuffix(out, ".")
	}
	return out
}

// body returns a commit message without its subject paragraph.
func body(msg string) string {
	if _, rest, ok := strings.Cut(strings.TrimLeft(msg, "\n"), "\n\n"); ok {
		return strings.TrimLeft(rest, "\n")
	}
	return ""
}

// indentMessage indents each message line by four spaces, as git's
// medium, full, fuller, and raw formats do.
func indentMessage(msg string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}
