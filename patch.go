package git

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// format-patch, apply, and am: the email patch workflow.

const gitVersion = "2.54.0 (go-git)"

func (g *gitRun) formatPatch(args []string) error {
	var toStdout, numbered, noNumbered bool
	outDir := ""
	signature := gitVersion
	count := -1
	prefix := "PATCH"
	var revs []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--stdout":
			toStdout = true
		case a == "-n" || a == "--numbered":
			numbered = true
		case a == "-N" || a == "--no-numbered":
			noNumbered = true
		case a == "--no-signature":
			signature = ""
		case strings.HasPrefix(a, "--signature="):
			signature = strings.TrimPrefix(a, "--signature=")
		case strings.HasPrefix(a, "--subject-prefix="):
			prefix = strings.TrimPrefix(a, "--subject-prefix=")
		case (a == "-o" || a == "--output-directory") && i+1 < len(args):
			i++
			outDir = args[i]
		case len(a) > 1 && a[0] == '-' && isDigits(a[1:]):
			count, _ = strconv.Atoi(a[1:])
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			revs = append(revs, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	f := newLogFormat()
	f.filter.maxParents = 1
	switch {
	case count >= 0:
		f.count = count
		f.revs = revs
		if len(f.revs) == 0 {
			f.revs = []string{"HEAD"}
		}
	case len(revs) == 1 && !strings.Contains(revs[0], ".."):
		f.revs = []string{revs[0] + "..HEAD"}
	default:
		f.revs = revs
	}
	commits, err := r.walk(f, nil)
	if err != nil {
		return err
	}
	commits = topoOrder(commits)
	for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
		commits[i], commits[j] = commits[j], commits[i]
	}
	total := len(commits)
	for n, c := range commits {
		subj := subject(c.Message)
		tag := "[" + prefix + "]"
		if (total > 1 || numbered) && !noNumbered {
			tag = fmt.Sprintf("[%s %d/%d]", prefix, n+1, total)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "From %s Mon Sep 17 00:00:00 2001\nFrom: %s\nDate: %s\nSubject: %s %s\n\n",
			c.Hash, ident(c.Author), formatDate(c.Author.When, "rfc"), tag, subj)
		b.WriteString(body(c.Message))
		b.WriteString("---\n")
		diffs, err := r.commitDiffs(c, nil)
		if err != nil {
			return err
		}
		writeStat(&b, diffs)
		writeModeSummary(&b, diffs)
		b.WriteString("\n")
		for _, d := range diffs {
			d.writePatch(&b)
		}
		if signature != "" {
			fmt.Fprintf(&b, "-- \n%s\n\n", signature)
		}
		if toStdout {
			fmt.Fprint(g.out, b.String())
			continue
		}
		name := sanitizedSubject(subj)
		if len(name) > 52 {
			name = strings.TrimRight(name[:52], "-.")
		}
		file := fmt.Sprintf("%04d-%s.patch", n+1, name)
		if outDir != "" {
			if err := newBillyFS(g.fsys, ".").MkdirAll(fsName(g.abs(outDir)), 0777); err != nil {
				return err
			}
			file = path.Join(outDir, file)
		}
		if err := writeFSFile(g.fsys, fsName(g.abs(file)), []byte(b.String())); err != nil {
			return err
		}
		fmt.Fprintln(g.out, file)
	}
	return nil
}

