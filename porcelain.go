package git

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-billy/v5"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	format "github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

func (g *gitRun) init(args []string) error {
	quiet, bare, branch, dir := false, false, "", "."
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--bare":
			bare = true
		case (a == "-b" || a == "--initial-branch") && i+1 < len(args):
			i++
			branch = args[i]
		case strings.HasPrefix(a, "--initial-branch="):
			branch = strings.TrimPrefix(a, "--initial-branch=")
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			dir = a
		}
	}
	top := g.abs(dir)
	if branch == "" {
		branch, _ = g.configLookup("init.defaultBranch", "")
	}
	if branch == "" {
		branch = "master"
	}
	wt := newBillyFS(g.fsys, fsName(top))
	if err := wt.MkdirAll(".", 0777); err != nil {
		return fatalf("cannot mkdir %s: %v", dir, err)
	}
	gitDir := path.Join(top, ".git")
	if bare {
		gitDir = top
	}
	verb := "Initialized empty"
	if _, err := fs.Stat(g.fsys, fsName(path.Join(gitDir, "HEAD"))); err == nil {
		verb = "Reinitialized existing"
	} else {
		dot, worktree := billy.Filesystem(nil), billy.Filesystem(wt)
		if bare {
			dot, worktree = wt, nil
		} else {
			dot, _ = wt.Chroot(".git")
		}
		st := filesystem.NewStorage(dot, cache.NewObjectLRUDefault())
		_, err := gogit.InitWithOptions(st, worktree, gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)})
		if err != nil {
			return fatalf("%v", err)
		}
		// Replace go-git's config with the one git init writes.
		cfg := format.New()
		cfg.SetOption("core", "", "repositoryformatversion", "0")
		cfg.SetOption("core", "", "filemode", "true")
		cfg.SetOption("core", "", "bare", fmt.Sprint(bare))
		if !bare {
			cfg.SetOption("core", "", "logallrefupdates", "true")
		}
		if err := g.writeConfig(fsName(path.Join(gitDir, "config")), cfg); err != nil {
			return err
		}
	}
	if !quiet {
		fmt.Fprintf(g.out, "%s Git repository in %s/\n", verb, gitDir)
	}
	return nil
}

