package git

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// git rebase with the merge backend (non-interactive). State lives in
// .git/rebase-merge like git's, so status can describe a stopped rebase.

const rebaseDir = "rebase-merge"

type rebaseState struct {
	headName string // "refs/heads/feature", or "detached HEAD"
	onto     plumbing.Hash
	origHead plumbing.Hash
	done     []string // "pick <hash> # <subject>"
	todo     []string
}

func (r *repo) rebaseFile(name string) string { return path.Join(rebaseDir, name) }

func (r *repo) loadRebase() *rebaseState {
	head, ok := r.gitFile(r.rebaseFile("head-name"))
	if !ok {
		return nil
	}
	onto, _ := r.gitFile(r.rebaseFile("onto"))
	orig, _ := r.gitFile(r.rebaseFile("orig-head"))
	done, _ := r.gitFile(r.rebaseFile("done"))
	todo, _ := r.gitFile(r.rebaseFile("git-rebase-todo"))
	return &rebaseState{
		headName: strings.TrimSpace(head),
		onto:     plumbing.NewHash(strings.TrimSpace(onto)),
		origHead: plumbing.NewHash(strings.TrimSpace(orig)),
		done:     lines(done),
		todo:     lines(todo),
	}
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (r *repo) saveRebase(st *rebaseState) error {
	if err := newBillyFS(r.g.fsys, fsName(r.gitDir)).MkdirAll(rebaseDir, 0777); err != nil {
		return err
	}
	join := func(ls []string) string {
		if len(ls) == 0 {
			return ""
		}
		return strings.Join(ls, "\n") + "\n"
	}
	files := map[string]string{
		"head-name":       st.headName + "\n",
		"onto":            st.onto.String() + "\n",
		"orig-head":       st.origHead.String() + "\n",
		"done":            join(st.done),
		"git-rebase-todo": join(st.todo),
		"interactive":     "",
		"msgnum":          strconv.Itoa(len(st.done)) + "\n",
		"end":             strconv.Itoa(len(st.done)+len(st.todo)) + "\n",
	}
	for name, content := range files {
		if err := r.writeGitFile(r.rebaseFile(name), content); err != nil {
			return err
		}
	}
	return nil
}

func (r *repo) clearRebase() {
	d := newBillyFS(r.g.fsys, fsName(r.gitDir))
	infos, err := d.ReadDir(rebaseDir)
	if err != nil {
		return
	}
	for _, info := range infos {
		d.Remove(path.Join(rebaseDir, info.Name()))
	}
	d.Remove(rebaseDir)
}

func todoLine(c *object.Commit) string {
	return fmt.Sprintf("pick %s # %s", c.Hash, subject(c.Message))
}

func todoHash(line string) plumbing.Hash {
	f := strings.Fields(line)
	if len(f) < 2 {
		return plumbing.ZeroHash
	}
	return plumbing.NewHash(f[1])
}

func (g *gitRun) rebase(args []string) error {
	var cont, abort, skip, quiet bool
	onto := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--continue":
			cont = true
		case a == "--abort" || a == "--quit":
			abort = true
		case a == "--skip":
			skip = true
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--onto" && i+1 < len(args):
			i++
			onto = args[i]
		case strings.HasPrefix(a, "--onto="):
			onto = strings.TrimPrefix(a, "--onto=")
		case a == "-i" || a == "--interactive" || a == "--edit-todo":
			return fatalf("interactive rebase needs an editor, which is not available")
		case a == "--merge" || a == "-m" || a == "--no-autosquash" || a == "--no-fork-point":
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
	st := r.loadRebase()
	switch {
	case cont || skip || abort:
		if st == nil {
			return fatalf("no rebase in progress")
		}
		if abort {
			return r.abortRebase(st)
		}
		if skip {
			head, err := r.headCommit()
			if err != nil {
				return err
			}
			if err := r.resetHard(head); err != nil {
				return err
			}
			r.removeGitFiles(r.rebaseFile("stopped-sha"), r.rebaseFile("message"))
			return r.runRebase(st, quiet)
		}
		idx, err := r.readIndex()
		if err != nil {
			return err
		}
		if un := unmergedPaths(idx); len(un) > 0 {
			var b strings.Builder
			for _, p := range sortedKeys(un) {
				b.WriteString(p + ": needs merge\n")
			}
			b.WriteString("You must edit all merge conflicts and then\nmark them as resolved using git add\n")
			return failf(1, "%s", b.String())
		}
		if err := r.commitStopped("rebase (continue)"); err != nil {
			return err
		}
		return r.runRebase(st, quiet)
	case st != nil:
		return fatalf("It seems that there is already a rebase-merge directory, and\n" +
			"I wonder if you are in the middle of another rebase.  If that is the\n" +
			"case, please try\n\tgit rebase (--continue | --abort | --skip)\n" +
			"If that is not the case, please\n\trm -fr \".git/rebase-merge\"\n" +
			"and run me again.  I am stopping in case you still have something\n" +
			"valuable there.\n")
	}
	if err := r.errUnmerged("Rebasing", "Exiting because of an unresolved conflict."); err != nil {
		return err
	}
	upstreamName := ""
	switch len(rest) {
	case 0:
		branch, _ := r.branchName()
		up := r.upstreamRef(branch)
		if up == "" {
			return fatalf("There is no tracking information for the current branch.")
		}
		upstreamName = shortRef(up)
	case 1, 2:
		upstreamName = rest[0]
		if len(rest) == 2 {
			r.quietCheckout = true
			err := r.switchBranch(rest[1], false, "", false, true)
			r.quietCheckout = false
			if err != nil {
				return err
			}
		}
	default:
		return usagef("usage: git rebase [--onto <newbase>] [<upstream> [<branch>]]")
	}
	return r.startRebase(upstreamName, onto, quiet)
}

// patchID identifies a commit's change independently of its position, like
// git patch-id: the diffs' changed lines with whitespace removed.
func (r *repo) patchID(c *object.Commit) (string, error) {
	diffs, err := r.commitDiffs(c, nil)
	if err != nil {
		return "", err
	}
	h := sha1.New()
	for _, d := range diffs {
		fmt.Fprintf(h, "%s\x00%s\x00", d.fromPath(), d.path)
		for _, op := range d.ops {
			if op.kind != ' ' {
				fmt.Fprintf(h, "%c%s", op.kind, strings.Join(strings.Fields(op.line), ""))
			}
		}
		if d.binary {
			fmt.Fprintf(h, "bin%v%v", d.from != nil, d.to != nil)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (r *repo) startRebase(upstreamName, ontoName string, quiet bool) error {
	g := r.g
	upstream, err := r.resolveCommit(upstreamName)
	if err != nil {
		return fatalf("invalid upstream '%s'", upstreamName)
	}
	if ontoName == "" {
		ontoName = upstreamName
	}
	onto, err := r.resolveCommit(ontoName)
	if err != nil {
		return fatalf("Does not point to a valid commit '%s'", ontoName)
	}
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	branch, _ := r.branchName()
	headName := "detached HEAD"
	if branch != "" {
		headName = plumbing.NewBranchReferenceName(branch).String()
	}
	base, err := r.mergeBase(upstream.Hash, head.Hash)
	if err != nil {
		return err
	}
	if base != nil && base.Hash == onto.Hash {
		if branch == "" {
			fmt.Fprintln(g.out, "HEAD is up to date.")
		} else {
			fmt.Fprintf(g.out, "Current branch %s is up to date.\n", branch)
		}
		return nil
	}
	// Commits to replay: upstream..HEAD without merges, oldest first.
	f := newLogFormat()
	f.revs = []string{upstream.Hash.String() + ".." + head.Hash.String()}
	f.filter.maxParents = 1
	picks, err := r.walk(f, nil)
	if err != nil {
		return err
	}
	picks = topoOrder(picks)
	// Drop commits whose change is already upstream.
	f2 := newLogFormat()
	f2.revs = []string{head.Hash.String() + ".." + upstream.Hash.String()}
	f2.filter.maxParents = 1
	theirs, err := r.walk(f2, nil)
	if err != nil {
		return err
	}
	upstreamIDs := map[string]bool{}
	for _, c := range theirs {
		id, err := r.patchID(c)
		if err != nil {
			return err
		}
		upstreamIDs[id] = true
	}
	st := &rebaseState{headName: headName, onto: onto.Hash, origHead: head.Hash}
	for i := len(picks) - 1; i >= 0; i-- {
		c := picks[i]
		id, err := r.patchID(c)
		if err != nil {
			return err
		}
		if upstreamIDs[id] {
			fmt.Fprintf(g.err, "warning: skipped previously applied commit %s\n"+
				"hint: use --reapply-cherry-picks to include skipped commits\n"+
				"hint: Disable this message with \"git config set advice.skippedCherryPicks false\"\n", short(c.Hash))
			continue
		}
		st.todo = append(st.todo, todoLine(c))
	}
	if err := r.switchTo(onto, "checkout", false); err != nil {
		return err
	}
	r.writeGitFile(origHeadFile, head.Hash.String()+"\n")
	if err := r.pointHead("", onto.Hash, "rebase (start): checkout "+ontoName); err != nil {
		return err
	}
	if err := r.saveRebase(st); err != nil {
		return err
	}
	return r.runRebase(st, quiet)
}

// runRebase picks the remaining commits, stopping at a conflict.
func (r *repo) runRebase(st *rebaseState, quiet bool) error {
	g := r.g
	total := len(st.done) + len(st.todo)
	for len(st.todo) > 0 {
		line := st.todo[0]
		st.todo, st.done = st.todo[1:], append(st.done, line)
		c, err := r.CommitObject(todoHash(line))
		if err != nil {
			return err
		}
		if !quiet {
			fmt.Fprintf(g.err, "Rebasing (%d/%d)\r", len(st.done), total)
		}
		if err := r.saveRebase(st); err != nil {
			return err
		}
		stopped, err := r.pickForRebase(c)
		if err != nil {
			return err
		}
		if stopped {
			return failf(1, "error: could not apply %s... %s\n"+
				"hint: Resolve all conflicts manually, mark them as resolved with\n"+
				"hint: \"git add/rm <conflicted_files>\", then run \"git rebase --continue\".\n"+
				"hint: You can instead skip this commit: run \"git rebase --skip\".\n"+
				"hint: To abort and get back to the state before \"git rebase\", run \"git rebase --abort\".\n"+
				"hint: Disable this message with \"git config set advice.mergeConflict false\"\n"+
				"Could not apply %s... # %s\n", short(c.Hash), subject(c.Message), short(c.Hash), subject(c.Message))
		}
	}
	return r.finishRebase(st, quiet)
}

// pickForRebase applies c on HEAD and commits it; it reports a conflict
// stop, leaving the message and author for --continue.
func (r *repo) pickForRebase(c *object.Commit) (bool, error) {
	head, err := r.headCommit()
	if err != nil {
		return false, err
	}
	var parent *object.Commit
	if len(c.ParentHashes) > 0 {
		if parent, err = r.CommitObject(c.ParentHashes[0]); err != nil {
			return false, err
		}
	}
	baseSide, err := r.treeSide(parent)
	if err != nil {
		return false, err
	}
	ours, err := r.treeSide(head)
	if err != nil {
		return false, err
	}
	theirs, err := r.treeSide(c)
	if err != nil {
		return false, err
	}
	tm, err := r.mergeTrees(baseSide, ours, theirs, "HEAD", short(c.Hash)+" ("+subject(c.Message)+")")
	if err != nil {
		return false, err
	}
	if err := r.applyMerge(tm, ours, "merge"); err != nil {
		return false, err
	}
	for _, m := range tm.messages {
		fmt.Fprintln(r.g.out, m)
	}
	r.writeGitFile(r.rebaseFile("stopped-sha"), c.Hash.String()+"\n")
	r.writeGitFile(r.rebaseFile("message"), c.Message)
	if len(tm.conflicted()) > 0 {
		return true, nil
	}
	return false, r.commitStopped("rebase (pick)")
}

// commitStopped commits the index for the commit recorded in stopped-sha,
// keeping its author and message. An unchanged tree drops the commit.
// After a conflict (--continue) the commit is summarized as git commit does.
func (r *repo) commitStopped(action string) error {
	s, ok := r.gitFile(r.rebaseFile("stopped-sha"))
	if !ok {
		return nil
	}
	c, err := r.CommitObject(plumbing.NewHash(strings.TrimSpace(s)))
	if err != nil {
		return err
	}
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	headSide, err := r.treeSide(head)
	if err != nil {
		return err
	}
	r.removeGitFiles(r.rebaseFile("stopped-sha"), r.rebaseFile("message"))
	if len(changes(headSide, r.indexSide(idx), nil)) == 0 {
		return nil // the change is already present; drop the commit
	}
	committer, err := r.signature("COMMITTER")
	if err != nil {
		return err
	}
	h, err := r.commitIndex(c.Message, &c.Author, committer, []plumbing.Hash{head.Hash})
	if err != nil {
		return err
	}
	if err := r.setHead(h, action+": "+subject(c.Message)); err != nil {
		return err
	}
	if action == "rebase (continue)" {
		return r.printCommitSummary(h, false)
	}
	return nil
}

func (r *repo) finishRebase(st *rebaseState, quiet bool) error {
	g := r.g
	head := r.refHash(plumbing.HEAD)
	if strings.HasPrefix(st.headName, "refs/") {
		name := plumbing.ReferenceName(st.headName)
		if err := r.updateRef(name, head, "rebase (finish): "+st.headName+" onto "+st.onto.String()); err != nil {
			return err
		}
		if err := r.pointHead(name, plumbing.ZeroHash, "rebase (finish): returning to "+st.headName); err != nil {
			return err
		}
	}
	r.clearRebase()
	if !quiet {
		fmt.Fprintf(g.err, "Successfully rebased and updated %s.\n", st.headName)
	}
	return nil
}

func (r *repo) abortRebase(st *rebaseState) error {
	orig, err := r.CommitObject(st.origHead)
	if err != nil {
		return err
	}
	if err := r.resetHard(orig); err != nil {
		return err
	}
	if strings.HasPrefix(st.headName, "refs/") {
		if err := r.pointHead(plumbing.ReferenceName(st.headName), plumbing.ZeroHash, "rebase (abort): returning to "+st.headName); err != nil {
			return err
		}
	} else if err := r.pointHead("", orig.Hash, "rebase (abort): returning to "+orig.Hash.String()); err != nil {
		return err
	}
	r.clearRebase()
	return nil
}

// rebaseStatus writes the status block for a rebase in progress.
func (r *repo) writeRebaseStatus(w interface{ Write([]byte) (int, error) }, st *rebaseState, unmerged bool) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	p("interactive rebase in progress; onto %s\n", short(st.onto))
	abbrev := func(line string) string {
		f := strings.Fields(line)
		if len(f) >= 2 && len(f[1]) == 40 {
			f[1] = f[1][:7]
		}
		return strings.Join(f, " ")
	}
	if n := len(st.done); n > 0 {
		if n == 1 {
			p("Last command done (1 command done):\n")
		} else {
			p("Last commands done (%d commands done):\n", n)
		}
		for _, l := range st.done[max(0, n-2):] {
			p("   %s\n", abbrev(l))
		}
		if n > 2 {
			p("  (see more in file .git/rebase-merge/done)\n")
		}
	}
	switch n := len(st.todo); n {
	case 0:
		p("No commands remaining.\n")
	default:
		if n == 1 {
			p("Next command to do (1 remaining command):\n")
		} else {
			p("Next commands to do (%d remaining commands):\n", n)
		}
		for _, l := range st.todo[:min(2, n)] {
			p("   %s\n", abbrev(l))
		}
		p("  (use \"git rebase --edit-todo\" to view and edit)\n")
	}
	branch := strings.TrimPrefix(st.headName, "refs/heads/")
	p("You are currently rebasing branch '%s' on '%s'.\n", branch, short(st.onto))
	if unmerged {
		p("  (fix conflicts and then run \"git rebase --continue\")\n")
		p("  (use \"git rebase --skip\" to skip this patch)\n")
		p("  (use \"git rebase --abort\" to check out the original branch)\n\n")
	} else {
		p("  (all conflicts fixed: run \"git rebase --continue\")\n\n")
	}
}
