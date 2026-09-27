package git

import (
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
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
	count     int // -1 for unlimited
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
		case a == "--no-decorate" || a == "--decorate" || a == "--no-color" || a == "--first-parent":
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
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	specs, err := r.pathspecs(f.paths)
	if err != nil {
		return err
	}
	from, exclude := "HEAD", ""
	switch len(f.revs) {
	case 0:
	case 1:
		from = f.revs[0]
		if a, b, ok := strings.Cut(from, ".."); ok {
			exclude, from = a, b
			if from == "" {
				from = "HEAD"
			}
		}
	default:
		return fatalf("this git supports at most one revision or range")
	}
	if from == "HEAD" {
		if head, err := r.headCommit(); err == nil && head == nil {
			branch, _ := r.branchName()
			return fatalf("your current branch '%s' does not have any commits yet", branch)
		}
	}
	start, err := r.resolveCommit(from)
	if err != nil {
		return err
	}
	excluded := map[plumbing.Hash]bool{}
	if exclude != "" {
		c, err := r.resolveCommit(exclude)
		if err != nil {
			return err
		}
		iter := object.NewCommitPreorderIter(c, nil, nil)
		iter.ForEach(func(c *object.Commit) error { excluded[c.Hash] = true; return nil })
	}
	var commits []*object.Commit
	iter := object.NewCommitPreorderIter(start, nil, nil)
	err = iter.ForEach(func(c *object.Commit) error {
		if excluded[c.Hash] {
			return nil
		}
		if len(specs) > 0 {
			diffs, err := r.commitDiffs(c, specs)
			if err != nil || len(diffs) == 0 {
				return err
			}
		}
		commits = append(commits, c)
		if f.count >= 0 && len(commits) >= f.count && !f.reverse {
			return errStop
		}
		return nil
	})
	if err != nil && err != errStop {
		return err
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

var errStop = fmt.Errorf("stop")

// writeCommit prints the n-th commit of a log or show listing.
func (r *repo) writeCommit(w io.Writer, c *object.Commit, f *logFormat, specs []string, n int) error {
	switch f.kind {
	case "medium":
		if n > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "commit %s\n", c.Hash)
		if len(c.ParentHashes) > 1 {
			fmt.Fprint(w, "Merge:")
			for _, p := range c.ParentHashes {
				fmt.Fprintf(w, " %s", short(p))
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "Author: %s <%s>\nDate:   %s\n\n", c.Author.Name, c.Author.Email, gitDate(c.Author.When))
		for _, line := range strings.Split(strings.TrimRight(c.Message, "\n"), "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	case "oneline", "full-oneline":
		h := c.Hash.String()
		if f.kind == "oneline" {
			h = short(c.Hash)
		}
		fmt.Fprintf(w, "%s %s\n", h, subject(c.Message))
	case "format":
		if n > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprint(w, expandFormat(f.template, c))
	case "tformat":
		fmt.Fprintf(w, "%s\n", expandFormat(f.template, c))
	}
	if f.noPatch || !(f.patch || f.stat || f.nameOnly || f.nameStat) {
		return nil
	}
	diffs, err := r.commitDiffs(c, specs)
	if err != nil || len(diffs) == 0 {
		return err
	}
	if f.kind != "oneline" && f.kind != "full-oneline" {
		fmt.Fprintln(w)
	}
	writeDiffs(w, diffs, f.stat, f.patch, f.nameOnly, f.nameStat, false)
	return nil
}

// writeDiffs prints the selected diff outputs in git's order.
func writeDiffs(w io.Writer, diffs []*fileDiff, stat, patch, nameOnly, nameStat, numstat bool) {
	for _, d := range diffs {
		switch {
		case numstat && d.binary:
			fmt.Fprintf(w, "-\t-\t%s\n", d.path)
		case numstat:
			fmt.Fprintf(w, "%d\t%d\t%s\n", d.added, d.deleted, d.path)
		case nameStat:
			fmt.Fprintf(w, "%c\t%s\n", changeCode(d.change), d.path)
		case nameOnly:
			fmt.Fprintln(w, d.path)
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

func expandFormat(tmpl string, c *object.Commit) string {
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
		case tmpl[i] == 'd' || tmpl[i] == 'D':
			// Decorations are not supported; git prints nothing without refs.
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
	r, err := g.openRepo()
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
	var cached, stat, nameOnly, nameStat, numstat, quiet, exitCode bool
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
		case "--no-color", "-p", "-u", "--patch":
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
	diffs, err := computeDiffs(changes(a, b, specs))
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
	r, err := g.openRepo()
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
	r, err := g.openRepo()
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
			fmt.Fprintf(g.out, "%s %s %d\t%s\n", mode6(e.Mode), e.Hash, e.Stage, r.display(e.Name))
		} else {
			fmt.Fprintln(g.out, r.display(e.Name))
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
		if r, err = g.openRepo(); err != nil {
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