func writeFSFile(fsys FS, name string, data []byte) error {
	f, err := fsys.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Parsing unified diffs.

type patchFile struct {
	oldPath, newPath string // "" for /dev/null
	oldMode, newMode filemode.FileMode
	newFile, deleted bool
	rename           bool
	binary           bool
	hunks            []patchHunk
}

type patchHunk struct {
	oldStart, oldLines, newStart, newLines int
	lines                                  []string // with ' ', '-', '+' prefixes; "\n" kept
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// unquote reverses git's C-style path quoting.
func unquote(s string) string {
	if !strings.HasPrefix(s, "\"") || !strings.HasSuffix(s, "\"") || len(s) < 2 {
		return s
	}
	s = s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch c := s[i]; c {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'v':
			b.WriteByte('\v')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		default:
			if c >= '0' && c <= '7' && i+2 < len(s) {
				v, err := strconv.ParseUint(s[i:i+3], 8, 8)
				if err == nil {
					b.WriteByte(byte(v))
					i += 2
					continue
				}
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

func stripPrefix(p string, n int) string {
	p = unquote(p)
	if p == "/dev/null" {
		return ""
	}
	for ; n > 0; n-- {
		_, rest, ok := strings.Cut(p, "/")
		if !ok {
			break
		}
		p = rest
	}
	return p
}

// parsePatch reads the file patches of a unified diff.
func parsePatch(data []byte, strip int) ([]*patchFile, error) {
	lines := splitLines(data)
	var out []*patchFile
	var cur *patchFile
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\n")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			cur = &patchFile{}
			out = append(out, cur)
			rest := strings.TrimPrefix(line, "diff --git ")
			if a, b, ok := splitGitPaths(rest); ok {
				cur.oldPath, cur.newPath = stripPrefix(a, strip), stripPrefix(b, strip)
			}
		case cur == nil && strings.HasPrefix(line, "--- "):
			cur = &patchFile{}
			out = append(out, cur)
			i--
			continue
		case cur == nil:
		case strings.HasPrefix(line, "new file mode "):
			cur.newFile = true
			cur.newMode = parseMode(strings.TrimPrefix(line, "new file mode "))
			cur.oldPath = ""
		case strings.HasPrefix(line, "deleted file mode "):
			cur.deleted = true
			cur.oldMode = parseMode(strings.TrimPrefix(line, "deleted file mode "))
			cur.newPath = ""
		case strings.HasPrefix(line, "old mode "):
			cur.oldMode = parseMode(strings.TrimPrefix(line, "old mode "))
		case strings.HasPrefix(line, "new mode "):
			cur.newMode = parseMode(strings.TrimPrefix(line, "new mode "))
		case strings.HasPrefix(line, "rename from "):
			cur.rename, cur.oldPath = true, unquote(strings.TrimPrefix(line, "rename from "))
		case strings.HasPrefix(line, "rename to "):
			cur.rename, cur.newPath = true, unquote(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "index "):
			if f := strings.Fields(line); len(f) == 3 {
				m := parseMode(f[2])
				cur.oldMode, cur.newMode = m, m
			}
		case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
			cur.binary = true
		case strings.HasPrefix(line, "--- "):
			if p := stripPrefix(strings.TrimSpace(strings.TrimPrefix(line, "--- ")), strip); p != "" || cur.oldPath == "" {
				cur.oldPath = p
			}
			if p := stripPrefix(strings.TrimSpace(strings.TrimPrefix(line, "--- ")), strip); p == "" {
				cur.newFile = true
			}
		case strings.HasPrefix(line, "+++ "):
			p := stripPrefix(strings.TrimSpace(strings.TrimPrefix(line, "+++ ")), strip)
			if p == "" {
				cur.deleted = true
			} else {
				cur.newPath = p
			}
		case strings.HasPrefix(line, "@@ "):
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("corrupt patch at line %d", i+1)
			}
			atoi := func(s string, def int) int {
				if s == "" {
					return def
				}
				n, _ := strconv.Atoi(s)
				return n
			}
			h := patchHunk{oldStart: atoi(m[1], 0), oldLines: atoi(m[2], 1), newStart: atoi(m[3], 0), newLines: atoi(m[4], 1)}
			oldLeft, newLeft := h.oldLines, h.newLines
			for oldLeft > 0 || newLeft > 0 {
				i++
				if i >= len(lines) {
					return nil, fmt.Errorf("corrupt patch at line %d", i+1)
				}
				l := lines[i]
				if l == "\n" {
					l = " \n" // blank context lines are sometimes stripped
				}
				switch l[0] {
				case ' ':
					oldLeft--
					newLeft--
				case '-':
					oldLeft--
				case '+':
					newLeft--
				case '\\':
					continue
				default:
					return nil, fmt.Errorf("corrupt patch at line %d", i+1)
				}
				h.lines = append(h.lines, l)
				// "\ No newline at end of file" applies to the line before it.
				if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "\\") {
					h.lines[len(h.lines)-1] = strings.TrimSuffix(h.lines[len(h.lines)-1], "\n")
					i++
				}
			}
			cur.hunks = append(cur.hunks, h)
		}
	}
	for _, f := range out {
		if f.newFile {
			f.oldPath = ""
		}
		if f.deleted {
			f.newPath = ""
		}
	}
	return out, nil
}

// splitGitPaths splits "a/x b/y" from a diff --git line, honoring quotes.
func splitGitPaths(s string) (string, string, bool) {
	if strings.HasPrefix(s, "\"") {
		end := strings.Index(s[1:], "\"")
		for end >= 0 && s[end] == '\\' {
			next := strings.Index(s[end+2:], "\"")
			if next < 0 {
				return "", "", false
			}
			end += next + 1
		}
		if end < 0 {
			return "", "", false
		}
		return s[:end+2], strings.TrimSpace(s[end+2:]), true
	}
	// Unquoted: the paths are equal lengths when unchanged; otherwise
	// split at " b/".
	if i := strings.Index(s, " b/"); i >= 0 {
		return s[:i], s[i+1:], true
	}
	a, b, ok := strings.Cut(s, " ")
	return a, b, ok
}

func parseMode(s string) filemode.FileMode {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 8, 32)
	if err != nil {
		return filemode.Regular
	}
	return filemode.FileMode(v)
}

