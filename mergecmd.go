package git

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type mergeOptions struct {
	label    string // theirs in conflict markers
	noCommit bool
	squash   bool
	noFF     bool
	quiet    bool
}

func (g *gitRun) merge(args []string) error {
	var opts mergeOptions
	var ffOnly, abort, cont bool
	message := ""
	var revs []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--ff-only":
			ffOnly = true
		case a == "--no-ff":
			opts.noFF = true
		case a == "--ff", "--commit" == a, a == "--stat", a == "--no-edit", a == "--edit":
		case a == "--squash":
			opts.squash = true
		case a == "--no-commit":
			opts.noCommit = true
		case a == "-q" || a == "--quiet":
			opts.quiet = true
		case a == "--abort":
			abort = true
		case a == "--continue":
			cont = true
		case a == "-m" && i+1 < len(args):
			i++
			message = args[i]
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
	switch {
	case abort:
		if _, ok := r.gitFile(mergeHeadFile); !ok {
			return fatalf("There is no merge to abort (MERGE_HEAD missing).")
		}
		head, err := r.headCommit()
		if err != nil {
			return err
		}
		if err := r.resetHard(head); err != nil {
			return err
		}
		r.clearOperationState()
		return nil
	case cont:
		if _, ok := r.gitFile(mergeHeadFile); !ok {
			return fatalf("There is no merge in progress (MERGE_HEAD missing).")
		}
		return g.commit(nil)
	}
	if err := r.errUnmerged("Merging", "Exiting because of an unresolved conflict."); err != nil {
		return err
	}
	if _, ok := r.gitFile(mergeHeadFile); ok {
		return fatalf("You have not concluded your merge (MERGE_HEAD exists).\nPlease, commit your changes before you merge.")
	}
	if len(revs) != 1 {
		return fatalf("this git supports merging exactly one commit")
	}
	target, err := r.resolveCommit(revs[0])
	if err != nil {
		return failf(1, "merge: %s - not something we can merge\n", revs[0])
	}
	opts.label = revs[0]
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	if message == "" {
		message = r.mergeMessage(revs[0])
	}
	switch {
	case head != nil && r.isAncestor(target.Hash, head.Hash):
		fmt.Fprintln(g.out, "Already up to date.")
		return nil
	case head == nil || (r.isAncestor(head.Hash, target.Hash) && !opts.noFF):
		if opts.squash && head != nil {
			return r.squashFastForward(head, target)
		}
		return r.fastForward(head, target, opts.quiet)
	case ffOnly:
		return fatalf("Not possible to fast-forward, aborting.")
	}
	return r.threeWayMerge(target, message, opts)
}

// mergeMessage is git's default merge commit subject for a merged name.
func (r *repo) mergeMessage(name string) string {
	msg := fmt.Sprintf("Merge commit '%s'", name)
	switch {
	case r.isBranch(name):
		msg = fmt.Sprintf("Merge branch '%s'", name)
	case r.hasRef(plumbing.ReferenceName("refs/remotes/" + name)):
		msg = fmt.Sprintf("Merge remote-tracking branch '%s'", name)
	case r.hasRef(plumbing.NewTagReferenceName(name)):
		msg = fmt.Sprintf("Merge tag '%s'", name)
	}
	if cur, _ := r.branchName(); cur != "" && cur != "main" && cur != "master" {
		msg += " into " + cur
	}
	return msg
}

func (r *repo) hasRef(name plumbing.ReferenceName) bool {
	_, err := r.Storer.Reference(name)
	return err == nil
}

