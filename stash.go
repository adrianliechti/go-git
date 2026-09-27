package git

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// git stash, built like git's: the stash is a commit W of the worktree whose
// parents are HEAD, a commit I of the index, and with -u a commit U of the
// untracked files. refs/stash points at the newest W; its reflog is the
// stack of stashes.

const stashRef plumbing.ReferenceName = "refs/stash"

func (g *gitRun) stash(args []string) error {
	sub := "push"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	switch sub {
	case "push", "save":
		return r.stashPush(sub, args)
	case "list":
		return r.stashList(args)
	case "show":
		return r.stashShow(args)
	case "apply", "pop":
		return r.stashApply(sub, args)
	case "drop":
		quiet := false
		var rest []string
		for _, a := range args {
			if a == "-q" || a == "--quiet" {
				quiet = true
			} else {
				rest = append(rest, a)
			}
		}
		return r.stashDrop(rest, quiet)
	case "clear":
		r.Storer.RemoveReference(stashRef)
		r.deleteReflog(stashRef)
		return nil
	case "branch":
		return r.stashBranch(args)
	}
	return &exitError{code: 129, msg: "error: unknown subcommand: `" + sub + "'\n" + commandUsage("stash")}
}

func (r *repo) stashEntries() []reflogEntry {
	entries := r.readReflog(stashRef)
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

// stashArg resolves a stash argument ("", "1", "stash@{1}") to its index,
// display name, and commit.
func (r *repo) stashArg(args []string) (int, string, *object.Commit, error) {
	entries := r.stashEntries()
	if len(entries) == 0 {
		return 0, "", nil, failf(1, "No stash entries found.\n")
	}
	n, name := 0, "refs/stash@{0}"
	if len(args) > 0 {
		arg := args[0]
		if v, err := strconv.Atoi(arg); err == nil {
			n = v
		} else if inner, ok := strings.CutPrefix(arg, "stash@{"); ok {
			v, err := strconv.Atoi(strings.TrimSuffix(inner, "}"))
			if err != nil {
				return 0, "", nil, fatalf("%s is not a valid reference", arg)
			}
			n = v
		} else {
			return 0, "", nil, failf(1, "error: %s is not a valid reference\n", arg)
		}
		name = fmt.Sprintf("stash@{%d}", n)
		if n >= len(entries) {
			return 0, "", nil, failf(1, "error: %s is not a valid reference\n", arg)
		}
	}
	c, err := r.CommitObject(entries[n].new)
	if err != nil {
		return 0, "", nil, err
	}
	return n, name, c, nil
}

func (r *repo) stashPush(sub string, args []string) error {
	g := r.g
	var keepIndex, untracked, all, quiet bool
	message := ""
	var paths []string
	args = splitFlags(args, "kuaq", "m")
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			paths = append(paths, args[i+1:]...)
			i = len(args)
		case a == "-k" || a == "--keep-index":
			keepIndex = true
		case a == "--no-keep-index":
			keepIndex = false
		case a == "-u" || a == "--include-untracked":
			untracked = true
		case a == "-a" || a == "--all":
			untracked, all = true, true
		case a == "-q" || a == "--quiet":
			quiet = true
		case (a == "-m" || a == "--message") && i+1 < len(args):
			i++
			message = args[i]
		case strings.HasPrefix(a, "--message="):
			message = strings.TrimPrefix(a, "--message=")
		case a == "-p" || a == "--patch":
			return fatalf("git stash --patch needs an interactive terminal")
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		case sub == "save":
			message = strings.TrimSpace(message + " " + a)
		default:
			paths = append(paths, a)
		}
	}
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	if head == nil {
		return fatalf("You do not have the initial commit yet")
	}
	specs, err := r.pathspecs(paths)
	if err != nil {
		return err
	}
	headSide, err := r.treeSide(head)
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	if len(unmergedPaths(idx)) > 0 {
		return failf(1, "error: could not write index\nCannot save the current index state\n")
	}
	index := r.indexSide(idx)

	// I: the whole index, even with a pathspec, as git records it.
	iSide := copySide(headSide)
	for _, c := range changes(headSide, index, nil) {
		if c.to == nil {
			delete(iSide, c.path)
		} else {
			iSide[c.path] = *c.to
		}
	}
	// W: I with the selected worktree changes of tracked files.
	var paths2 []string
	for p := range index {
		paths2 = append(paths2, p)
	}
	work, err := r.worktreeSide(paths2)
	if err != nil {
		return err
	}
	wSide := copySide(iSide)
	for _, c := range changes(index, work, specs) {
		if c.from == nil {
			continue // untracked
		}
		if c.to == nil {
			delete(wSide, c.path)
		} else {
			data, err := c.to.data()
			if err != nil {
				return err
			}
			e, err := r.writeBlob(data, c.to.mode)
			if err != nil {
				return err
			}
			wSide[c.path] = e
		}
	}
	var untrackedFiles []string
	if untracked {
		m, err := r.ignoreMatcher()
		if err != nil {
			return err
		}
		files, err := r.walkFiles("", m, all)
		if err != nil {
			return err
		}
		for _, f := range files {
			if _, tracked := index[f.path]; !tracked && !f.repo && (all || !f.ignored) && matchAny(specs, f.path) {
				untrackedFiles = append(untrackedFiles, f.path)
			}
		}
	}
	if len(changes(headSide, wSide, nil)) == 0 && len(changes(headSide, iSide, nil)) == 0 && len(untrackedFiles) == 0 {
		fmt.Fprintln(g.out, "No local changes to save")
		return nil
	}

	branch, _ := r.branchName()
	if branch == "" {
		branch = "(no branch)"
	}
	base := fmt.Sprintf("%s: %s %s", branch, short(head.Hash), subject(head.Message))
	sig, err := r.signature("COMMITTER")
	if err != nil {
		return err
	}
	author, err := r.signature("AUTHOR")
	if err != nil {
		return err
	}
	commit := func(s side, msg string, parents []plumbing.Hash) (plumbing.Hash, error) {
		tree, err := r.writeTree(s)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return r.storeObject(&object.Commit{Author: *author, Committer: *sig, Message: msg, TreeHash: tree, ParentHashes: parents})
	}
	// Only the WIP commit's message lacks a trailing newline, as in git.
	iHash, err := commit(iSide, "index on "+base+"\n", []plumbing.Hash{head.Hash})
	if err != nil {
		return err
	}
	parents := []plumbing.Hash{head.Hash, iHash}
	if len(untrackedFiles) > 0 {
		uSide := side{}
		for _, p := range untrackedFiles {
			e, _, err := r.worktreeEntry(p)
			if err != nil {
				return err
			}
			data, _ := e.data()
			if uSide[p], err = r.writeBlob(data, e.mode); err != nil {
				return err
			}
		}
		uHash, err := commit(uSide, "untracked files on "+base+"\n", nil)
		if err != nil {
			return err
		}
		parents = append(parents, uHash)
	}
	wMsg := "WIP on " + base
	if message != "" {
		wMsg = "On " + branch + ": " + message
	}
	wHash, err := commit(wSide, wMsg, parents)
	if err != nil {
		return err
	}
	old := r.refHash(stashRef)
	if err := r.Storer.SetReference(plumbing.NewHashReference(stashRef, wHash)); err != nil {
		return err
	}
	r.appendReflog(stashRef, old, wHash, wMsg)

	// Reset the stashed paths, then remove stashed untracked files.
	if len(specs) == 0 {
		if err := r.resetHard(head); err != nil {
			return err
		}
	} else {
		for _, c := range changes(headSide, wSide, specs) {
			if err := r.restorePath(c.path, c.from); err != nil {
				return err
			}
		}
	}
	for _, p := range untrackedFiles {
		if err := r.removeFile(p); err != nil {
			return err
		}
	}
	if keepIndex {
		for p, e := range iSide {
			if cur, ok := headSide[p]; ok && sameEntry(&cur, &e) {
				continue
			}
			if err := r.checkoutEntry(p, e); err != nil {
				return err
			}
		}
	}
	if !quiet {
		fmt.Fprintf(g.out, "Saved working directory and index state %s\n", wMsg)
	}
	return nil
}