func (p *patchFile) reverse() {
	p.oldPath, p.newPath = p.newPath, p.oldPath
	p.oldMode, p.newMode = p.newMode, p.oldMode
	p.newFile, p.deleted = p.deleted, p.newFile
	for i := range p.hunks {
		h := &p.hunks[i]
		h.oldStart, h.newStart = h.newStart, h.oldStart
		h.oldLines, h.newLines = h.newLines, h.oldLines
		for j, l := range h.lines {
			switch l[0] {
			case '-':
				h.lines[j] = "+" + l[1:]
			case '+':
				h.lines[j] = "-" + l[1:]
			}
		}
	}
}

func (p *patchFile) displayPath() string {
	if p.newPath != "" {
		return p.newPath
	}
	return p.oldPath
}

// applyHunks applies the hunks to content, searching for each hunk's
// context around its expected position as git apply does (no fuzz).
func (p *patchFile) applyHunks(content []byte) ([]byte, error) {
	lines := splitLines(content)
	var out []string
	pos, offset := 0, 0
	for _, h := range p.hunks {
		var pre, post []string
		for _, l := range h.lines {
			if l[0] != '+' {
				pre = append(pre, l[1:])
			}
			if l[0] != '-' {
				post = append(post, l[1:])
			}
		}
		want := h.oldStart - 1 + offset
		if h.oldLines == 0 {
			want = h.oldStart + offset
		}
		at := -1
		for d := 0; d <= len(lines); d++ {
			for _, cand := range []int{want - d, want + d} {
				if cand < pos || cand+len(pre) > len(lines) {
					continue
				}
				if equalLines(lines, cand, pre, 0, len(pre)) {
					at = cand
					break
				}
			}
			if at >= 0 {
				break
			}
		}
		if at < 0 {
			return nil, errPatchFailed
		}
		out = append(out, lines[pos:at]...)
		out = append(out, post...)
		pos = at + len(pre)
		offset += len(post) - len(pre) + (at - want)
	}
	out = append(out, lines[pos:]...)
	return []byte(strings.Join(out, "")), nil
}

var errPatchFailed = errors.New("patch does not apply")

type applyOptions struct {
	check, index, cached, reverse bool
	strip                         int
}

