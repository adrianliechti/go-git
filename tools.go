package git

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// describe, shortlog, grep, and blame.

func (g *gitRun) describe(args []string) error {
	var useTags, all, long, always, exact bool
	dirty := ""
	abbrev := 7
	match := ""
	var revs []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--tags":
			useTags = true
		case a == "--all":
			all = true
		case a == "--long":
			long = true
		case a == "--always":
			always = true
		case a == "--exact-match":
			exact = true
		case a == "--dirty":
			dirty = "-dirty"
		case strings.HasPrefix(a, "--dirty="):
			dirty = strings.TrimPrefix(a, "--dirty=")
		case strings.HasPrefix(a, "--abbrev="):
			abbrev, _ = strconv.Atoi(strings.TrimPrefix(a, "--abbrev="))
		case strings.HasPrefix(a, "--match="):
			match = strings.TrimPrefix(a, "--match=")
		case a == "--match" && i+1 < len(args):
			i++
			match = args[i]
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			revs = append(revs, a)
		}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	if len(revs) == 0 {
		revs = []string{"HEAD"}
	} else if dirty != "" {
		return fatalf("option '--dirty' and commit-ishes cannot be used together")
	}
	for _, rev := range revs {
		c, err := r.resolveCommit(rev)
		if err != nil {
			return fatalf("Not a valid object name %s", rev)
		}
		name, err := r.describeCommit(c, useTags, all, long, exact, abbrev, match)
		if err != nil {
			if always {
				name = c.Hash.String()[:max(abbrev, 4)]
			} else {
				return err
			}
		}
		if dirty != "" && r.isDirty() {
			name += dirty
		}
		fmt.Fprintln(g.out, name)
	}
	return nil
}

func (r *repo) isDirty() bool {
	if r.bare() {
		return false
	}
	st, err := r.computeStatus(nil, false, false)
	return err == nil && (st.hasStaged() || st.hasUnstaged() || st.hasUnmerged())
}

func (r *repo) describeCommit(c *object.Commit, useTags, all, long, exact bool, abbrev int, match string) (string, error) {
	type candidate struct {
		name  string
		depth int
		when  int64
	}
	var cands []candidate
	unannotated := false
	reach := r.ancestors(c.Hash)
	for _, name := range r.sortedRefs() {
		label := ""
		switch {
		case name.IsTag():
			label = name.Short()
			if all {
				label = "tags/" + label
			}
		case all && name.IsBranch():
			label = "heads/" + name.Short()
		case all && name.IsRemote():
			label = "remotes/" + strings.TrimPrefix(name.String(), "refs/remotes/")
		default:
			continue
		}
		if match != "" {
			if ok, _ := pathMatch(match, name.Short()); !ok {
				continue
			}
		}
		h := r.refHash(name)
		var when int64
		if t, err := r.TagObject(h); err == nil {
			when = t.Tagger.When.Unix()
			h = t.Target
		} else if name.IsTag() && !useTags && !all {
			unannotated = unannotated || reach[h]
			continue
		}
		if !reach[h] {
			continue
		}
		depth := 0
		for x := range reach {
			if !r.ancestors(h)[x] {
				depth++
			}
		}
		if when == 0 {
			if tc, err := r.CommitObject(h); err == nil {
				when = tc.Committer.When.Unix()
			}
		}
		cands = append(cands, candidate{label, depth, when})
	}
	if len(cands) == 0 {
		if unannotated {
			return "", fatalf("No annotated tags can describe '%s'.\nHowever, there were unannotated tags: try --tags.", c.Hash)
		}
		return "", fatalf("No names found, cannot describe anything.")
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].depth != cands[j].depth {
			return cands[i].depth < cands[j].depth
		}
		return cands[i].when > cands[j].when
	})
	best := cands[0]
	if best.depth == 0 && !long {
		return best.name, nil
	}
	if exact {
		return "", fatalf("no tag exactly matches '%s'", c.Hash)
	}
	return fmt.Sprintf("%s-%d-g%s", best.name, best.depth, c.Hash.String()[:max(abbrev, 4)]), nil
}