func (g *gitRun) add(args []string) error {
	all, update, force := false, false, false
	var paths []string
	for i, a := range args {
		if a == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		switch a {
		case "-A", "--all":
			all = true
		case "-u", "--update":
			update = true
		case "-f", "--force":
			force = true
		case "-v", "--verbose":
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			paths = append(paths, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	if len(paths) == 0 && !all && !update {
		fmt.Fprint(g.err, "Nothing specified, nothing added.\nhint: Maybe you wanted to say 'git add .'?\n"+
			"hint: Disable this message with \"git config set advice.addEmptyPathspec false\"\n")
		return nil
	}
	specs, err := r.pathspecs(paths)
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		specs = []string{""}
	}
	return r.stage(specs, paths, update, force)
}

// stage implements add: new and modified files are written, and deleted files
// are removed from the index. With update, untracked files are left alone.
func (r *repo) stage(specs, args []string, update, force bool) error {
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	m, err := r.ignoreMatcher()
	if err != nil {
		return err
	}
	files, err := r.walkFiles("", m, force)
	if err != nil {
		return err
	}
	tracked := r.indexSide(idx)
	for p := range unmergedPaths(idx) {
		tracked[p] = entry{}
	}
	matched := make([]bool, len(specs))
	var ignored []string
	selected := map[string]bool{}
	for _, f := range files {
		_, isTracked := tracked[f.path]
		for i, s := range specs {
			if !matchPath(s, f.path) {
				continue
			}
			matched[i] = true
			switch {
			case isTracked:
				selected[f.path] = true
			case update:
			case f.ignored && !force:
				if s == f.path {
					ignored = append(ignored, r.display(f.path))
				}
			default:
				selected[f.path] = true
			}
		}
	}
	for p := range tracked {
		for i, s := range specs {
			if matchPath(s, p) {
				matched[i] = true
				selected[p] = true
			}
		}
	}
	for i, ok := range matched {
		if !ok && specs[i] != "" && len(ignored) == 0 {
			return fatalf("pathspec '%s' did not match any files", args[i])
		}
	}
	for p := range selected {
		if err := r.stageFile(idx, p); err != nil {
			return err
		}
	}
	if err := r.writeIndex(idx); err != nil {
		return err
	}
	if len(ignored) > 0 {
		return failf(1, "The following paths are ignored by one of your .gitignore files:\n%s\n"+
			"hint: Use -f if you really want to add them.\n"+
			"hint: Disable this message with \"git config set advice.addIgnoredFile false\"\n", strings.Join(ignored, "\n"))
	}
	return nil
}

func (g *gitRun) rm(args []string) error {
	cached, recursive, force, quiet := false, false, false, false
	var paths []string
	for i, a := range args {
		if a == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		switch a {
		case "--cached":
			cached = true
		case "-r":
			recursive = true
		case "-f", "--force":
			force = true
		case "-q", "--quiet":
			quiet = true
		case "-rf", "-fr":
			recursive, force = true, true
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		return usagef("usage: git rm [<options>] [--] <file>...")
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	specs, err := r.pathspecs(paths)
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	headSide, err := r.treeSide(head)
	if err != nil {
		return err
	}
	var targets []string
	seen := map[string]bool{}
	unmerged := unmergedPaths(idx)
	for i, s := range specs {
		found := false
		for _, e := range idx.Entries {
			if !matchPath(s, e.Name) {
				continue
			}
			if seen[e.Name] {
				found = true
				continue
			}
			seen[e.Name] = true
			if e.Name != s && !recursive && !strings.ContainsAny(s, "*?[") {
				return fatalf("not removing '%s' recursively without -r", paths[i])
			}
			found = true
			targets = append(targets, e.Name)
		}
		if !found {
			return fatalf("pathspec '%s' did not match any files", paths[i])
		}
	}
	sort.Strings(targets)
	if !force {
		var staged, local, both []string
		for _, p := range targets {
			if _, conflicted := unmerged[p]; conflicted {
				continue // removing resolves the conflict
			}
			ie, _ := idx.Entry(p)
			he, inHead := headSide[p]
			we, inWork, err := r.worktreeEntry(p)
			if err != nil {
				return err
			}
			stagedDiff := !inHead || he.hash != ie.Hash
			localDiff := inWork && we.hash != ie.Hash
			switch {
			case stagedDiff && localDiff:
				both = append(both, p)
			case stagedDiff && !cached:
				staged = append(staged, p)
			case localDiff && !cached:
				local = append(local, p)
			}
		}
		var msg strings.Builder
		report := func(files []string, what, hint string) {
			if len(files) == 0 {
				return
			}
			noun := "file has"
			if len(files) > 1 {
				noun = "files have"
			}
			fmt.Fprintf(&msg, "error: the following %s %s:\n    %s\n%s\n", noun, what, strings.Join(files, "\n    "), hint)
		}
		report(both, "staged content different from both the\nfile and the HEAD", "(use -f to force removal)")
		report(staged, "changes staged in the index", "(use --cached to keep the file, or -f to force removal)")
		report(local, "local modifications", "(use --cached to keep the file, or -f to force removal)")
		if msg.Len() > 0 {
			return failf(1, "%s", msg.String())
		}
	}
	for _, p := range targets {
		if !quiet {
			fmt.Fprintf(g.out, "rm '%s'\n", p)
		}
		removeEntry(idx, p)
		if !cached {
			if err := r.removeFile(p); err != nil {
				return err
			}
		}
	}
	return r.writeIndex(idx)
}

func (g *gitRun) commit(args []string) error {
	var messages []string
	all, quiet, allowEmpty, allowEmptyMessage, amend, noEdit, haveMessage := false, false, false, false, false, false, false
	readFile := func(name string) error {
		var data []byte
		var err error
		if name == "-" {
			data, err = io.ReadAll(g.stdin)
		} else {
			data, err = fs.ReadFile(g.fsys, fsName(g.abs(name)))
		}
		if err != nil {
			return fatalf("could not read log file '%s': %v", name, err)
		}
		messages = append(messages, string(data))
		haveMessage = true
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		// value returns an option's argument: attached, or the next word.
		value := func(attached string) (string, error) {
			if attached != "" {
				return attached, nil
			}
			if i+1 >= len(args) {
				return "", usagef("error: switch `%s' requires a value", strings.TrimLeft(a, "-"))
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--message" || strings.HasPrefix(a, "--message="):
			v, err := value(strings.TrimPrefix(strings.TrimPrefix(a, "--message"), "="))
			if err != nil {
				return err
			}
			messages, haveMessage = append(messages, v), true
		case a == "--file" || strings.HasPrefix(a, "--file="):
			v, err := value(strings.TrimPrefix(strings.TrimPrefix(a, "--file"), "="))
			if err != nil {
				return err
			}
			if err := readFile(v); err != nil {
				return err
			}
		case a == "--all":
			all = true
		case a == "--quiet":
			quiet = true
		case a == "--allow-empty":
			allowEmpty = true
		case a == "--allow-empty-message":
			allowEmptyMessage = true
		case a == "--amend":
			amend = true
		case a == "--no-edit":
			noEdit = true
		case len(a) > 1 && a[0] == '-' && a[1] != '-':
			// A cluster of short options such as -qam "message".
		cluster:
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'a':
					all = true
				case 'q':
					quiet = true
				case 'm', 'F':
					v, err := value(a[j+1:])
					if err != nil {
						return err
					}
					if a[j] == 'm' {
						messages, haveMessage = append(messages, v), true
					} else if err := readFile(v); err != nil {
						return err
					}
					break cluster
				default:
					return usagef("error: unknown switch `%c'", a[j])
				}
			}
		default:
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	if all {
		if err := r.stage([]string{""}, nil, true, false); err != nil {
			return err
		}
	}
	if err := r.errUnmergedCommit(); err != nil {
		return err
	}
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	base := head
	var parents []plumbing.Hash
	if head != nil {
		parents = []plumbing.Hash{head.Hash}
	}
	mergeHead := r.stateCommit(mergeHeadFile)
	picked := r.stateCommit(cherryPickFile)
	reverted := r.stateCommit(revertFile)
	if mergeHead != nil {
		parents = append(parents, mergeHead.Hash)
	}
	if amend {
		if head == nil {
			return fatalf("You have nothing to amend.")
		}
		parents, base = head.ParentHashes, nil
		if len(parents) > 0 {
			if base, err = r.CommitObject(parents[0]); err != nil {
				return err
			}
		}
	}
	if !allowEmpty && !amend && mergeHead == nil {
		idx, err := r.readIndex()
		if err != nil {
			return err
		}
		baseSide, err := r.treeSide(base)
		if err != nil {
			return err
		}
		if len(changes(baseSide, r.indexSide(idx), nil)) == 0 {
			st, err := r.computeStatus(nil, true, true)
			if err != nil {
				return err
			}
			r.writeLongStatus(g.out, st, true)
			return failf(1, "")
		}
	}
	msg := strings.Join(messages, "\n\n")
	if !haveMessage {
		// A prepared message is used as is with --no-edit; otherwise it is
		// treated as if an editor returned it unchanged, which strips
		// comment lines.
		prepared := ""
		for _, file := range []string{mergeMsgFile, squashMsgFile} {
			if m, ok := r.gitFile(file); ok {
				prepared = m
				if !noEdit {
					prepared = stripComments(m)
				}
				break
			}
		}
		switch {
		case prepared != "":
			msg = prepared
		case amend && noEdit:
			msg = head.Message
		default:
			return fatalf("no commit message given; use -m or -F (no editor is available)")
		}
	}
	msg = cleanupMessage(msg)
	if msg == "" && !allowEmptyMessage {
		return failf(1, "Aborting commit due to empty commit message.\n")
	}
	author, err := r.signature("AUTHOR")
	if err != nil {
		return err
	}
	switch {
	case amend:
		author = &head.Author
	case picked != nil:
		author = &picked.Author
	}
	committer, err := r.signature("COMMITTER")
	if err != nil {
		return err
	}
	h, err := r.commitIndex(msg, author, committer, parents)
	if err != nil {
		return err
	}
	action := "commit"
	switch {
	case amend:
		action = "commit (amend)"
	case len(parents) == 0:
		action = "commit (initial)"
	case len(parents) > 1:
		action = "commit (merge)"
	}
	if err := r.setHead(h, action+": "+subject(msg)); err != nil {
		return err
	}
	r.clearOperationState()
	_ = reverted
	if quiet {
		return nil
	}
	return r.printCommitSummary(h, amend || picked != nil)
}

// printCommitSummary prints the "[branch hash] subject" block that commit,
// cherry-pick, and revert show. Merge commits get no diffstat.
func (r *repo) printCommitSummary(h plumbing.Hash, showDate bool) error {
	g := r.g
	c, err := r.CommitObject(h)
	if err != nil {
		return err
	}
	branch, err := r.branchName()
	if err != nil {
		return err
	}
	if branch == "" {
		branch = "detached HEAD"
	}
	root := ""
	if len(c.ParentHashes) == 0 {
		root = " (root-commit)"
	}
	fmt.Fprintf(g.out, "[%s%s %s] %s\n", branch, root, short(h), subject(c.Message))
	if ident(c.Author) != ident(c.Committer) {
		fmt.Fprintf(g.out, " Author: %s\n", ident(c.Author))
	}
	if showDate {
		fmt.Fprintf(g.out, " Date: %s\n", gitDate(c.Author.When))
	}
	if len(c.ParentHashes) > 1 {
		return nil
	}
	diffs, err := r.commitDiffs(c, nil)
	if err != nil {
		return err
	}
	if len(diffs) > 0 {
		writeSummary(g.out, diffs)
		writeModeSummary(g.out, diffs)
	}
	return nil
}

// stripComments drops "#" lines from a prepared message, as git's default
// cleanup does for messages that went through the editor.
func stripComments(msg string) string {
	var lines []string
	for _, l := range strings.Split(msg, "\n") {
		if !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// errUnmergedCommit is commit's refusal while paths are unmerged; git lists
// the paths on stdout after the error.
func (r *repo) errUnmergedCommit() error {
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	un := unmergedPaths(idx)
	if len(un) == 0 {
		return nil
	}
	fmt.Fprint(r.g.err, "error: Committing is not possible because you have unmerged files.\n"+
		"hint: Fix them up in the work tree, and then use 'git add/rm <file>'\n"+
		"hint: as appropriate to mark resolution and make a commit.\n"+
		"fatal: Exiting because of an unresolved conflict.\n")
	for _, p := range sortedKeys(un) {
		fmt.Fprintf(r.g.out, "U\t%s\n", p)
	}
	return &exitError{code: 128}
}

func (g *gitRun) status(args []string) error {
	short, porcelain, showBranch, ignored, untracked := false, false, false, false, "normal"
	var paths []string
	for _, a := range args {
		switch {
		case a == "-s" || a == "--short":
			short = true
		case a == "--porcelain" || a == "--porcelain=v1":
			porcelain = true
		case a == "-b" || a == "--branch":
			showBranch = true
		case a == "-sb" || a == "-bs":
			short, showBranch = true, true
		case a == "--long":
			short, porcelain = false, false
		case a == "--ignored" || a == "--ignored=traditional":
			ignored = true
		case a == "-uall" || a == "--untracked-files=all":
			untracked = "all"
		case a == "-uno" || a == "--untracked-files=no":
			untracked = "no"
		case a == "-unormal" || a == "--untracked-files=normal" || a == "--":
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
	st, err := r.computeStatus(specs, untracked == "normal", untracked != "no")
	if err != nil {
		return err
	}
	if ignored {
		if err := r.addIgnored(st, specs, untracked != "all"); err != nil {
			return err
		}
	}
	if !short && !porcelain {
		r.writeLongStatus(g.out, st, false)
		return nil
	}
	name := r.display
	if porcelain {
		name = func(p string) string { return p }
	}
	if showBranch {
		switch {
		case st.branch == "":
			fmt.Fprintln(g.out, "## HEAD (no branch)")
		case st.head == nil:
			fmt.Fprintf(g.out, "## No commits yet on %s\n", st.branch)
		default:
			line := "## " + st.branch
			if t := r.tracking(st.branch); t != nil {
				line += "..." + t.upstream
				if s := t.counts(); s != "" {
					line += " [" + s + "]"
				}
			}
			fmt.Fprintln(g.out, line)
		}
	}
	for _, f := range st.tracked {
		p := qs(name(f.path))
		if f.oldPath != "" {
			p = qs(name(f.oldPath)) + " -> " + p
		}
		fmt.Fprintf(g.out, "%c%c %s\n", f.staged, f.unstaged, p)
	}
	for _, p := range st.untracked {
		fmt.Fprintf(g.out, "?? %s\n", qs(name(p)))
	}
	for _, p := range st.ignored {
		fmt.Fprintf(g.out, "!! %s\n", qs(name(p)))
	}
	return nil
}

// statusName is a long-status path, "old -> new" for renames.
func (r *repo) statusName(f fileStatus) string {
	if f.oldPath != "" {
		return q(r.display(f.oldPath)) + " -> " + q(r.display(f.path))
	}
	return q(r.display(f.path))
}

// writeOperationState describes a merge, cherry-pick, or revert in progress.
func (r *repo) writeOperationState(w io.Writer, st *repoStatus) {
	unmerged := st.hasUnmerged()
	if _, ok := r.gitFile(path.Join(amDir, "queue")); ok {
		fmt.Fprint(w, "You are in the middle of an am session.\n"+
			"  (fix conflicts and then run \"git am --continue\")\n"+
			"  (use \"git am --skip\" to skip this patch)\n"+
			"  (use \"git am --abort\" to restore the original branch)\n\n")
		return
	}
	if _, ok := r.gitFile(mergeHeadFile); ok {
		if unmerged {
			fmt.Fprint(w, "You have unmerged paths.\n  (fix conflicts and run \"git commit\")\n  (use \"git merge --abort\" to abort the merge)\n\n")
		} else {
			fmt.Fprint(w, "All conflicts fixed but you are still merging.\n  (use \"git commit\" to conclude merge)\n\n")
		}
		return
	}
	for _, op := range []struct{ file, verb, cmd string }{
		{cherryPickFile, "cherry-picking", "cherry-pick"},
		{revertFile, "reverting", "revert"},
	} {
		c := r.stateCommit(op.file)
		if c == nil {
			continue
		}
		fmt.Fprintf(w, "You are currently %s commit %s.\n", op.verb, short(c.Hash))
		if unmerged {
			fmt.Fprintf(w, "  (fix conflicts and run \"git %s --continue\")\n", op.cmd)
		} else {
			fmt.Fprintf(w, "  (all conflicts fixed: run \"git %s --continue\")\n", op.cmd)
		}
		fmt.Fprintf(w, "  (use \"git %s --skip\" to skip this patch)\n", op.cmd)
		fmt.Fprintf(w, "  (use \"git %s --abort\" to cancel the %s operation)\n\n", op.cmd, op.cmd)
		return
	}
}

func ident(s object.Signature) string { return s.Name + " <" + s.Email + ">" }

// writeLongStatus prints git status output; commit uses slightly different
// wording for an unborn branch.
func (r *repo) writeLongStatus(w io.Writer, st *repoStatus, forCommit bool) {
	if rb := r.loadRebase(); rb != nil {
		r.writeRebaseStatus(w, rb, st.hasUnmerged())
		r.writeStatusBody(w, st, forCommit)
		return
	}
	if st.branch == "" {
		fmt.Fprintf(w, "HEAD detached at %s\n", short(st.head.Hash))
	} else {
		fmt.Fprintf(w, "On branch %s\n", st.branch)
		if st.head != nil {
			if t := r.tracking(st.branch); t != nil {
				fmt.Fprintf(w, "%s\n", t.longStatus())
			}
		}
	}
	r.writeOperationState(w, st)
	r.writeStatusBody(w, st, forCommit)
}

// writeStatusBody writes the sections after the branch and state header.
func (r *repo) writeStatusBody(w io.Writer, st *repoStatus, forCommit bool) {
	if st.head == nil {
		if forCommit {
			fmt.Fprint(w, "\nInitial commit\n\n")
		} else {
			fmt.Fprint(w, "\nNo commits yet\n\n")
		}
	}
	labels := map[byte]string{'A': "new file:", 'M': "modified:", 'D': "deleted:", 'R': "renamed:"}
	if st.hasStaged() {
		fmt.Fprintln(w, "Changes to be committed:")
		_, merging := r.gitFile(mergeHeadFile)
		switch {
		case merging:
		case st.head == nil:
			fmt.Fprintln(w, `  (use "git rm --cached <file>..." to unstage)`)
		default:
			fmt.Fprintln(w, `  (use "git restore --staged <file>..." to unstage)`)
		}
		for _, f := range st.tracked {
			if f.staged != ' ' && f.unmerged == "" {
				fmt.Fprintf(w, "\t%-12s%s\n", labels[f.staged], r.statusName(f))
			}
		}
		fmt.Fprintln(w)
	}
	if st.hasUnmerged() {
		hint := `  (use "git add <file>..." to mark resolution)`
		for _, f := range st.tracked {
			if f.unmerged != "" && f.unmerged != "both modified:" && f.unmerged != "both added:" {
				hint = `  (use "git add/rm <file>..." as appropriate to mark resolution)`
			}
		}
		fmt.Fprintln(w, "Unmerged paths:")
		if !r.inOperation() && st.head != nil {
			fmt.Fprintln(w, `  (use "git restore --staged <file>..." to unstage)`)
		}
		fmt.Fprintln(w, hint)
		for _, f := range st.tracked {
			if f.unmerged != "" {
				fmt.Fprintf(w, "\t%-17s%s\n", f.unmerged, q(r.display(f.path)))
			}
		}
		fmt.Fprintln(w)
	}
	if st.hasUnstaged() {
		verb := "add"
		for _, f := range st.tracked {
			if f.unstaged == 'D' {
				verb = "add/rm"
			}
		}
		fmt.Fprintln(w, "Changes not staged for commit:")
		fmt.Fprintf(w, "  (use \"git %s <file>...\" to update what will be committed)\n", verb)
		fmt.Fprintln(w, `  (use "git restore <file>..." to discard changes in working directory)`)
		for _, f := range st.tracked {
			if strings.Contains(f.sub, "content") {
				fmt.Fprintln(w, `  (commit or discard the untracked or modified content in submodules)`)
				break
			}
		}
		for _, f := range st.tracked {
			if f.unstaged != ' ' && f.unmerged == "" {
				label := labels[f.unstaged]
				if f.sub != "" {
					label = labels['M']
				}
				line := fmt.Sprintf("\t%-12s%s", label, q(r.display(f.path)))
				if f.sub != "" {
					line += " " + f.sub
				}
				fmt.Fprintln(w, line)
			}
		}
		fmt.Fprintln(w)
	}
	if len(st.untracked) > 0 {
		fmt.Fprintln(w, "Untracked files:")
		fmt.Fprintln(w, `  (use "git add <file>..." to include in what will be committed)`)
		for _, p := range st.untracked {
			fmt.Fprintf(w, "\t%s\n", q(r.display(p)))
		}
		fmt.Fprintln(w)
	}
	if len(st.ignored) > 0 {
		fmt.Fprintln(w, "Ignored files:")
		fmt.Fprintln(w, `  (use "git add -f <file>..." to include in what will be committed)`)
		for _, p := range st.ignored {
			fmt.Fprintf(w, "\t%s\n", q(r.display(p)))
		}
		fmt.Fprintln(w)
	}
	switch {
	case st.hasStaged():
	case st.hasUnstaged() || st.hasUnmerged():
		fmt.Fprintln(w, `no changes added to commit (use "git add" and/or "git commit -a")`)
	case len(st.untracked) > 0:
		fmt.Fprintln(w, `nothing added to commit but untracked files present (use "git add" to track)`)
	case st.head == nil:
		fmt.Fprintln(w, `nothing to commit (create/copy files and use "git add" to track)`)
	default:
		fmt.Fprintln(w, "nothing to commit, working tree clean")
	}
}

func mustAncestor(a, b *object.Commit) bool {
	ok, err := a.IsAncestor(b)
	return err == nil && ok
}

// createBranch creates a branch at start; from names the start point in
// the reflog ("HEAD" for checkout -b, the current branch for git branch).
func (r *repo) createBranch(name, start string, from ...string) error {
	ref := plumbing.NewBranchReferenceName(name)
	if err := ref.Validate(); err != nil || strings.HasPrefix(name, "-") {
		return fatalf("'%s' is not a valid branch name", name)
	}
	if _, err := r.Storer.Reference(ref); err == nil {
		return fatalf("a branch named '%s' already exists", name)
	}
	c, err := r.resolveCommit(start)
	if err != nil {
		if start == "HEAD" {
			return fatalf("not a valid object name: 'HEAD'")
		}
		return fatalf("not a valid object name: '%s'", start)
	}
	msg := "branch: Created from " + start
	if len(from) > 0 {
		msg = "branch: Created from " + from[0]
	}
	return r.updateRef(ref, c.Hash, msg)
}

// switchBranch implements both git switch and branch-mode git checkout.
func (r *repo) switchBranch(name string, create bool, start string, detach bool, quiet bool) error {
	g := r.g
	if err := r.errNeedsMerge(); err != nil {
		return err
	}
	current, err := r.branchName()
	if err != nil {
		return err
	}
	if create {
		if err := r.createBranch(name, start); err != nil {
			return err
		}
	}
	var target *object.Commit
	ref := plumbing.NewBranchReferenceName(name)
	isBranch := false
	if !detach {
		if b, err := r.Storer.Reference(ref); err == nil {
			isBranch = true
			target, err = r.CommitObject(b.Hash())
			if err != nil {
				return err
			}
		}
	}
	if !isBranch {
		if target, err = r.resolveCommit(name); err != nil {
			return failf(1, "error: pathspec '%s' did not match any file(s) known to git\n", name)
		}
	}
	if isBranch && !(name == current && !create) {
		if p, used := r.branchUsedElsewhere(ref); used {
			return fatalf("'%s' is already used by worktree at '%s'", name, p)
		}
	}
	if isBranch && name == current && !create {
		r.appendReflog(plumbing.HEAD, target.Hash, target.Hash, "checkout: moving from "+name+" to "+name)
		if !quiet {
			fmt.Fprintf(g.err, "Already on '%s'\n", name)
		}
		return nil
	}
	if err := r.switchTo(target, "checkout", false); err != nil {
		if create {
			r.deleteRef(ref)
		}
		return err
	}
	msg := "checkout: moving from " + r.headName() + " to " + name
	if r.quietCheckout && isBranch {
		err = r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, ref))
	} else if isBranch {
		err = r.pointHead(ref, plumbing.ZeroHash, msg)
	} else {
		err = r.pointHead("", target.Hash, msg)
	}
	if err != nil || quiet {
		return err
	}
	defer r.showLocalChanges()
	switch {
	case create:
		fmt.Fprintf(g.err, "Switched to a new branch '%s'\n", name)
	case isBranch:
		fmt.Fprintf(g.err, "Switched to branch '%s'\n", name)
	default:
		fmt.Fprintf(g.err, "HEAD is now at %s %s\n", short(target.Hash), subject(target.Message))
	}
	return nil
}

// showLocalChanges lists tracked files that differ from HEAD, as git does
// after switching branches with changes carried over.
func (r *repo) showLocalChanges() {
	st, err := r.computeStatus(nil, false, false)
	if err != nil {
		return
	}
	for _, f := range st.tracked {
		code := byte('M')
		switch {
		case f.staged == 'A' && f.unstaged != 'D':
			code = 'A'
		case f.staged == 'A':
			continue
		case f.staged == 'D' || f.unstaged == 'D':
			code = 'D'
		}
		fmt.Fprintf(r.g.out, "%c\t%s\n", code, f.path)
	}
}

// dwimRemote finds the single remote-tracking branch <remote>/<name>, as
// git checkout and git switch do for a branch that exists only remotely.
func (r *repo) dwimRemote(name string) string {
	remotes, err := r.remotes()
	if err != nil {
		return ""
	}
	found := ""
	for _, rm := range remotes {
		if _, err := r.Storer.Reference(plumbing.ReferenceName("refs/remotes/" + rm.name + "/" + name)); err == nil {
			if found != "" {
				return ""
			}
			found = rm.name + "/" + name
		}
	}
	return found
}

// checkoutTracking creates branch name from a remote-tracking branch, sets
// it as upstream, and switches to it.
func (r *repo) checkoutTracking(name, remote string, quiet bool) error {
	if err := r.switchBranch(name, true, remote, false, true); err != nil {
		return err
	}
	remoteName, rest, _ := strings.Cut(remote, "/")
	if err := r.setUpstream(name, remoteName, plumbing.NewBranchReferenceName(rest)); err != nil {
		return err
	}
	if !quiet {
		// git's buffered stdout lands after its stderr.
		fmt.Fprintf(r.g.err, "Switched to a new branch '%s'\n", name)
		fmt.Fprintf(r.g.out, "branch '%s' set up to track '%s'.\n", name, remote)
	}
	return nil
}

func (g *gitRun) checkout(args []string) error {
	args = splitFlags(args, "qf", "bB")
	var create, quiet, detach, force bool
	var newBranch string
	var rest, paths []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			paths = args[i+1:]
			i = len(args)
		case (a == "-b" || a == "-B") && i+1 < len(args):
			create = true
			i++
			newBranch = args[i]
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--detach":
			detach = true
		case a == "-f" || a == "--force":
			force = true
		case strings.HasPrefix(a, "-") && a != "-":
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	if len(paths) > 0 || (len(rest) > 1 && !create) {
		source := ""
		if len(paths) == 0 {
			source, paths = rest[0], rest[1:]
		} else if len(rest) == 1 {
			source = rest[0]
		}
		return r.checkoutPaths(source, paths)
	}
	if create {
		start := "HEAD"
		if len(rest) > 0 {
			start = rest[0]
		}
		return r.switchBranch(newBranch, true, start, false, quiet)
	}
	if len(rest) == 0 {
		if !detach {
			return nil
		}
		rest = []string{"HEAD"}
	}
	name := rest[0]
	if name == "-" {
		prev, ok := r.previousBranch(1)
		if !ok {
			return fatalf("invalid reference: @{-1}")
		}
		name = prev
	}
	if !detach && !r.isBranch(name) {
		if remote := r.dwimRemote(name); remote != "" {
			return r.checkoutTracking(name, remote, quiet)
		}
	}
	if _, err := r.Storer.Reference(plumbing.NewBranchReferenceName(name)); err != nil && !detach {
		if _, err := r.resolveRev(name); err != nil {
			return r.checkoutPaths("", rest) // not a revision: treat as paths
		}
	}
	if force {
		c, err := r.resolveCommit(name)
		if err != nil {
			return err
		}
		if err := r.resetHard(c); err != nil {
			return err
		}
	}
	return r.switchBranch(name, false, "", detach, quiet)
}

// checkoutPaths restores files from the index, or from source into both the
// index and the worktree.
func (r *repo) checkoutPaths(source string, paths []string) error {
	specs, err := r.pathspecs(paths)
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	from := r.indexSide(idx)
	if source != "" {
		c, err := r.resolveCommit(source)
		if err != nil {
			return err
		}
		if from, err = r.treeSide(c); err != nil {
			return err
		}
	}
	unmerged := unmergedPaths(idx)
	for i, s := range specs {
		found := false
		if source == "" {
			for p := range unmerged {
				if matchPath(s, p) {
					return failf(1, "error: path '%s' is unmerged\n", p)
				}
			}
		}
		for p, e := range from {
			if !matchPath(s, p) {
				continue
			}
			found = true
			info, err := r.writeFile(p, e)
			if err != nil {
				return err
			}
			if source != "" {
				setEntry(idx, p, e, info)
			}
		}
		if !found {
			return failf(1, "error: pathspec '%s' did not match any file(s) known to git\n", paths[i])
		}
	}
	return r.writeIndex(idx)
}

func (g *gitRun) switchBranch(args []string) error {
	args = splitFlags(args, "qdcC", "")
	var create, quiet, detach bool
	var rest []string
	for _, a := range args {
		switch a {
		case "-c", "-C", "--create":
			create = true
		case "-q", "--quiet":
			quiet = true
		case "-d", "--detach":
			detach = true
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return fatalf("missing branch or commit argument")
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	if create {
		start := "HEAD"
		if len(rest) > 1 {
			start = rest[1]
		}
		return r.switchBranch(rest[0], true, start, false, quiet)
	}
	if rest[0] == "-" {
		prev, ok := r.previousBranch(1)
		if !ok {
			return fatalf("invalid reference: @{-1}")
		}
		rest[0] = prev
	}
	if !detach && !r.isBranch(rest[0]) {
		if remote := r.dwimRemote(rest[0]); remote != "" {
			return r.checkoutTracking(rest[0], remote, quiet)
		}
		return fatalf("invalid reference: %s", rest[0])
	}
	return r.switchBranch(rest[0], false, "", detach, quiet)
}

func (g *gitRun) restore(args []string) error {
	var staged, worktree bool
	source := ""
	var paths []string
	for i, a := range args {
		switch {
		case a == "--":
			paths = append(paths, args[i+1:]...)
		case a == "-S" || a == "--staged":
			staged = true
		case a == "-W" || a == "--worktree":
			worktree = true
		case strings.HasPrefix(a, "--source="):
			source = strings.TrimPrefix(a, "--source=")
		case (a == "-s" || a == "--source") && i+1 < len(args):
			source = args[i+1]
			args[i+1] = "--source=" + source // consumed on the next iteration
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			paths = append(paths, a)
			continue
		}
		if a == "--" {
			break
		}
	}
	if len(paths) == 0 {
		return fatalf("you must specify path(s) to restore")
	}
	if !staged {
		worktree = true
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	specs, err := r.pathspecs(paths)
	if err != nil {
		return err
	}
	if staged {
		src := source
		if src == "" {
			src = "HEAD"
		}
		var c *object.Commit
		if head, _ := r.headCommit(); head != nil || src != "HEAD" {
			if c, err = r.resolveCommit(src); err != nil {
				return err
			}
		}
		if err := r.resetIndex(c, specs); err != nil {
			return err
		}
	}
	if worktree {
		if source != "" || staged {
			src := source
			if src == "" {
				src = "HEAD"
			}
			return r.restoreWorktree(src, specs, paths)
		}
		return r.checkoutPaths("", paths)
	}
	return nil
}

// restoreWorktree makes worktree files match source without touching the
// index; tracked files missing from source are deleted.
func (r *repo) restoreWorktree(source string, specs, args []string) error {
	c, err := r.resolveCommit(source)
	if err != nil {
		return err
	}
	from, err := r.treeSide(c)
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	tracked := r.indexSide(idx)
	for i, s := range specs {
		found := false
		for p, e := range from {
			if matchPath(s, p) {
				found = true
				if _, err := r.writeFile(p, e); err != nil {
					return err
				}
			}
		}
		for p := range tracked {
			if _, inSource := from[p]; !inSource && matchPath(s, p) {
				found = true
				if err := r.removeFile(p); err != nil {
					return err
				}
			}
		}
		if !found {
			return failf(1, "error: pathspec '%s' did not match any file(s) known to git\n", args[i])
		}
	}
	return nil
}

func (g *gitRun) reset(args []string) error {
	mode, quiet := "mixed", false
	var rest, paths []string
	for i, a := range args {
		if a == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		switch a {
		case "--soft", "--mixed", "--hard":
			mode = strings.TrimPrefix(a, "--")
		case "-q", "--quiet":
			quiet = true
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			rest = append(rest, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	rev := "HEAD"
	if len(rest) > 0 {
		if _, err := r.resolveRev(rest[0]); err == nil {
			rev, rest = rest[0], rest[1:]
		}
	}
	paths = append(rest, paths...)
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	var target *object.Commit
	if head != nil || rev != "HEAD" {
		if target, err = r.resolveCommit(rev); err != nil {
			return err
		}
	}
	if len(paths) > 0 {
		if mode != "mixed" {
			return fatalf("Cannot do %s reset with paths.", mode)
		}
		specs, err := r.pathspecs(paths)
		if err != nil {
			return err
		}
		if err := r.resetIndex(target, specs); err != nil {
			return err
		}
	} else {
		switch mode {
		case "hard":
			err = r.resetHard(target)
		case "mixed":
			err = r.resetIndex(target, nil)
		}
		if err != nil {
			return err
		}
		if target != nil {
			if err := r.setHead(target.Hash, "reset: moving to "+rev); err != nil {
				return err
			}
		}
	}
	if quiet {
		return nil
	}
	if mode == "hard" {
		fmt.Fprintf(g.out, "HEAD is now at %s %s\n", short(target.Hash), subject(target.Message))
		return nil
	}
	if mode == "mixed" {
		st, err := r.computeStatus(nil, true, false)
		if err != nil {
			return err
		}
		if st.hasUnstaged() {
			fmt.Fprintln(g.out, "Unstaged changes after reset:")
			for _, f := range st.tracked {
				if f.unstaged != ' ' {
					fmt.Fprintf(g.out, "%c\t%s\n", f.unstaged, f.path)
				}
			}
		}
	}
	return nil
}

func (g *gitRun) tag(args []string) error {
	var annotate, del, list bool
	var message string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-a" || a == "--annotate":
			annotate = true
		case a == "-m" && i+1 < len(args):
			i++
			message, annotate = args[i], true
		case a == "-d" || a == "--delete":
			del = true
		case a == "-l" || a == "--list":
			list = true
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	if del {
		for _, name := range rest {
			ref, err := r.Storer.Reference(plumbing.NewTagReferenceName(name))
			if err != nil {
				return failf(1, "error: tag '%s' not found.\n", name)
			}
			if err := r.Storer.RemoveReference(ref.Name()); err != nil {
				return err
			}
			fmt.Fprintf(g.out, "Deleted tag '%s' (was %s)\n", name, short(ref.Hash()))
		}
		return nil
	}
	if list || len(rest) == 0 {
		iter, err := r.Tags()
		if err != nil {
			return err
		}
		var names []string
		iter.ForEach(func(ref *plumbing.Reference) error {
			name := ref.Name().Short()
			if len(rest) == 0 || matchGlob(rest, name) {
				names = append(names, name)
			}
			return nil
		})
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintln(g.out, n)
		}
		return nil
	}
	if _, err := r.Storer.Reference(plumbing.NewTagReferenceName(rest[0])); err == nil {
		return fatalf("tag '%s' already exists", rest[0])
	}
	start := "HEAD"
	if len(rest) > 1 {
		start = rest[1]
	}
	c, err := r.resolveCommit(start)
	if err != nil {
		return err
	}
	target := c.Hash
	if annotate {
		if message == "" {
			return fatalf("no tag message given; use -m (no editor is available)")
		}
		tagger, err := r.signature("COMMITTER")
		if err != nil {
			return err
		}
		target, err = r.storeObject(&object.Tag{
			Name: rest[0], Tagger: *tagger, Message: cleanupMessage(message),
			TargetType: plumbing.CommitObject, Target: c.Hash,
		})
		if err != nil {
			return err
		}
	}
	ref := plumbing.NewTagReferenceName(rest[0])
	if err := ref.Validate(); err != nil {
		return fatalf("'%s' is not a valid tag name.", rest[0])
	}
	return r.Storer.SetReference(plumbing.NewHashReference(ref, target))
}

func matchGlob(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func (g *gitRun) config(args []string) error {
	global, get, unset, list := false, false, false, false
	file := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if (a == "-f" || a == "--file") && i+1 < len(args) {
			i++
			file = args[i]
			continue
		}
		if f, ok := strings.CutPrefix(a, "--file="); ok {
			file = f
			continue
		}
		switch a {
		case "--global":
			global = true
		case "--local":
		case "--get":
			get = true
		case "--unset":
			unset = true
		case "-l", "--list":
			list = true
		case "--system":
			return fatalf("--system is not available in this sandbox")
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			rest = append(rest, a)
		}
	}
	var files []string
	if file != "" {
		files = []string{fsName(g.abs(file))}
	} else if global {
		if files = []string{g.globalConfig()}; files[0] == "" {
			return fatalf("$HOME not set")
		}
	} else if r, err := g.openAny(); err == nil {
		files = []string{g.globalConfig(), r.localConfig()}
	} else if len(rest) > 1 || unset {
		return err
	} else {
		files = []string{g.globalConfig()}
	}
	if list {
		for _, file := range files {
			cfg, err := g.readConfig(file)
			if err != nil {
				return err
			}
			for _, s := range cfg.Sections {
				for _, o := range s.Options {
					fmt.Fprintf(g.out, "%s.%s=%s\n", strings.ToLower(s.Name), strings.ToLower(o.Key), o.Value)
				}
				for _, sub := range s.Subsections {
					for _, o := range sub.Options {
						fmt.Fprintf(g.out, "%s.%s.%s=%s\n", strings.ToLower(s.Name), sub.Name, strings.ToLower(o.Key), o.Value)
					}
				}
			}
		}
		if !global {
			for _, kv := range g.overrides {
				k, v, hasValue := strings.Cut(kv, "=")
				if sec, sub, name, ok := splitKey(k); ok {
					k = strings.ToLower(sec) + "." + strings.ToLower(name)
					if sub != "" {
						k = strings.ToLower(sec) + "." + sub + "." + strings.ToLower(name)
					}
				}
				if hasValue {
					fmt.Fprintf(g.out, "%s=%s\n", k, v)
				} else {
					fmt.Fprintln(g.out, k)
				}
			}
		}
		return nil
	}
	if len(rest) == 0 || len(rest) > 2 {
		return usagef("usage: git config [<options>] <name> [<value>]")
	}
	sec, sub, key, ok := splitKey(rest[0])
	if !ok {
		return failf(1, "error: key does not contain a section: %s\n", rest[0])
	}
	if len(rest) == 1 && !unset {
		value, found := "", false
		for _, file := range files {
			cfg, err := g.readConfig(file)
			if err != nil {
				return err
			}
			if v, ok := configGet(cfg, rest[0]); ok {
				value, found = v, true
			}
		}
		if v, ok := g.configOverride(rest[0]); ok && !global && file == "" {
			value, found = v, true
		}
		if !found {
			return failf(1, "")
		}
		fmt.Fprintln(g.out, value)
		return nil
	}
	_ = get
	target := files[len(files)-1]
	cfg, err := g.readConfig(target)
	if err != nil {
		return err
	}
	if unset {
		if _, ok := configGet(cfg, rest[0]); !ok {
			return failf(5, "")
		}
		if sub == "" {
			cfg.Section(sec).RemoveOption(key)
		} else {
			cfg.Section(sec).Subsection(sub).RemoveOption(key)
		}
	} else {
		cfg.SetOption(sec, sub, key, rest[1])
	}
	if err := g.writeConfig(target, cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fatalf("could not write config file %s", target)
		}
		return err
	}
	return nil
}