func copySide(s side) side {
	out := make(side, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// restorePath makes p in the index and worktree match e (nil deletes it).
func (r *repo) restorePath(p string, e *entry) error {
	ix, err := r.readIndex()
	if err != nil {
		return err
	}
	if e == nil {
		removeEntry(ix, p)
		if err := r.removeFile(p); err != nil {
			return err
		}
	} else {
		info, err := r.writeFile(p, *e)
		if err != nil {
			return err
		}
		setEntry(ix, p, *e, info)
	}
	return r.writeIndex(ix)
}

// checkoutEntry writes e to the worktree and index.
func (r *repo) checkoutEntry(p string, e entry) error {
	return r.restorePath(p, &e)
}

func (r *repo) stashList(args []string) error {
	for i, e := range r.stashEntries() {
		fmt.Fprintf(r.g.out, "stash@{%d}: %s\n", i, e.msg)
	}
	return nil
}

func (r *repo) stashShow(args []string) error {
	var o diffOutput
	withUntracked := false
	var rest []string
	for _, a := range args {
		switch a {
		case "-p", "--patch", "-u":
			if a == "-u" {
				withUntracked = true
			} else {
				o.patch = true
			}
		case "--stat":
			o.stat = true
		case "--include-untracked":
			withUntracked = true
		case "--name-only":
			o.nameOnly = true
		case "--name-status":
			o.nameStat = true
		default:
			rest = append(rest, a)
		}
	}
	if !o.any() {
		o.stat = true
	}
	_, _, w, err := r.stashArg(rest)
	if err != nil {
		return err
	}
	base, err := r.CommitObject(w.ParentHashes[0])
	if err != nil {
		return err
	}
	a, err := r.treeSide(base)
	if err != nil {
		return err
	}
	b, err := r.treeSide(w)
	if err != nil {
		return err
	}
	if withUntracked && len(w.ParentHashes) > 2 {
		u, err := r.CommitObject(w.ParentHashes[2])
		if err != nil {
			return err
		}
		us, err := r.treeSide(u)
		if err != nil {
			return err
		}
		for p, e := range us {
			b[p] = e
		}
	}
	diffs, err := computeDiffs(changes(a, b, nil), false)
	if err != nil {
		return err
	}
	writeDiffs(r.g.out, diffs, o)
	return nil
}

func (r *repo) stashApply(sub string, args []string) error {
	g := r.g
	restoreIndex, quiet := false, false
	var rest []string
	for _, a := range args {
		switch a {
		case "--index":
			restoreIndex = true
		case "-q", "--quiet":
			quiet = true
		default:
			rest = append(rest, a)
		}
	}
	n, name, w, err := r.stashArg(rest)
	if err != nil {
		return err
	}
	if err := r.errUnmerged("Applying a stash", "Exiting because of an unresolved conflict."); err != nil {
		return err
	}
	base, err := r.CommitObject(w.ParentHashes[0])
	if err != nil {
		return err
	}
	baseSide, err := r.treeSide(base)
	if err != nil {
		return err
	}
	theirs, err := r.treeSide(w)
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	ours := r.indexSide(idx)
	// Untracked files must not already exist.
	var uSide side
	if len(w.ParentHashes) > 2 {
		u, err := r.CommitObject(w.ParentHashes[2])
		if err != nil {
			return err
		}
		if uSide, err = r.treeSide(u); err != nil {
			return err
		}
		for p := range uSide {
			if _, err := r.wt.Stat(p); err == nil {
				return failf(1, "%s already exists, no checkout\nerror: could not restore untracked files from stash\n", p)
			}
		}
	}
	tm, err := r.mergeTrees(baseSide, ours, theirs, "Updated upstream", "Stashed changes")
	if err != nil {
		return err
	}
	if err := r.applyMerge(tm, ours, "merge"); err != nil {
		return err
	}
	for _, m := range tm.messages {
		fmt.Fprintln(g.out, m)
	}
	// Unstage the applied changes, except files the stash added.
	idx, err = r.readIndex()
	if err != nil {
		return err
	}
	var iSide side
	if restoreIndex {
		ic, err := r.CommitObject(w.ParentHashes[1])
		if err != nil {
			return err
		}
		if iSide, err = r.treeSide(ic); err != nil {
			return err
		}
	}
	for _, mp := range tm.paths {
		if mp.conflict != "" {
			continue
		}
		_, inBase := baseSide[mp.path]
		cur, inOurs := ours[mp.path]
		switch {
		case restoreIndex:
			if e, ok := iSide[mp.path]; ok {
				setEntry(idx, mp.path, e, nil)
			} else if inOurs {
				removeEntry(idx, mp.path)
			}
		case !inBase && mp.result != nil:
			// Newly added files stay added.
		case inOurs:
			setEntry(idx, mp.path, cur, nil)
		default:
			removeEntry(idx, mp.path)
		}
	}
	if err := r.writeIndex(idx); err != nil {
		return err
	}
	for p, e := range uSide {
		if _, err := r.writeFile(p, e); err != nil {
			return err
		}
	}
	if !quiet {
		st, err := r.computeStatus(nil, true, true)
		if err != nil {
			return err
		}
		r.writeLongStatus(g.out, st, false)
	}
	if len(tm.conflicted()) > 0 {
		if sub == "pop" {
			return failf(1, "The stash entry is kept in case you need it again.\n")
		}
		return &exitError{code: 1}
	}
	if sub == "pop" {
		return r.dropStash(n, name, quiet)
	}
	return nil
}

func (r *repo) stashDrop(args []string, quiet bool) error {
	n, name, _, err := r.stashArg(args)
	if err != nil {
		return err
	}
	return r.dropStash(n, name, quiet)
}

func (r *repo) dropStash(n int, name string, quiet bool) error {
	entries := r.readReflog(stashRef) // oldest first
	i := len(entries) - 1 - n
	dropped := entries[i].new
	entries = append(entries[:i], entries[i+1:]...)
	if len(entries) == 0 {
		r.Storer.RemoveReference(stashRef)
		r.deleteReflog(stashRef)
	} else {
		if err := r.Storer.SetReference(plumbing.NewHashReference(stashRef, entries[len(entries)-1].new)); err != nil {
			return err
		}
		if err := r.writeReflog(stashRef, entries); err != nil {
			return err
		}
	}
	if !quiet {
		fmt.Fprintf(r.g.out, "Dropped %s (%s)\n", name, dropped)
	}
	return nil
}

func (r *repo) stashBranch(args []string) error {
	if len(args) == 0 {
		return fatalf("No branch name specified")
	}
	n, name, w, err := r.stashArg(args[1:])
	if err != nil {
		return err
	}
	if err := r.switchBranch(args[0], true, w.ParentHashes[0].String(), false, false); err != nil {
		return err
	}
	if err := r.stashApply("apply", []string{"--index", fmt.Sprint(n)}); err != nil {
		return err
	}
	return r.dropStash(n, name, false)
}
