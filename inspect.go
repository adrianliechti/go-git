package git

import (
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func gitDate(t time.Time) string { return t.Format("Mon Jan 2 15:04:05 2006 -0700") }

// logFormat selects how commits are printed.
type logFormat struct {
	kind      string // "medium", "oneline", "format" (separator), "tformat" (terminator)
	template  string
	patch     bool
	stat      bool
	nameOnly  bool
	nameStat  bool
	noPatch   bool
	decorate  bool
	all       bool
	graphMode bool
	topo      bool
	graph     *logGraph
	excluded  map[plumbing.Hash]bool
	deco      map[plumbing.Hash][]string // loaded lazily
	count     int                        // -1 for unlimited
	reverse   bool
	revs      []string
	paths     []string
	remaining []string // unrecognized arguments, for the caller
}

// parseLogArgs parses options shared by log and show.
func parseLogArgs(args []string) (*logFormat, error) {
	f := &logFormat{kind: "medium", count: -1}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			f.paths = append(f.paths, args[i+1:]...)
			i = len(args)
		case a == "--oneline":
			f.kind = "oneline"
		case strings.HasPrefix(a, "--format=") || strings.HasPrefix(a, "--pretty="):
			v := a[strings.Index(a, "=")+1:]
			switch {
			case v == "oneline":
				f.kind = "full-oneline"
			case v == "medium":
				f.kind = v
			case strings.HasPrefix(v, "format:"):
				f.kind, f.template = "format", strings.TrimPrefix(v, "format:")
			default:
				f.kind, f.template = "tformat", strings.TrimPrefix(v, "tformat:")
			}
		case a == "-p" || a == "-u" || a == "--patch":
			f.patch = true
		case a == "--stat":
			f.stat = true
		case a == "--name-only":
			f.nameOnly = true
		case a == "--name-status":
			f.nameStat = true
		case a == "-s" || a == "--no-patch":
			f.noPatch = true
		case a == "--reverse":
			f.reverse = true
		case a == "--all":
			f.all = true
		case a == "--graph":
			f.graphMode, f.topo = true, true
		case a == "--topo-order":
			f.topo = true
		case a == "--date-order":
		case a == "--decorate" || a == "--decorate=short" || a == "--decorate=full":
			f.decorate = true
		case a == "--no-decorate":
			f.decorate = false
		case a == "--no-color" || a == "--first-parent":
		case a == "-n" && i+1 < len(args):
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return nil, fatalf("'%s': not an integer", args[i])
			}
			f.count = n
		case strings.HasPrefix(a, "--max-count=") || (strings.HasPrefix(a, "-n") && len(a) > 2):
			v := strings.TrimPrefix(strings.TrimPrefix(a, "--max-count="), "-n")
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fatalf("'%s': not an integer", v)
			}
			f.count = n
		case len(a) > 1 && a[0] == '-' && isDigits(a[1:]):
			f.count, _ = strconv.Atoi(a[1:])
		case strings.HasPrefix(a, "-"):
			f.remaining = append(f.remaining, a)
		default:
			f.revs = append(f.revs, a)
		}
	}
	return f, nil
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func (g *gitRun) log(args []string) error {
	f, err := parseLogArgs(args)
	if err != nil {
		return err
	}
	if len(f.remaining) > 0 {
		return fatalf("unrecognized argument: %s", f.remaining[0])
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	specs, err := r.pathspecs(f.paths)
	if err != nil {
		return err
	}
	if f.graphMode && f.reverse {
		return fatalf("options '--graph' and '--reverse' cannot be used together")
	}
	count := f.count
	if f.topo {
		f.count = -1 // topological sorting needs the whole walk
	}
	commits, err := r.walk(f, specs)
	f.count = count
	if err != nil {
		return err
	}
	if f.topo {
		commits = topoOrder(commits)
		if f.count >= 0 && len(commits) > f.count && !f.reverse {
			commits = commits[:f.count]
		}
	}
	if f.graphMode {
		f.graph = newLogGraph(func(h plumbing.Hash) bool { return !f.excluded[h] })
	}
	if f.reverse {
		if f.count >= 0 && len(commits) > f.count {
			commits = commits[:f.count]
		}
		for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
			commits[i], commits[j] = commits[j], commits[i]
		}
	}
	for i, c := range commits {
		if err := r.writeCommit(g.out, c, f, specs, i); err != nil {
			return err
		}
	}
	if f.kind == "format" && len(commits) > 0 && (f.patch || f.stat) {
		fmt.Fprintln(g.out)
	}
	return nil
}