// applyPatches applies parsed patches to the worktree and/or index. It
// returns git apply's error text on failure, changing nothing.
func (r *repo) applyPatches(files []*patchFile, o applyOptions) (string, error) {
	idx, err := r.readIndex()
	if err != nil {
		return "", err
	}
	indexSide := r.indexSide(idx)
	type result struct {
		p    *patchFile
		data []byte
	}
	var results []result
	var errs strings.Builder
	for _, p := range files {
		if o.reverse {
			p.reverse()
		}
		var old []byte
		if p.oldPath != "" {
			switch {
			case o.cached:
				e, ok := indexSide[p.oldPath]
				if !ok {
					fmt.Fprintf(&errs, "error: %s: does not exist in index\n", p.oldPath)
					continue
				}
				old, _ = e.data()
			default:
				data, err := fs.ReadFile(r.g.fsys, fsName(path.Join(r.top, p.oldPath)))
				if err != nil {
					fmt.Fprintf(&errs, "error: %s: No such file or directory\n", p.oldPath)
					continue
				}
				old = data
				if o.index {
					if e, ok := indexSide[p.oldPath]; !ok || e.hash != plumbing.ComputeHash(plumbing.BlobObject, data) {
						fmt.Fprintf(&errs, "error: %s: does not match index\n", p.oldPath)
						continue
					}
				}
			}
		} else if p.newPath != "" {
			_, statErr := r.wt.Stat(p.newPath)
			if _, inIndex := indexSide[p.newPath]; (!o.cached && statErr == nil) || (o.cached && inIndex) {
				where := "working directory"
				if o.cached {
					where = "index"
				}
				fmt.Fprintf(&errs, "error: %s: already exists in %s\n", p.newPath, where)
				continue
			}
		}
		if p.binary {
			fmt.Fprintf(&errs, "error: cannot apply binary patch to '%s' without full index line\nerror: %s: patch does not apply\n", p.displayPath(), p.displayPath())
			continue
		}
		data, err := p.applyHunks(old)
		if err != nil {
			line := 0
			if len(p.hunks) > 0 {
				line = p.hunks[0].oldStart
			}
			fmt.Fprintf(&errs, "error: patch failed: %s:%d\nerror: %s: patch does not apply\n", p.oldPath, line, p.oldPath)
			continue
		}
		results = append(results, result{p, data})
	}
	if errs.Len() > 0 {
		return errs.String(), nil
	}
	if o.check {
		return "", nil
	}
	for _, res := range results {
		p := res.p
		if p.oldPath != "" && (p.newPath == "" || p.oldPath != p.newPath) {
			if !o.cached {
				r.removeFile(p.oldPath)
			}
			if o.index || o.cached {
				removeEntry(idx, p.oldPath)
			}
		}
		if p.newPath == "" {
			continue
		}
		mode := p.newMode
		if mode == 0 {
			mode = filemode.Regular
		}
		data := res.data
		e := entry{hash: plumbing.ComputeHash(plumbing.BlobObject, data), mode: mode, data: func() ([]byte, error) { return data, nil }}
		var info fs.FileInfo
		if !o.cached {
			if info, err = r.writeFile(p.newPath, e); err != nil {
				return "", err
			}
		}
		if o.index || o.cached {
			if _, err := r.writeBlob(data, mode); err != nil {
				return "", err
			}
			setEntry(idx, p.newPath, e, info)
		}
	}
	if o.index || o.cached {
		return "", r.writeIndex(idx)
	}
	return "", nil
}

func (g *gitRun) apply(args []string) error {
	var o applyOptions
	var stat, numstat, summary bool
	o.strip = 1
	var files []string
	for _, a := range args {
		switch {
		case a == "--check":
			o.check = true
		case a == "--index":
			o.index = true
		case a == "--cached":
			o.cached = true
		case a == "-R" || a == "--reverse":
			o.reverse = true
		case a == "--stat":
			stat = true
		case a == "--numstat":
			numstat = true
		case a == "--summary":
			summary = true
		case strings.HasPrefix(a, "-p"):
			o.strip, _ = strconv.Atoi(strings.TrimPrefix(a, "-p"))
		case a == "-v" || a == "--verbose" || a == "--apply":
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			files = append(files, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	var data []byte
	if len(files) == 0 {
		if data, err = io.ReadAll(g.stdin); err != nil {
			return err
		}
	}
	for _, f := range files {
		d, err := fs.ReadFile(g.fsys, fsName(g.abs(f)))
		if err != nil {
			return fatalf("can't open patch '%s': No such file or directory", f)
		}
		data = append(data, d...)
	}
	patches, err := parsePatch(data, o.strip)
	if err != nil {
		return fatalf("%v", err)
	}
	if len(patches) == 0 {
		return failf(128, "error: No valid patches in input (allow with \"--allow-empty\")\n")
	}
	if o.reverse {
		// Undoing a patch series goes backwards through it.
		for i, j := 0, len(patches)-1; i < j; i, j = i+1, j-1 {
			patches[i], patches[j] = patches[j], patches[i]
		}
	}
	if stat || numstat || summary {
		diffs := make([]*fileDiff, 0, len(patches))
		for _, p := range patches {
			if o.reverse {
				p.reverse()
			}
			d := &fileDiff{change: change{path: p.displayPath()}}
			if p.oldPath != "" {
				d.from = &entry{mode: p.oldMode}
			}
			if p.newPath != "" {
				d.to = &entry{mode: p.newMode}
			}
			for _, h := range p.hunks {
				for _, l := range h.lines {
					switch l[0] {
					case '+':
						d.added++
					case '-':
						d.deleted++
					}
				}
			}
			diffs = append(diffs, d)
		}
		switch {
		case numstat:
			for _, d := range diffs {
				fmt.Fprintf(g.out, "%d\t%d\t%s\n", d.added, d.deleted, q(d.path))
			}
		case stat:
			writeStatWidth(g.out, diffs, 4)
		}
		if summary {
			writeModeSummary(g.out, diffs)
		}
		return nil
	}
	msg, err := r.applyPatches(patches, o)
	if err != nil {
		return err
	}
	if msg != "" {
		return failf(1, "%s", msg)
	}
	return nil
}

// am: applying mailbox patches.

type mailPatch struct {
	author  object.Signature
	subject string
	message string
	diff    []byte
	raw     []byte
}

var patchPrefix = regexp.MustCompile(`^\[[^\]]*\]\s*`)

// splitMbox splits format-patch output into messages.
func splitMbox(data []byte) []mailPatch {
	var out []mailPatch
	var chunks [][]byte
	var cur bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "From ") && strings.HasSuffix(line, " 2001") && cur.Len() > 0 {
			chunks = append(chunks, bytes.Clone(cur.Bytes()))
			cur.Reset()
		}
		cur.WriteString(line + "\n")
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.Bytes())
	}
	for _, c := range chunks {
		out = append(out, parseMail(c))
	}
	return out
}