// threeWayMerge merges target into HEAD and commits unless asked not to.
func (r *repo) threeWayMerge(target *object.Commit, message string, opts mergeOptions) error {
	g := r.g
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	base, err := r.mergeBase(head.Hash, target.Hash)
	if err != nil {
		return err
	}
	baseSide, err := r.treeSide(base)
	if err != nil {
		return err
	}
	ours, err := r.treeSide(head)
	if err != nil {
		return err
	}
	theirs, err := r.treeSide(target)
	if err != nil {
		return err
	}
	tm, err := r.mergeTrees(baseSide, ours, theirs, "HEAD", opts.label)
	if err != nil {
		return err
	}
	if err := r.applyMerge(tm, ours, "merge"); err != nil {
		return err
	}
	for _, m := range tm.messages {
		fmt.Fprintln(g.out, m)
	}
	r.writeGitFile(origHeadFile, head.Hash.String()+"\n")
	conflicts := tm.conflicted()
	if opts.squash {
		r.writeGitFile(squashMsgFile, r.squashMessage(head, target))
		fmt.Fprintln(g.out, "Squash commit -- not updating HEAD")
		if len(conflicts) > 0 {
			return failf(1, "Automatic merge failed; fix conflicts and then commit the result.\n")
		}
		fmt.Fprintln(g.out, "Automatic merge went well; stopped before committing as requested")
		return nil
	}
	if len(conflicts) > 0 || opts.noCommit {
		msg := message + "\n"
		if len(conflicts) > 0 {
			msg += "\n# Conflicts:\n#\t" + strings.Join(conflicts, "\n#\t") + "\n"
		}
		r.writeGitFile(mergeHeadFile, target.Hash.String()+"\n")
		r.writeGitFile(mergeMsgFile, msg)
		mode := ""
		if opts.noFF {
			mode = "no-ff"
		}
		r.writeGitFile(mergeModeFile, mode)
		if len(conflicts) > 0 {
			return failf(1, "Automatic merge failed; fix conflicts and then commit the result.\n")
		}
		fmt.Fprintln(g.out, "Automatic merge went well; stopped before committing as requested")
		return nil
	}
	author, err := r.signature("AUTHOR")
	if err != nil {
		return err
	}
	committer, err := r.signature("COMMITTER")
	if err != nil {
		return err
	}
	h, err := r.commitIndex(cleanupMessage(message), author, committer, []plumbing.Hash{head.Hash, target.Hash})
	if err != nil {
		return err
	}
	if err := r.setHead(h); err != nil {
		return err
	}
	if opts.quiet {
		return nil
	}
	fmt.Fprintln(g.out, "Merge made by the 'ort' strategy.")
	merged, err := r.CommitObject(h)
	if err != nil {
		return err
	}
	after, err := r.treeSide(merged)
	if err != nil {
		return err
	}
	diffs, err := computeDiffs(changes(ours, after, nil), false)
	if err != nil {
		return err
	}
	writeStat(g.out, diffs)
	writeModeSummary(g.out, diffs)
	return nil
}

// squashFastForward applies a fast-forwardable merge without moving HEAD.
func (r *repo) squashFastForward(head, target *object.Commit) error {
	g := r.g
	fmt.Fprintf(g.out, "Updating %s..%s\nFast-forward\nSquash commit -- not updating HEAD\n", short(head.Hash), short(target.Hash))
	ours, err := r.treeSide(head)
	if err != nil {
		return err
	}
	theirs, err := r.treeSide(target)
	if err != nil {
		return err
	}
	tm, err := r.mergeTrees(ours, ours, theirs, "HEAD", "")
	if err != nil {
		return err
	}
	if err := r.applyMerge(tm, ours, "merge"); err != nil {
		return err
	}
	r.writeGitFile(squashMsgFile, r.squashMessage(head, target))
	diffs, err := computeDiffs(changes(ours, theirs, nil), false)
	if err != nil {
		return err
	}
	writeStat(g.out, diffs)
	writeModeSummary(g.out, diffs)
	return nil
}