func pathMatch(pattern, name string) (bool, error) {
	re, err := globRegexp(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(name), nil
}

// globRegexp converts a shell glob to an anchored regular expression.
func globRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for _, c := range glob {
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func (g *gitRun) shortlog(args []string) error {
	var summary, numbered, email, committer bool
	var rest []string
	for _, a := range args {
		switch {
		case a == "-s" || a == "--summary":
			summary = true
		case a == "-n" || a == "--numbered":
			numbered = true
		case a == "-e" || a == "--email":
			email = true
		case a == "-c" || a == "--committer":
			committer = true
		case len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.Trim(a[1:], "sne") == "":
			summary = summary || strings.Contains(a, "s")
			numbered = numbered || strings.Contains(a, "n")
			email = email || strings.Contains(a, "e")
		default:
			rest = append(rest, a)
		}
	}
	f, err := parseLogArgs(rest)
	if err != nil {
		return err
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	specs, err := r.pathspecs(f.paths)
	if err != nil {
		return err
	}
	commits, err := r.walk(f, specs)
	if err != nil {
		return err
	}
	groups := map[string][]string{}
	for _, c := range commits {
		sig := c.Author
		if committer {
			sig = c.Committer
		}
		key := sig.Name
		if email {
			key = ident(sig)
		}
		groups[key] = append(groups[key], subject(c.Message))
	}
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	if numbered {
		sort.SliceStable(names, func(i, j int) bool { return len(groups[names[i]]) > len(groups[names[j]]) })
	}
	for _, n := range names {
		subjects := groups[n]
		if summary {
			fmt.Fprintf(g.out, "%6d\t%s\n", len(subjects), n)
			continue
		}
		fmt.Fprintf(g.out, "%s (%d):\n", n, len(subjects))
		// Oldest first, as git lists them.
		for i := len(subjects) - 1; i >= 0; i-- {
			fmt.Fprintf(g.out, "      %s\n", subjects[i])
		}
		fmt.Fprintln(g.out)
	}
	return nil
}

// breToRE2 converts a POSIX basic regular expression (git grep's default)
// to Go syntax: \( \) \{ \} \| \+ \? are special, bare ones are literal.
func breToRE2(bre string) string {
	var b strings.Builder
	for i := 0; i < len(bre); i++ {
		c := bre[i]
		if c == '\\' && i+1 < len(bre) {
			n := bre[i+1]
			i++
			if strings.IndexByte("(){}|+?", n) >= 0 {
				b.WriteByte(n)
			} else {
				b.WriteByte('\\')
				b.WriteByte(n)
			}
			continue
		}
		if strings.IndexByte("(){}|+?", c) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (g *gitRun) grep(args []string) error {
	var lineNumbers, ignoreCase, countOnly, filesOnly, filesWithout, word, invert, noFilename, quiet, cached, extended, fixed, onlyMatching bool
	var patterns, rest, paths []string
	args = splitFlags(args, "nicLlwvhHqEFGPo", "e")
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			paths = append(paths, args[i+1:]...)
			i = len(args)
		case a == "-n" || a == "--line-number":
			lineNumbers = true
		case a == "-i" || a == "--ignore-case":
			ignoreCase = true
		case a == "-c" || a == "--count":
			countOnly = true
		case a == "-l" || a == "--files-with-matches" || a == "--name-only":
			filesOnly = true
		case a == "-L" || a == "--files-without-match":
			filesWithout = true
		case a == "-w" || a == "--word-regexp":
			word = true
		case a == "-v" || a == "--invert-match":
			invert = true
		case a == "-h":
			noFilename = true
		case a == "-H":
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--cached":
			cached = true
		case a == "-E" || a == "--extended-regexp" || a == "-P" || a == "--perl-regexp":
			extended = true
		case a == "-F" || a == "--fixed-strings":
			fixed = true
		case a == "-G" || a == "--basic-regexp":
			extended, fixed = false, false
		case a == "-o" || a == "--only-matching":
			onlyMatching = true
		case a == "--or" || a == "--no-color" || a == "--full-name":
		case a == "-e" && i+1 < len(args):
			i++
			patterns = append(patterns, args[i])
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	if len(patterns) == 0 {
		if len(rest) == 0 {
			return fatalf("no pattern given")
		}
		patterns, rest = rest[:1], rest[1:]
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	// Remaining arguments are revisions until one is not.
	var trees []string
	for len(rest) > 0 {
		if _, err := r.resolveCommit(rest[0]); err != nil {
			break
		}
		trees, rest = append(trees, rest[0]), rest[1:]
	}
	paths = append(rest, paths...)
	specs, err := r.pathspecs(paths)
	if err != nil {
		return err
	}
	var exprs []string
	for _, p := range patterns {
		switch {
		case fixed:
			p = regexp.QuoteMeta(p)
		case !extended:
			p = breToRE2(p)
		}
		if word {
			p = `\b(?:` + p + `)\b`
		}
		exprs = append(exprs, "(?:"+p+")")
	}
	expr := strings.Join(exprs, "|")
	if ignoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return fatalf("command line, '%s': Invalid regular expression", patterns[0])
	}
	type source struct {
		label string // prefix before the path, e.g. "HEAD:"
		files side
	}
	var sources []source
	switch {
	case len(trees) > 0:
		for _, t := range trees {
			c, _ := r.resolveCommit(t)
			s, err := r.treeSide(c)
			if err != nil {
				return err
			}
			sources = append(sources, source{t + ":", s})
		}
	case cached:
		idx, err := r.readIndex()
		if err != nil {
			return err
		}
		sources = append(sources, source{"", r.indexSide(idx)})
	default:
		idx, err := r.readIndex()
		if err != nil {
			return err
		}
		var tracked []string
		for p := range r.indexSide(idx) {
			tracked = append(tracked, p)
		}
		s, err := r.worktreeSide(tracked)
		if err != nil {
			return err
		}
		sources = append(sources, source{"", s})
	}
	found := false
	for _, src := range sources {
		var names []string
		for p, e := range src.files {
			if e.mode == filemode.Submodule {
				continue
			}
			if matchAny(specs, p) && (r.prefix == "" || strings.HasPrefix(p, r.prefix) || len(specs) > 0) {
				names = append(names, p)
			}
		}
		sort.Strings(names)
		for _, p := range names {
			data, err := src.files[p].data()
			if err != nil {
				return err
			}
			name := src.label + q(r.display(p))
			if isBinary(data) {
				if re.Match(data) != invert {
					found = true
					if !quiet {
						if filesOnly {
							fmt.Fprintln(g.out, name)
						} else {
							fmt.Fprintf(g.out, "Binary file %s matches\n", name)
						}
					}
				}
				continue
			}
			count := 0
			for n, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
				if re.Match(line) == invert {
					continue
				}
				count++
				found = true
				if quiet || countOnly || filesOnly || filesWithout {
					continue
				}
				prefix := ""
				if !noFilename {
					prefix = name + ":"
				}
				if lineNumbers {
					prefix += strconv.Itoa(n+1) + ":"
				}
				if onlyMatching && !invert {
					for _, m := range re.FindAll(line, -1) {
						fmt.Fprintf(g.out, "%s%s\n", prefix, m)
					}
					continue
				}
				fmt.Fprintf(g.out, "%s%s\n", prefix, line)
			}
			switch {
			case quiet:
			case countOnly && count > 0:
				fmt.Fprintf(g.out, "%s:%d\n", name, count)
			case filesOnly && count > 0:
				fmt.Fprintln(g.out, name)
			case filesWithout && count == 0:
				fmt.Fprintln(g.out, name)
			}
		}
	}
	if !found {
		return failf(1, "")
	}
	return nil
}

func (g *gitRun) blame(args []string) error {
	var suppress, showEmail, longHash, rawTime bool
	lineRange := ""
	var rest, paths []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			paths = append(paths, args[i+1:]...)
			i = len(args)
		case a == "-s":
			suppress = true
		case a == "-e" || a == "--show-email":
			showEmail = true
		case a == "-l":
			longHash = true
		case a == "-t":
			rawTime = true
		case a == "-L" && i+1 < len(args):
			i++
			lineRange = args[i]
		case strings.HasPrefix(a, "-L"):
			lineRange = a[2:]
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	rev := "HEAD"
	if len(paths) == 0 && len(rest) > 0 {
		paths, rest = rest[len(rest)-1:], rest[:len(rest)-1]
	}
	if len(rest) > 0 {
		rev = rest[0]
	}
	if len(paths) != 1 {
		return &exitError{code: 129, msg: "usage: git blame [<options>] [<rev-opts>] [<rev>] [--] <file>\n"}
	}
	p, err := r.repoPath(paths[0])
	if err != nil {
		return err
	}
	start, err := r.resolveCommit(rev)
	if err != nil {
		return err
	}
	lines, origins, err := r.blameFile(start, p, len(rest) == 0)
	if err != nil {
		return err
	}
	from, to := 1, len(lines)
	if lineRange != "" {
		a, b, _ := strings.Cut(lineRange, ",")
		if n, err := strconv.Atoi(a); err == nil {
			from = n
		}
		if b != "" {
			if strings.HasPrefix(b, "+") {
				n, _ := strconv.Atoi(b[1:])
				to = from + n - 1
			} else if n, err := strconv.Atoi(b); err == nil {
				to = n
			}
		}
		if from < 1 || from > len(lines) || to < from {
			return fatalf("file %s has only %d lines", p, len(lines))
		}
		to = min(to, len(lines))
	}
	nameWidth, numWidth := 0, len(strconv.Itoa(to))
	for i := from - 1; i < to; i++ {
		c := origins[i]
		label := c.Author.Name
		if showEmail {
			label = "<" + c.Author.Email + ">"
		}
		nameWidth = max(nameWidth, len(label))
	}
	for i := from - 1; i < to; i++ {
		c := origins[i]
		boundary := len(c.ParentHashes) == 0
		hash := c.Hash.String()[:8]
		if longHash {
			hash = c.Hash.String()
		}
		if boundary {
			hash = "^" + c.Hash.String()[:7]
			if longHash {
				hash = "^" + c.Hash.String()[:39]
			}
		}
		line := strings.TrimSuffix(lines[i], "\n")
		if suppress {
			fmt.Fprintf(g.out, "%s %*d) %s\n", hash, numWidth, i+1, line)
			continue
		}
		label := c.Author.Name
		if showEmail {
			label = "<" + c.Author.Email + ">"
		}
		date := c.Author.When.Format("2006-01-02 15:04:05 -0700")
		if rawTime {
			date = formatDate(c.Author.When, "raw")
		}
		fmt.Fprintf(g.out, "%s (%-*s %s %*d) %s\n", hash, nameWidth, label, date, numWidth, i+1, line)
	}
	return nil
}

// blameFile assigns each line of p at start to the commit that introduced
// it, following first parents and renames. Lines present in the root
// commit belong to it (git marks those boundary commits with '^').
func (r *repo) blameFile(start *object.Commit, p string, withWorktree bool) ([]string, []*object.Commit, error) {
	s, err := r.treeSide(start)
	if err != nil {
		return nil, nil, err
	}
	e, ok := s[p]
	if !ok {
		return nil, nil, fatalf("no such path '%s' in HEAD", p)
	}
	data, err := e.data()
	if err != nil {
		return nil, nil, err
	}
	lines := splitLines(data)
	origins := make([]*object.Commit, len(lines))
	// track[k] is the output line shown by line k of the current version,
	// or -1 once that line has been attributed.
	track := make([]int, len(lines))
	for i := range track {
		track[i] = i
	}
	pending := len(lines)
	assign := func(c *object.Commit) {
		for k, out := range track {
			if out >= 0 {
				origins[out] = c
				track[k] = -1
			}
		}
		pending = 0
	}
	cur, curPath, curLines := start, p, lines
	for pending > 0 {
		if len(cur.ParentHashes) == 0 {
			assign(cur)
			break
		}
		parent, err := r.CommitObject(cur.ParentHashes[0])
		if err != nil {
			return nil, nil, err
		}
		ps, err := r.treeSide(parent)
		if err != nil {
			return nil, nil, err
		}
		pPath := curPath
		if _, ok := ps[pPath]; !ok {
			// Follow a rename into this commit.
			diffs, err := r.commitDiffs(cur, nil)
			if err != nil {
				return nil, nil, err
			}
			for _, d := range diffs {
				if d.path == curPath && d.oldPath != "" {
					pPath = d.oldPath
				}
			}
		}
		pe, ok := ps[pPath]
		if !ok {
			assign(cur)
			break
		}
		pdata, err := pe.data()
		if err != nil {
			return nil, nil, err
		}
		pLines := splitLines(pdata)
		next := make([]int, len(pLines))
		for i := range next {
			next[i] = -1
		}
		for _, op := range diffLines(pLines, curLines) {
			switch op.kind {
			case ' ':
				next[op.a] = track[op.b]
			case '+':
				if out := track[op.b]; out >= 0 {
					origins[out] = cur
					pending--
				}
			}
		}
		track, cur, curPath, curLines = next, parent, pPath, pLines
	}
	return lines, origins, nil
}