func parseMail(data []byte) mailPatch {
	m := mailPatch{raw: data}
	lines := strings.SplitAfter(string(data), "\n")
	i := 0
	var headerKey string
	headers := map[string]string{}
	for ; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], "\n")
		if l == "" {
			i++
			break
		}
		if strings.HasPrefix(l, "From ") && i == 0 {
			continue
		}
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && headerKey != "" {
			headers[headerKey] += " " + strings.TrimSpace(l)
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if ok {
			headerKey = strings.ToLower(k)
			headers[headerKey] = strings.TrimSpace(v)
		}
	}
	m.subject = patchPrefix.ReplaceAllString(headers["subject"], "")
	if name, email, ok := strings.Cut(headers["from"], " <"); ok {
		m.author.Name = strings.Trim(name, "\"")
		m.author.Email = strings.TrimSuffix(email, ">")
	}
	if t, err := parseDate(headers["date"]); err == nil {
		m.author.When = t
	}
	var body strings.Builder
	for ; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimRight(l, "\n") == "---" || strings.HasPrefix(l, "diff --git ") {
			if strings.TrimRight(l, "\n") == "---" {
				i++
			}
			break
		}
		body.WriteString(l)
	}
	m.diff = []byte(strings.Join(lines[i:], ""))
	m.message = cleanupMessage(m.subject + "\n\n" + body.String())
	return m
}

const amDir = "rebase-apply"