// squashMessage lists the squashed commits like git's SQUASH_MSG.
func (r *repo) squashMessage(head, target *object.Commit) string {
	var b strings.Builder
	b.WriteString("Squashed commit of the following:\n")
	f := &logFormat{kind: "medium", count: -1, revs: []string{head.Hash.String() + ".." + target.Hash.String()}}
	commits, err := r.walk(f, nil)
	if err != nil {
		return b.String()
	}
	for _, c := range commits {
		b.WriteString("\n")
		r.writeCommit(&b, c, f, nil, 0)
	}
	return b.String()
}

func (g *gitRun) mergeBaseCmd(args []string) error {
	isAncestor := false
	var revs []string
	for _, a := range args {
		switch {
		case a == "--is-ancestor":
			isAncestor = true
		case a == "--all":
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			revs = append(revs, a)
		}
	}
	if len(revs) != 2 {
		return &exitError{code: 129, msg: "usage: git merge-base [--is-ancestor] <commit> <commit>\n"}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	a, err := r.resolveCommit(revs[0])
	if err != nil {
		return err
	}
	b, err := r.resolveCommit(revs[1])
	if err != nil {
		return err
	}
	if isAncestor {
		if r.isAncestor(a.Hash, b.Hash) {
			return nil
		}
		return failf(1, "")
	}
	base, err := r.mergeBase(a.Hash, b.Hash)
	if err != nil {
		return err
	}
	if base == nil {
		return failf(1, "")
	}
	fmt.Fprintln(g.out, base.Hash)
	return nil
}

// Cherry-pick and revert

func (g *gitRun) cherryPick(args []string) error { return g.pick(args, false) }
func (g *gitRun) revert(args []string) error     { return g.pick(args, true) }

func (g *gitRun) pick(args []string, revert bool) error {
	cmd, verb, stateFile := "cherry-pick", "Cherry-picking", cherryPickFile
	if revert {
		cmd, verb, stateFile = "revert", "Reverting", revertFile
	}
	var cont, abort, skip, noCommit, recordOrigin bool
	var revs []string
	for _, a := range args {
		switch a {
		case "--continue":
			cont = true
		case "--abort", "--quit":
			abort = true
		case "--skip":
			skip = true
		case "-n", "--no-commit":
			noCommit = true
		case "-x":
			recordOrigin = true
		case "--no-edit", "--ff":
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			revs = append(revs, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	if cont || abort || skip {
		picked := r.stateCommit(stateFile)
		if picked == nil {
			return failf(128, "error: no cherry-pick or revert in progress\nfatal: %s failed\n", cmd)
		}
		if cont {
			if err := r.errUnmergedCommit(); err != nil {
				return err
			}
			return r.finishPick(picked, revert)
		}
		head, err := r.headCommit()
		if err != nil {
			return err
		}
		if err := r.resetHard(head); err != nil {
			return err
		}
		r.clearOperationState()
		return nil
	}
	if len(revs) == 0 {
		return &exitError{code: 129, msg: fmt.Sprintf("usage: git %s [<options>] <commit>...\n", cmd)}
	}
	if err := r.errUnmerged(verb, cmd+" failed"); err != nil {
		return err
	}
	for _, rev := range revs {
		c, err := r.resolveCommit(rev)
		if err != nil {
			return failf(128, "fatal: bad revision '%s'\n", rev)
		}
		if len(c.ParentHashes) > 1 {
			return failf(128, "error: commit %s is a merge but no -m option was given.\nfatal: %s failed\n", c.Hash, cmd)
		}
		if err := r.pickOne(c, revert, noCommit, recordOrigin); err != nil {
			return err
		}
	}
	return nil
}

func pickMessage(c *object.Commit, revert, recordOrigin bool) string {
	if revert {
		return fmt.Sprintf("Revert \"%s\"\n\nThis reverts commit %s.\n", subject(c.Message), c.Hash)
	}
	msg := cleanupMessage(c.Message)
	if recordOrigin {
		msg += "\n(cherry picked from commit " + c.Hash.String() + ")\n"
	}
	return msg
}

func (r *repo) pickOne(c *object.Commit, revert, noCommit, recordOrigin bool) error {
	g := r.g
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	if head == nil {
		return fatalf("your current branch appears to be broken")
	}
	var parent *object.Commit
	if len(c.ParentHashes) > 0 {
		if parent, err = r.CommitObject(c.ParentHashes[0]); err != nil {
			return err
		}
	}
	base, theirs := parent, c
	label := short(c.Hash) + " (" + subject(c.Message) + ")"
	if revert {
		base, theirs = c, parent
		label = "parent of " + label
	}
	baseSide, err := r.treeSide(base)
	if err != nil {
		return err
	}
	ours, err := r.treeSide(head)
	if err != nil {
		return err
	}
	theirSide, err := r.treeSide(theirs)
	if err != nil {
		return err
	}
	tm, err := r.mergeTrees(baseSide, ours, theirSide, "HEAD", label)
	if err != nil {
		return err
	}
	if err := r.applyMerge(tm, ours, "merge"); err != nil {
		return err
	}
	for _, m := range tm.messages {
		fmt.Fprintln(g.out, m)
	}
	if noCommit {
		return nil
	}
	cmd, stateFile, what := "cherry-pick", cherryPickFile, "apply"
	if revert {
		cmd, stateFile, what = "revert", revertFile, "revert"
	}
	msg := pickMessage(c, revert, recordOrigin)
	r.writeGitFile(origHeadFile, head.Hash.String()+"\n")
	if conflicts := tm.conflicted(); len(conflicts) > 0 {
		r.writeGitFile(stateFile, c.Hash.String()+"\n")
		r.writeGitFile(mergeMsgFile, msg+"\n# Conflicts:\n#\t"+strings.Join(conflicts, "\n#\t")+"\n")
		return failf(1, "error: could not %s %s... %s\n"+
			"hint: After resolving the conflicts, mark them with\n"+
			"hint: \"git add/rm <pathspec>\", then run\n"+
			"hint: \"git %s --continue\".\n"+
			"hint: You can instead skip this commit with \"git %s --skip\".\n"+
			"hint: To abort and get back to the state before \"git %s\",\n"+
			"hint: run \"git %s --abort\".\n"+
			"hint: Disable this message with \"git config set advice.mergeConflict false\"\n",
			what, short(c.Hash), subject(c.Message), cmd, cmd, cmd, cmd)
	}
	r.writeGitFile(mergeMsgFile, msg)
	r.writeGitFile(stateFile, c.Hash.String()+"\n")
	return r.finishPick(c, revert)
}

// finishPick commits a cherry-pick or revert whose changes are in the index.
func (r *repo) finishPick(c *object.Commit, revert bool) error {
	g := r.g
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
	cmd := "cherry-pick"
	if revert {
		cmd = "revert"
	}
	if len(changes(headSide, r.indexSide(idx), nil)) == 0 {
		fmt.Fprintf(g.err, "The previous %s is now empty, possibly due to conflict resolution.\n"+
			"If you wish to commit it anyway, use:\n\n    git commit --allow-empty\n\n"+
			"Otherwise, please use 'git %s --skip'\n", cmd, cmd)
		st, err := r.computeStatus(nil, true, true)
		if err != nil {
			return err
		}
		r.writeLongStatus(g.out, st, false)
		return &exitError{code: 1}
	}
	msg, _ := r.gitFile(mergeMsgFile)
	msg = cleanupMessage(stripComments(msg))
	author, err := r.signature("AUTHOR")
	if err != nil {
		return err
	}
	if !revert {
		author = &c.Author
	}
	committer, err := r.signature("COMMITTER")
	if err != nil {
		return err
	}
	h, err := r.commitIndex(msg, author, committer, []plumbing.Hash{head.Hash})
	if err != nil {
		return err
	}
	if err := r.setHead(h); err != nil {
		return err
	}
	r.clearOperationState()
	return r.printCommitSummary(h, true)
}