// walk selects commits like git's default revision walk: a queue ordered by
// committer date, ties broken by insertion order, parents added as commits
// are shown.
func (r *repo) walk(f *logFormat, specs []string) ([]*object.Commit, error) {
	var include []plumbing.Hash
	excluded := map[plumbing.Hash]bool{}
	exclude := func(rev string) error {
		c, err := r.resolveCommit(rev)
		if err != nil {
			return err
		}
		for h := range r.ancestors(c.Hash) {
			excluded[h] = true
		}
		return nil
	}
	revs := f.revs
	if len(revs) == 0 && !f.all {
		if head, err := r.headCommit(); err == nil && head == nil {
			branch, _ := r.branchName()
			return nil, fatalf("your current branch '%s' does not have any commits yet", branch)
		}
		revs = []string{"HEAD"}
	}
	for _, rev := range revs {
		switch a, b, isRange := strings.Cut(rev, ".."); {
		case isRange:
			if err := exclude(defaultHead(a)); err != nil {
				return nil, err
			}
			rev = defaultHead(b)
		case strings.HasPrefix(rev, "^"):
			if err := exclude(rev[1:]); err != nil {
				return nil, err
			}
			continue
		}
		c, err := r.resolveCommit(rev)
		if err != nil {
			return nil, err
		}
		include = append(include, c.Hash)
	}
	if f.all {
		iter, err := r.Storer.IterReferences()
		if err != nil {
			return nil, err
		}
		var names []plumbing.ReferenceName
		iter.ForEach(func(ref *plumbing.Reference) error {
			if ref.Name() != plumbing.HEAD {
				names = append(names, ref.Name())
			}
			return nil
		})
		sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
		names = append(names, plumbing.HEAD)
		for _, n := range names {
			if h, ok := r.peel(n); ok {
				if _, err := r.CommitObject(h); err == nil {
					include = append(include, h)
				}
			}
		}
	}
	f.excluded = excluded
	type item struct {
		c   *object.Commit
		ctr int
	}
	var queue []item
	added := map[plumbing.Hash]bool{}
	ctr := 0
	push := func(h plumbing.Hash) error {
		if added[h] {
			return nil
		}
		added[h] = true
		c, err := r.CommitObject(h)
		if err != nil {
			return err
		}
		queue = append(queue, item{c, ctr})
		ctr++
		return nil
	}
	for _, h := range include {
		if err := push(h); err != nil {
			return nil, err
		}
	}
	var out []*object.Commit
	for len(queue) > 0 {
		best := 0
		for i, it := range queue {
			bt, it2 := queue[best].c.Committer.When, it.c.Committer.When
			if it2.After(bt) || (it2.Equal(bt) && it.ctr < queue[best].ctr) {
				best = i
			}
		}
		c := queue[best].c
		queue = append(queue[:best], queue[best+1:]...)
		if excluded[c.Hash] {
			continue
		}
		for _, p := range c.ParentHashes {
			if err := push(p); err != nil {
				return nil, err
			}
		}
		if len(specs) > 0 {
			diffs, err := r.commitDiffs(c, specs)
			if err != nil {
				return nil, err
			}
			if len(diffs) == 0 {
				continue
			}
		}
		out = append(out, c)
		if f.count >= 0 && len(out) >= f.count && !f.reverse {
			break
		}
	}
	return out, nil
}