func (g *gitRun) am(args []string) error {
	var cont, skip, abort, quiet bool
	var files []string
	for _, a := range args {
		switch {
		case a == "--continue" || a == "-r" || a == "--resolved":
			cont = true
		case a == "--skip":
			skip = true
		case a == "--abort" || a == "--quit":
			abort = true
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "-3" || a == "--3way" || a == "-s" || a == "--signoff" || a == "-k":
		case strings.HasPrefix(a, "--show-current-patch"):
			r, err := g.openRepo()
			if err != nil {
				return err
			}
			s, ok := r.gitFile(path.Join(amDir, "patch"))
			if !ok {
				return fatalf("Resolve operation not in progress, we are not resuming.")
			}
			fmt.Fprint(g.out, s)
			return nil
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			files = append(files, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	queue, inSession := r.gitFile(path.Join(amDir, "queue"))
	switch {
	case cont || skip || abort:
		if !inSession {
			return fatalf("Resolve operation not in progress, we are not resuming.")
		}
		if abort {
			orig, _ := r.gitFile(path.Join(amDir, "orig-head"))
			c, err := r.CommitObject(plumbing.NewHash(strings.TrimSpace(orig)))
			if err != nil {
				return err
			}
			if err := r.resetHard(c); err != nil {
				return err
			}
			if err := r.setHead(c.Hash, "am --abort"); err != nil {
				return err
			}
			r.clearAm()
			return nil
		}
		mails := splitMbox([]byte(queue))
		if skip {
			head, err := r.headCommit()
			if err != nil {
				return err
			}
			if err := r.resetHard(head); err != nil {
				return err
			}
		} else if err := r.commitMail(mails[0], quiet, true); err != nil {
			return err
		}
		return r.runAm(mails[1:], quiet)
	case inSession:
		return fatalf("previous rebase directory .git/rebase-apply still exists but mbox given.")
	}
	var data []byte
	if len(files) == 0 {
		if data, err = io.ReadAll(g.stdin); err != nil {
			return err
		}
	}
	for _, f := range files {
		d, err := fs.ReadFile(g.fsys, fsName(g.abs(f)))
		if err != nil {
			return fatalf("could not open '%s' for reading: No such file or directory", f)
		}
		data = append(data, d...)
	}
	mails := splitMbox(data)
	if len(mails) == 0 {
		return fatalf("Patch format detection failed.")
	}
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	if head != nil {
		r.writeGitFile(origHeadFile, head.Hash.String()+"\n")
	}
	return r.runAm(mails, quiet)
}

func (r *repo) clearAm() {
	d := newBillyFS(r.g.fsys, fsName(r.gitDir))
	infos, _ := d.ReadDir(amDir)
	for _, info := range infos {
		d.Remove(path.Join(amDir, info.Name()))
	}
	d.Remove(amDir)
}

// runAm applies mails in order. On failure it records the session in
// .git/rebase-apply for --continue, --skip, and --abort.
func (r *repo) runAm(mails []mailPatch, quiet bool) error {
	g := r.g
	var out strings.Builder // git's buffered stdout lands after errors
	defer func() { fmt.Fprint(g.out, out.String()) }()
	for i, m := range mails {
		if !quiet {
			fmt.Fprintf(&out, "Applying: %s\n", m.subject)
		}
		patches, err := parsePatch(m.diff, 1)
		if err != nil {
			return fatalf("%v", err)
		}
		msg, err := r.applyPatches(patches, applyOptions{index: true})
		if err != nil {
			return err
		}
		if msg != "" {
			var queue strings.Builder
			for _, rest := range mails[i:] {
				queue.Write(rest.raw)
			}
			newBillyFS(g.fsys, fsName(r.gitDir)).MkdirAll(amDir, 0777)
			r.writeGitFile(path.Join(amDir, "queue"), queue.String())
			r.writeGitFile(path.Join(amDir, "patch"), string(m.diff))
			if orig, ok := r.gitFile(origHeadFile); ok {
				r.writeGitFile(path.Join(amDir, "orig-head"), orig)
			}
			fmt.Fprintf(&out, "Patch failed at %04d %s\n", i+1, m.subject)
			fmt.Fprint(g.err, msg)
			fmt.Fprint(g.err, "hint: Use 'git am --show-current-patch=diff' to see the failed patch\n"+
				"hint: When you have resolved this problem, run \"git am --continue\".\n"+
				"hint: If you prefer to skip this patch, run \"git am --skip\" instead.\n"+
				"hint: To restore the original branch and stop patching, run \"git am --abort\".\n"+
				"hint: Disable this message with \"git config set advice.mergeConflict false\"\n")
			return &exitError{code: 128}
		}
		if err := r.commitMail(m, quiet, false); err != nil {
			return err
		}
	}
	r.clearAm()
	return nil
}

// commitMail commits the index with the mail's author and message.
func (r *repo) commitMail(m mailPatch, quiet, resumed bool) error {
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	var parents []plumbing.Hash
	if head != nil {
		parents = []plumbing.Hash{head.Hash}
	}
	committer, err := r.signature("COMMITTER")
	if err != nil {
		return err
	}
	h, err := r.commitIndex(m.message, &m.author, committer, parents)
	if err != nil {
		return err
	}
	if resumed && !quiet {
		fmt.Fprintf(r.g.out, "Applying: %s\n", m.subject)
	}
	return r.setHead(h, "am: "+m.subject)
}

// writeStatWidth is writeStat with a minimum count column width, which
// git apply --stat uses.
func writeStatWidth(w io.Writer, diffs []*fileDiff, minNumber int) {
	var b strings.Builder
	writeStat(&b, diffs)
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	for i, l := range lines {
		if i == len(lines)-1 {
			fmt.Fprintln(w, l)
			continue
		}
		name, rest, ok := strings.Cut(l, " | ")
		if !ok {
			fmt.Fprintln(w, l)
			continue
		}
		num, graph, _ := strings.Cut(strings.TrimLeft(rest, " "), " ")
		fmt.Fprintf(w, "%s | %*s", name, minNumber, num)
		if graph != "" {
			fmt.Fprintf(w, " %s", graph)
		}
		fmt.Fprintln(w)
	}
}