// commitText renders a commit's header and message without separators.
// The medium format ends with a newline; the one-line formats do not.
func (r *repo) commitText(c *object.Commit, f *logFormat) string {
	var b strings.Builder
	switch f.kind {
	case "medium":
		fmt.Fprintf(&b, "commit %s%s\n", c.Hash, r.decoration(f, c.Hash, f.decorate))
		if len(c.ParentHashes) > 1 {
			b.WriteString("Merge:")
			for _, p := range c.ParentHashes {
				fmt.Fprintf(&b, " %s", short(p))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Author: %s <%s>\nDate:   %s\n\n", c.Author.Name, c.Author.Email, gitDate(c.Author.When))
		for _, line := range strings.Split(strings.TrimRight(c.Message, "\n"), "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	case "oneline", "full-oneline":
		h := c.Hash.String()
		if f.kind == "oneline" {
			h = short(c.Hash)
		}
		fmt.Fprintf(&b, "%s%s %s", h, r.decoration(f, c.Hash, f.decorate), subject(c.Message))
	case "format", "tformat":
		b.WriteString(expandFormat(f.template, c, r.labels(f, c.Hash)))
	}
	return b.String()
}

// separated reports whether entries are separated (medium, format:) rather
// than terminated (oneline, tformat:).
func (f *logFormat) separated() bool { return f.kind == "medium" || f.kind == "format" }

// writeCommit prints the n-th commit of a log or show listing.
func (r *repo) writeCommit(w io.Writer, c *object.Commit, f *logFormat, specs []string, n int) error {
	if f.graph != nil {
		return r.writeGraphCommit(w, c, f, specs, n)
	}
	if n > 0 && f.separated() {
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, r.commitText(c, f))
	if !f.separated() {
		fmt.Fprintln(w)
	}
	diffs, err := r.logDiffs(c, f, specs)
	if err != nil || len(diffs) == 0 {
		return err
	}
	if f.kind != "oneline" && f.kind != "full-oneline" {
		fmt.Fprintln(w)
	}
	writeDiffs(w, diffs, f.stat, f.patch, f.nameOnly, f.nameStat, false)
	return nil
}

func (r *repo) logDiffs(c *object.Commit, f *logFormat, specs []string) ([]*fileDiff, error) {
	// Like git without -m or --cc, merge commits show no diff.
	if f.noPatch || !(f.patch || f.stat || f.nameOnly || f.nameStat) || len(c.ParentHashes) > 1 {
		return nil, nil
	}
	return r.commitDiffs(c, specs)
}

// writeGraphCommit prints a commit with graph rows as git's show_log and
// graph_show_commit_msg do: each message line after the first takes the
// next graph row, leftover rows follow the message, and diff lines are
// prefixed with padding rows.
func (r *repo) writeGraphCommit(w io.Writer, c *object.Commit, f *logFormat, specs []string, n int) error {
	g := f.graph
	var b strings.Builder
	g.update(c)
	if n > 0 && f.separated() {
		b.WriteString(g.paddingLine() + "\n")
	}
	g.showCommit(&b)
	text := r.commitText(c, f)
	for i, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		if i > 0 {
			row, _ := g.nextLine()
			b.WriteString(row)
		}
		b.WriteString(line)
	}
	nl := strings.HasSuffix(text, "\n")
	if !g.finished() {
		if !nl {
			b.WriteString("\n")
		}
		g.remainder(&b)
		if nl {
			b.WriteString("\n")
		}
	}
	if !f.separated() {
		if nl {
			b.WriteString(g.paddingLine())
		}
		b.WriteString("\n")
	}
	diffs, err := r.logDiffs(c, f, specs)
	if err != nil {
		return err
	}
	if len(diffs) > 0 {
		if f.kind != "oneline" && f.kind != "full-oneline" {
			b.WriteString(g.paddingLine() + "\n")
		}
		var d strings.Builder
		writeDiffs(&d, diffs, f.stat, f.patch, f.nameOnly, f.nameStat, false)
		for _, line := range strings.SplitAfter(d.String(), "\n") {
			if line != "" {
				b.WriteString(g.paddingLine() + line)
			}
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// writeDiffs prints the selected diff outputs in git's order.
func writeDiffs(w io.Writer, diffs []*fileDiff, stat, patch, nameOnly, nameStat, numstat bool) {
	for _, d := range diffs {
		switch {
		case numstat && d.binary:
			fmt.Fprintf(w, "-\t-\t%s\n", d.displayName())
		case numstat:
			fmt.Fprintf(w, "%d\t%d\t%s\n", d.added, d.deleted, d.displayName())
		case nameStat:
			if d.oldPath != "" {
				fmt.Fprintf(w, "R%03d\t%s\t%s\n", similarityIndex(d.score), q(d.oldPath), q(d.path))
			} else {
				fmt.Fprintf(w, "%c\t%s\n", changeCode(d.change), q(d.path))
			}
		case nameOnly:
			fmt.Fprintln(w, q(d.path))
		}
	}
	if stat {
		writeStat(w, diffs)
	}
	if patch {
		if stat {
			fmt.Fprintln(w)
		}
		for _, d := range diffs {
			d.writePatch(w)
		}
	}
}

// labels returns the decorations of h, loading all refs on first use.
func (r *repo) labels(f *logFormat, h plumbing.Hash) []string {
	if f.deco == nil {
		f.deco = r.decorations()
	}
	return f.deco[h]
}

// decoration formats " (labels)" when enabled and h has any.
func (r *repo) decoration(f *logFormat, h plumbing.Hash, enabled bool) string {
	if !enabled {
		return ""
	}
	if l := r.labels(f, h); len(l) > 0 {
		return " (" + strings.Join(l, ", ") + ")"
	}
	return ""
}

// decorations maps commits to ref labels in git's order: HEAD (with its
// branch) first, then the other refs in reverse refname order.
func (r *repo) decorations() map[plumbing.Hash][]string {
	out := map[plumbing.Hash][]string{}
	iter, err := r.Storer.IterReferences()
	if err != nil {
		return out
	}
	var names []plumbing.ReferenceName
	iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name() != plumbing.HEAD {
			names = append(names, ref.Name())
		}
		return nil
	})
	sort.Slice(names, func(i, j int) bool { return names[i] > names[j] })
	headBranch := plumbing.ReferenceName("")
	if head, err := r.Storer.Reference(plumbing.HEAD); err == nil && head.Type() == plumbing.SymbolicReference {
		headBranch = head.Target()
	}
	for _, name := range names {
		if name == headBranch {
			continue
		}
		h, ok := r.peel(name)
		if !ok {
			continue
		}
		label := name.String()
		switch {
		case name.IsBranch():
			label = name.Short()
		case name.IsTag():
			label = "tag: " + name.Short()
		case name.IsRemote():
			label = strings.TrimPrefix(label, "refs/remotes/")
		}
		out[h] = append(out[h], label)
	}
	if ref, err := r.Head(); err == nil {
		label := "HEAD"
		if headBranch != "" {
			label = "HEAD -> " + headBranch.Short()
		}
		out[ref.Hash()] = append([]string{label}, out[ref.Hash()]...)
	}
	return out
}

// peel resolves a ref to a commit, following symbolic refs and tags.
func (r *repo) peel(name plumbing.ReferenceName) (plumbing.Hash, bool) {
	ref, err := r.Reference(name, true)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	h := ref.Hash()
	for range 10 {
		t, err := r.TagObject(h)
		if err != nil {
			break
		}
		h = t.Target
	}
	return h, true
}

func expandFormat(tmpl string, c *object.Commit, labels []string) string {
	subj := subject(c.Message)
	body := ""
	if _, rest, ok := strings.Cut(strings.TrimLeft(c.Message, "\n"), "\n\n"); ok {
		body = strings.TrimLeft(rest, "\n")
	}
	var b strings.Builder
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '%' || i+1 >= len(tmpl) {
			b.WriteByte(tmpl[i])
			continue
		}
		i++
		two := ""
		if i+1 < len(tmpl) {
			two = tmpl[i : i+2]
		}
		sig := func(s object.Signature) bool {
			switch two[1] {
			case 'n':
				b.WriteString(s.Name)
			case 'e':
				b.WriteString(s.Email)
			case 'd':
				b.WriteString(gitDate(s.When))
			case 'i':
				b.WriteString(s.When.Format("2006-01-02 15:04:05 -0700"))
			case 'I':
				b.WriteString(s.When.Format("2006-01-02T15:04:05-07:00"))
			case 't':
				b.WriteString(strconv.FormatInt(s.When.Unix(), 10))
			default:
				return false
			}
			return true
		}
		switch {
		case tmpl[i] == 'H':
			b.WriteString(c.Hash.String())
		case tmpl[i] == 'h':
			b.WriteString(short(c.Hash))
		case tmpl[i] == 'T':
			b.WriteString(c.TreeHash.String())
		case tmpl[i] == 't':
			b.WriteString(short(c.TreeHash))
		case tmpl[i] == 'P' || tmpl[i] == 'p':
			var ps []string
			for _, p := range c.ParentHashes {
				if tmpl[i] == 'P' {
					ps = append(ps, p.String())
				} else {
					ps = append(ps, short(p))
				}
			}
			b.WriteString(strings.Join(ps, " "))
		case tmpl[i] == 's':
			b.WriteString(subj)
		case tmpl[i] == 'b':
			b.WriteString(body)
		case tmpl[i] == 'B':
			b.WriteString(c.Message)
		case tmpl[i] == 'n':
			b.WriteByte('\n')
		case tmpl[i] == '%':
			b.WriteByte('%')
		case tmpl[i] == 'd' && len(labels) > 0:
			b.WriteString(" (" + strings.Join(labels, ", ") + ")")
		case tmpl[i] == 'D':
			b.WriteString(strings.Join(labels, ", "))
		case tmpl[i] == 'd':
		case two != "" && tmpl[i] == 'a' && sig(c.Author):
			i++
		case two != "" && tmpl[i] == 'c' && sig(c.Committer):
			i++
		default:
			b.WriteByte('%')
			b.WriteByte(tmpl[i])
		}
	}
	return b.String()
}

func (g *gitRun) show(args []string) error {
	f, err := parseLogArgs(args)
	if err != nil {
		return err
	}
	if len(f.remaining) > 0 {
		return fatalf("unrecognized argument: %s", f.remaining[0])
	}
	if !f.stat && !f.nameOnly && !f.nameStat {
		f.patch = true
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	specs, err := r.pathspecs(f.paths)
	if err != nil {
		return err
	}
	if len(f.revs) == 0 {
		f.revs = []string{"HEAD"}
	}
	for n, rev := range f.revs {
		if strings.Contains(rev, ":") {
			h, err := r.resolveObject(rev)
			if err != nil {
				return err
			}
			if err := r.printObject(g.out, h, true); err != nil {
				return err
			}
			continue
		}
		if ref, err := r.Storer.Reference(plumbing.NewTagReferenceName(rev)); err == nil {
			if t, err := r.TagObject(ref.Hash()); err == nil {
				fmt.Fprintf(g.out, "tag %s\nTagger: %s <%s>\nDate:   %s\n\n%s\n",
					t.Name, t.Tagger.Name, t.Tagger.Email, gitDate(t.Tagger.When), t.Message)
			}
		}
		c, err := r.resolveCommit(rev)
		if err != nil {
			return err
		}
		if err := r.writeCommit(g.out, c, f, specs, n); err != nil {
			return err
		}
	}
	return nil
}

func (g *gitRun) diff(args []string) error {
	var cached, stat, nameOnly, nameStat, numstat, quiet, exitCode, noRenames bool
	var revs, paths []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--":
			paths = append(paths, args[i+1:]...)
			i = len(args)
		case "--cached", "--staged":
			cached = true
		case "--stat":
			stat = true
		case "--name-only":
			nameOnly = true
		case "--name-status":
			nameStat = true
		case "--numstat":
			numstat = true
		case "--quiet":
			quiet, exitCode = true, true
		case "--exit-code":
			exitCode = true
		case "--no-renames":
			noRenames = true
		case "--no-color", "-p", "-u", "--patch", "-M", "--find-renames":
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: invalid option: %s", a)
			}
			revs = append(revs, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	// Leading arguments that resolve are revisions; the rest are paths.
	var commits []*object.Commit
	for len(revs) > 0 && len(commits) < 2 {
		if a, b, ok := strings.Cut(revs[0], ".."); ok && len(commits) == 0 {
			ca, err := r.resolveCommit(defaultHead(a))
			if err != nil {
				return err
			}
			cb, err := r.resolveCommit(defaultHead(b))
			if err != nil {
				return err
			}
			commits = append(commits, ca, cb)
			revs = revs[1:]
			break
		}
		if _, err := r.ResolveRevision(plumbing.Revision(revs[0])); err != nil {
			break
		}
		c, err := r.resolveCommit(revs[0])
		if err != nil {
			return err
		}
		commits = append(commits, c)
		revs = revs[1:]
	}
	for _, p := range revs {
		if _, err := r.wt.Stat(mustRepoPath(r, p)); err != nil {
			return errAmbiguous(p)
		}
	}
	specs, err := r.pathspecs(append(revs, paths...))
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	indexSide := r.indexSide(idx)
	var a, b side
	switch {
	case len(commits) == 2:
		if a, err = r.treeSide(commits[0]); err != nil {
			return err
		}
		if b, err = r.treeSide(commits[1]); err != nil {
			return err
		}
	case cached:
		base := commits
		if len(base) == 0 {
			head, err := r.headCommit()
			if err != nil {
				return err
			}
			base = []*object.Commit{head}
		}
		if a, err = r.treeSide(base[0]); err != nil {
			return err
		}
		b = indexSide
	default:
		a = indexSide
		if len(commits) == 1 {
			if a, err = r.treeSide(commits[0]); err != nil {
				return err
			}
		}
		var paths []string
		for p := range a {
			paths = append(paths, p)
		}
		for p := range indexSide {
			if _, ok := a[p]; !ok {
				paths = append(paths, p)
			}
		}
		if b, err = r.worktreeSide(paths); err != nil {
			return err
		}
	}
	diffs, err := computeDiffs(changes(a, b, specs), noRenames)
	if err != nil {
		return err
	}
	if !quiet {
		patch := !stat && !nameOnly && !nameStat && !numstat
		writeDiffs(g.out, diffs, stat, patch, nameOnly, nameStat, numstat)
	}
	if exitCode && len(diffs) > 0 {
		return failf(1, "")
	}
	return nil
}

func defaultHead(rev string) string {
	if rev == "" {
		return "HEAD"
	}
	return rev
}

func mustRepoPath(r *repo, p string) string {
	rp, _ := r.repoPath(p)
	return dirOrDot(rp)
}

// resolveObject resolves any object name: revisions, "rev:path",
// "rev^{tree}", annotated tag names, and full hashes.
func (r *repo) resolveObject(spec string) (plumbing.Hash, error) {
	if rev, p, ok := strings.Cut(spec, ":"); ok {
		c, err := r.resolveCommit(defaultHead(rev))
		if err != nil {
			return plumbing.ZeroHash, err
		}
		tree, err := c.Tree()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if p = strings.Trim(p, "/"); p == "" {
			return tree.Hash, nil
		}
		e, err := tree.FindEntry(p)
		if err != nil {
			return plumbing.ZeroHash, fatalf("path '%s' does not exist in '%s'", p, defaultHead(rev))
		}
		return e.Hash, nil
	}
	for _, peel := range []string{"^{tree}", "^{commit}"} {
		if base, ok := strings.CutSuffix(spec, peel); ok {
			c, err := r.resolveCommit(base)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			if peel == "^{tree}" {
				return c.TreeHash, nil
			}
			return c.Hash, nil
		}
	}
	if len(spec) == 40 {
		if _, err := hex.DecodeString(spec); err == nil {
			h := plumbing.NewHash(spec)
			if _, err := r.Storer.EncodedObject(plumbing.AnyObject, h); err == nil {
				return h, nil
			}
		}
	}
	if ref, err := r.Storer.Reference(plumbing.NewTagReferenceName(spec)); err == nil {
		return ref.Hash(), nil
	}
	h, err := r.ResolveRevision(plumbing.Revision(spec))
	if err != nil {
		return plumbing.ZeroHash, errAmbiguous(spec)
	}
	return *h, nil
}

// printObject writes an object like cat-file -p; blobs are written raw.
func (r *repo) printObject(w io.Writer, h plumbing.Hash, pretty bool) error {
	obj, err := r.Storer.EncodedObject(plumbing.AnyObject, h)
	if err != nil {
		return err
	}
	if pretty && obj.Type() == plumbing.TreeObject {
		tree, err := r.TreeObject(h)
		if err != nil {
			return err
		}
		for _, e := range tree.Entries {
			kind := "blob"
			switch e.Mode {
			case filemode.Dir:
				kind = "tree"
			case filemode.Submodule:
				kind = "commit"
			}
			fmt.Fprintf(w, "%s %s %s\t%s\n", mode6(e.Mode), kind, e.Hash, e.Name)
		}
		return nil
	}
	rd, err := obj.Reader()
	if err != nil {
		return err
	}
	defer rd.Close()
	_, err = io.Copy(w, rd)
	return err
}

func (g *gitRun) revParse(args []string) error {
	r, err := g.openAny()
	if err != nil {
		if len(args) == 1 && args[0] == "--is-inside-work-tree" {
			return failf(128, "fatal: not a git repository (or any of the parent directories): .git\n")
		}
		return err
	}
	var abbrevRef, shortHash, verify, quiet, fullName bool
	shortLen := 7
	for _, a := range args {
		switch {
		case a == "--show-toplevel":
			fmt.Fprintln(g.out, r.top)
		case a == "--git-dir":
			if g.cwd == r.top {
				fmt.Fprintln(g.out, ".git")
			} else {
				fmt.Fprintln(g.out, strings.TrimSuffix(r.top, "/")+"/.git")
			}
		case a == "--show-prefix":
			fmt.Fprintln(g.out, r.prefix)
		case a == "--show-cdup":
			fmt.Fprintln(g.out, strings.Repeat("../", strings.Count(r.prefix, "/")))
		case a == "--is-inside-work-tree":
			fmt.Fprintln(g.out, "true")
		case a == "--is-inside-git-dir" || a == "--is-bare-repository":
			fmt.Fprintln(g.out, "false")
		case a == "--abbrev-ref":
			abbrevRef = true
		case a == "--symbolic-full-name":
			fullName = true
		case a == "--short":
			shortHash = true
		case strings.HasPrefix(a, "--short="):
			shortHash = true
			shortLen, _ = strconv.Atoi(strings.TrimPrefix(a, "--short="))
			shortLen = min(max(shortLen, 4), 40)
		case a == "--verify":
			verify = true
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--":
		case strings.HasPrefix(a, "-"):
			return fatalf("unsupported rev-parse option: %s", a)
		default:
			if (abbrevRef || fullName) && (a == "HEAD" || r.isBranch(a)) {
				name := a
				if a == "HEAD" {
					if name, _ = r.branchName(); name == "" {
						name = "HEAD"
					}
				}
				if fullName && name != "HEAD" {
					name = "refs/heads/" + name
				}
				fmt.Fprintln(g.out, name)
				continue
			}
			h, err := r.resolveObject(a)
			if err != nil {
				if verify {
					if quiet {
						return failf(1, "")
					}
					return fatalf("Needed a single revision")
				}
				fmt.Fprintln(g.out, a)
				return err
			}
			if shortHash {
				fmt.Fprintln(g.out, h.String()[:shortLen])
			} else {
				fmt.Fprintln(g.out, h)
			}
		}
	}
	return nil
}

func (r *repo) isBranch(name string) bool {
	_, err := r.Storer.Reference(plumbing.NewBranchReferenceName(name))
	return err == nil
}

func (g *gitRun) catFile(args []string) error {
	if len(args) != 2 {
		return usagef("usage: git cat-file (-t | -s | -e | -p | <type>) <object>")
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	h, err := r.resolveObject(args[1])
	if err != nil {
		if args[0] == "-e" {
			return failf(1, "")
		}
		return fatalf("Not a valid object name %s", args[1])
	}
	obj, err := r.Storer.EncodedObject(plumbing.AnyObject, h)
	if err != nil {
		if args[0] == "-e" {
			return failf(1, "")
		}
		return fatalf("Not a valid object name %s", args[1])
	}
	switch args[0] {
	case "-e":
	case "-t":
		fmt.Fprintln(g.out, obj.Type())
	case "-s":
		fmt.Fprintln(g.out, obj.Size())
	case "-p":
		return r.printObject(g.out, h, true)
	case "blob", "tree", "commit", "tag":
		if obj.Type().String() != args[0] {
			return fatalf("git cat-file %s: bad file", args[1])
		}
		return r.printObject(g.out, h, false)
	default:
		return usagef("error: unknown option `%s'", strings.TrimLeft(args[0], "-"))
	}
	return nil
}

func (g *gitRun) lsFiles(args []string) error {
	stage := false
	var paths []string
	for _, a := range args {
		switch {
		case a == "-s" || a == "--stage":
			stage = true
		case a == "-c" || a == "--cached" || a == "--":
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			paths = append(paths, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	specs, err := r.pathspecs(paths)
	if err != nil {
		return err
	}
	if len(specs) == 0 && r.prefix != "" {
		specs = []string{strings.TrimSuffix(r.prefix, "/")}
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	for _, e := range idx.Entries {
		if !matchAny(specs, e.Name) {
			continue
		}
		if stage {
			fmt.Fprintf(g.out, "%s %s %d\t%s\n", mode6(e.Mode), e.Hash, e.Stage, q(r.display(e.Name)))
		} else {
			fmt.Fprintln(g.out, q(r.display(e.Name)))
		}
	}
	return nil
}

func (g *gitRun) hashObject(args []string) error {
	write, stdin := false, false
	var files []string
	for _, a := range args {
		switch a {
		case "-w":
			write = true
		case "--stdin":
			stdin = true
		case "--":
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			files = append(files, a)
		}
	}
	var r *repo
	if write {
		var err error
		if r, err = g.openAny(); err != nil {
			return err
		}
	}
	emit := func(data []byte) error {
		h := plumbing.ComputeHash(plumbing.BlobObject, data)
		if write {
			if _, err := r.writeBlob(data, filemode.Regular); err != nil {
				return err
			}
		}
		fmt.Fprintln(g.out, h)
		return nil
	}
	if stdin {
		data, err := io.ReadAll(g.stdin)
		if err != nil {
			return err
		}
		if err := emit(data); err != nil {
			return err
		}
	}
	for _, name := range files {
		data, err := fs.ReadFile(g.fsys, fsName(g.abs(name)))
		if err != nil {
			return fatalf("could not open '%s' for reading: %v", name, err)
		}
		if err := emit(data); err != nil {
			return err
		}
	}
	return nil
}
