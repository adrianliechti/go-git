package git

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// Linked worktrees, laid out like git's: <common>/worktrees/<name> holds
// HEAD, index, commondir, and gitdir; the worktree's .git file points back.

type worktreeInfo struct {
	path     string
	admin    string // "" for the main worktree
	head     plumbing.Hash
	branch   plumbing.ReferenceName
	bare     bool
	locked   bool
	prunable bool
}

func (w worktreeInfo) main() bool { return w.admin == "" }

// worktrees lists the main worktree first, then linked ones by name.
func (r *repo) worktrees() []worktreeInfo {
	g := r.g
	var out []worktreeInfo
	readHead := func(dir string) (plumbing.Hash, plumbing.ReferenceName) {
		data, err := fs.ReadFile(g.fsys, fsName(path.Join(dir, "HEAD")))
		if err != nil {
			return plumbing.ZeroHash, ""
		}
		s := strings.TrimSpace(string(data))
		if target, ok := strings.CutPrefix(s, "ref: "); ok {
			name := plumbing.ReferenceName(target)
			return r.refHash(name), name
		}
		return plumbing.NewHash(s), ""
	}
	mainPath := strings.TrimSuffix(r.commonDir, "/.git")
	h, b := readHead(r.commonDir)
	out = append(out, worktreeInfo{path: mainPath, head: h, branch: b, bare: mainPath == r.commonDir})
	entries, _ := fs.ReadDir(g.fsys, fsName(path.Join(r.commonDir, "worktrees")))
	for _, e := range entries {
		admin := path.Join(r.commonDir, "worktrees", e.Name())
		gitdir, err := fs.ReadFile(g.fsys, fsName(path.Join(admin, "gitdir")))
		if err != nil {
			continue
		}
		p := path.Dir(strings.TrimSpace(string(gitdir)))
		h, b := readHead(admin)
		w := worktreeInfo{path: p, admin: admin, head: h, branch: b}
		_, lockErr := fs.Stat(g.fsys, fsName(path.Join(admin, "locked")))
		w.locked = lockErr == nil
		if _, err := fs.Stat(g.fsys, fsName(path.Join(p, ".git"))); err != nil && !w.locked {
			w.prunable = true
		}
		out = append(out, w)
	}
	return out
}

// branchUsedElsewhere returns the path of another worktree that has branch
// checked out.
func (r *repo) branchUsedElsewhere(branch plumbing.ReferenceName) (string, bool) {
	for _, w := range r.worktrees() {
		if w.branch == branch && path.Clean(w.path) != path.Clean(r.top) && !w.bare {
			return w.path, true
		}
	}
	return "", false
}

func (r *repo) findWorktree(arg string) (worktreeInfo, bool) {
	p := r.g.abs(arg)
	for _, w := range r.worktrees() {
		if path.Clean(w.path) == p {
			return w, true
		}
	}
	return worktreeInfo{}, false
}

func (g *gitRun) worktree(args []string) error {
	if len(args) == 0 {
		return &exitError{code: 129, msg: commandUsage("worktree")}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "add":
		return r.worktreeAdd(args)
	case "list":
		return r.worktreeList(len(args) > 0 && args[0] == "--porcelain")
	case "remove":
		force := false
		var rest []string
		for _, a := range args {
			if a == "-f" || a == "--force" {
				force = true
			} else {
				rest = append(rest, a)
			}
		}
		if len(rest) != 1 {
			return &exitError{code: 129, msg: "usage: git worktree remove [<options>] <worktree>\n"}
		}
		w, ok := r.findWorktree(rest[0])
		if !ok {
			return fatalf("'%s' is not a working tree", rest[0])
		}
		if w.main() {
			return fatalf("'%s' is a main working tree", rest[0])
		}
		if w.locked && !force {
			return fatalf("cannot remove a locked working tree;\nuse 'remove -f -f' to override or unlock first")
		}
		if !force {
			wr, err := g.openAt2(w.path)
			if err == nil {
				st, err := wr.computeStatus(nil, false, true)
				if err == nil && (st.hasStaged() || st.hasUnstaged() || len(st.untracked) > 0) {
					return fatalf("'%s' contains modified or untracked files, use --force to delete it", rest[0])
				}
			}
		}
		if err := removeTree(g, w.path); err != nil {
			return err
		}
		return removeTree(g, w.admin)
	case "prune":
		for _, w := range r.worktrees() {
			if w.prunable {
				if err := removeTree(g, w.admin); err != nil {
					return err
				}
			}
		}
		return nil
	case "move":
		if len(args) != 2 {
			return &exitError{code: 129, msg: "usage: git worktree move <worktree> <new-path>\n"}
		}
		w, ok := r.findWorktree(args[0])
		if !ok {
			return fatalf("'%s' is not a working tree", args[0])
		}
		if w.main() {
			return fatalf("'%s' is a main working tree", args[0])
		}
		if w.locked {
			return fatalf("cannot move a locked working tree;\nuse 'move -f -f' to override or unlock first")
		}
		dst := g.abs(args[1])
		if _, err := fs.Stat(g.fsys, fsName(dst)); err == nil {
			return fatalf("'%s' already exists", args[1])
		}
		if err := g.fsys.Rename(fsName(w.path), fsName(dst)); err != nil {
			return err
		}
		return writeFSFile(g.fsys, fsName(path.Join(w.admin, "gitdir")), []byte(dst+"/.git\n"))
	case "lock", "unlock":
		reason := ""
		var rest []string
		for i := 0; i < len(args); i++ {
			if args[i] == "--reason" && i+1 < len(args) {
				i++
				reason = args[i]
			} else {
				rest = append(rest, args[i])
			}
		}
		if len(rest) != 1 {
			return &exitError{code: 129, msg: commandUsage("worktree")}
		}
		w, ok := r.findWorktree(rest[0])
		if !ok {
			return fatalf("'%s' is not a working tree", rest[0])
		}
		if w.main() {
			return fatalf("The main working tree cannot be locked or unlocked")
		}
		lock := fsName(path.Join(w.admin, "locked"))
		if sub == "unlock" {
			if !w.locked {
				return fatalf("'%s' is not locked", rest[0])
			}
			return g.fsys.Remove(lock)
		}
		if w.locked {
			return fatalf("'%s' is already locked", rest[0])
		}
		return writeFSFile(g.fsys, lock, []byte(reason))
	}
	return &exitError{code: 129, msg: "error: unknown subcommand: `" + sub + "'\n" + commandUsage("worktree")}
}

// openAt2 opens the worktree at top, whatever its .git is.
func (g *gitRun) openAt2(top string) (*repo, error) {
	info, err := fs.Stat(g.fsys, fsName(path.Join(top, ".git")))
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return g.openAt(top)
	}
	return g.openLinked(top)
}

func removeTree(g *gitRun, abs string) error {
	return newBillyFS(g.fsys, ".").removeTree(fsName(abs))
}

func (b *billyFS) removeTree(p string) error {
	info, err := b.Stat(p)
	if err != nil {
		return nil
	}
	if info.IsDir() {
		entries, err := b.ReadDir(p)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := b.removeTree(path.Join(p, e.Name())); err != nil {
				return err
			}
		}
	}
	return b.Remove(p)
}

func (r *repo) worktreeAdd(args []string) error {
	g := r.g
	var newBranch string
	var force, detach, lock bool
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case (a == "-b" || a == "-B") && i+1 < len(args):
			i++
			newBranch = args[i]
		case a == "-f" || a == "--force":
			force = true
		case a == "-d" || a == "--detach":
			detach = true
		case a == "--lock":
			lock = true
		case a == "--checkout" || a == "-q" || a == "--quiet":
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 || len(rest) > 2 {
		return &exitError{code: 129, msg: "usage: git worktree add [<options>] <path> [<commit-ish>]\n"}
	}
	dir := g.abs(rest[0])
	if entries, err := fs.ReadDir(g.fsys, fsName(dir)); err == nil && len(entries) > 0 {
		return fatalf("'%s' already exists", rest[0])
	}
	start := "HEAD"
	if len(rest) == 2 {
		start = rest[1]
	}
	var branch plumbing.ReferenceName
	var preparing string
	switch {
	case newBranch != "":
		branch = plumbing.NewBranchReferenceName(newBranch)
		preparing = fmt.Sprintf("Preparing worktree (new branch '%s')", newBranch)
	case detach:
		c, err := r.resolveCommit(start)
		if err != nil {
			return fatalf("invalid reference: %s", start)
		}
		preparing = fmt.Sprintf("Preparing worktree (detached HEAD %s)", short(c.Hash))
	case len(rest) == 2 && r.isBranch(start):
		branch = plumbing.NewBranchReferenceName(start)
		preparing = fmt.Sprintf("Preparing worktree (checking out '%s')", start)
	case len(rest) == 2:
		c, err := r.resolveCommit(start)
		if err != nil {
			return fatalf("invalid reference: %s", start)
		}
		detach = true
		preparing = fmt.Sprintf("Preparing worktree (detached HEAD %s)", short(c.Hash))
	default:
		// Without a commit-ish, use a branch named after the directory.
		name := path.Base(dir)
		branch = plumbing.NewBranchReferenceName(name)
		if r.isBranch(name) {
			preparing = fmt.Sprintf("Preparing worktree (checking out '%s')", name)
		} else {
			newBranch = name
			preparing = fmt.Sprintf("Preparing worktree (new branch '%s')", name)
		}
	}
	fmt.Fprintln(g.err, preparing)
	if branch != "" && newBranch == "" && !force {
		for _, w := range r.worktrees() {
			if w.branch == branch && !w.bare {
				return fatalf("'%s' is already used by worktree at '%s'", branch.Short(), w.path)
			}
		}
	}
	target, err := r.resolveCommit(start)
	if err != nil {
		return fatalf("invalid reference: %s", start)
	}
	if newBranch != "" {
		from := start
		if len(rest) < 2 {
			from = "HEAD"
		}
		if err := r.createBranch(newBranch, start, from); err != nil {
			return err
		}
	}
	// Pick a unique administrative name.
	name := path.Base(dir)
	admin := path.Join(r.commonDir, "worktrees", name)
	for i := 1; ; i++ {
		if _, err := fs.Stat(g.fsys, fsName(admin)); err != nil {
			break
		}
		admin = path.Join(r.commonDir, "worktrees", fmt.Sprintf("%s%d", name, i))
	}
	b := newBillyFS(g.fsys, ".")
	if err := b.MkdirAll(fsName(admin), 0777); err != nil {
		return err
	}
	if err := b.MkdirAll(fsName(dir), 0777); err != nil {
		return err
	}
	head := target.Hash.String() + "\n"
	if !detach {
		head = "ref: " + branch.String() + "\n"
	}
	files := map[string]string{
		path.Join(admin, "HEAD"):      head,
		path.Join(admin, "commondir"): "../..\n",
		path.Join(admin, "gitdir"):    dir + "/.git\n",
		path.Join(dir, ".git"):        "gitdir: " + admin + "\n",
	}
	if lock {
		files[path.Join(admin, "locked")] = ""
	}
	for p, content := range files {
		if err := writeFSFile(g.fsys, fsName(p), []byte(content)); err != nil {
			return err
		}
	}
	wr, err := g.openLinked(dir)
	if err != nil {
		return err
	}
	if err := wr.resetHard(target); err != nil {
		return err
	}
	// git creates HEAD (logged without a message), then resets to it.
	wr.appendReflog(plumbing.HEAD, plumbing.ZeroHash, target.Hash, "")
	wr.appendReflog(plumbing.HEAD, target.Hash, target.Hash, "reset: moving to HEAD")
	fmt.Fprintf(g.out, "HEAD is now at %s %s\n", short(target.Hash), subject(target.Message))
	return nil
}

func (r *repo) worktreeList(porcelain bool) error {
	g := r.g
	list := r.worktrees()
	sort.SliceStable(list[1:], func(i, j int) bool { return list[1+i].path < list[1+j].path })
	if porcelain {
		for _, w := range list {
			fmt.Fprintf(g.out, "worktree %s\n", w.path)
			switch {
			case w.bare:
				fmt.Fprintln(g.out, "bare")
			default:
				fmt.Fprintf(g.out, "HEAD %s\n", w.head)
				if w.branch != "" {
					fmt.Fprintf(g.out, "branch %s\n", w.branch)
				} else {
					fmt.Fprintln(g.out, "detached")
				}
			}
			if w.locked {
				fmt.Fprintln(g.out, "locked")
			}
			if w.prunable {
				fmt.Fprintf(g.out, "prunable gitdir file points to non-existent location\n")
			}
			fmt.Fprintln(g.out)
		}
		return nil
	}
	width := 0
	for _, w := range list {
		width = max(width, len(w.path))
	}
	for _, w := range list {
		line := fmt.Sprintf("%-*s ", width, w.path)
		switch {
		case w.bare:
			line += "(bare)"
		case w.branch != "":
			line += short(w.head) + " [" + w.branch.Short() + "]"
		default:
			line += short(w.head) + " (detached HEAD)"
		}
		if w.locked {
			line += " locked"
		}
		if w.prunable {
			line += " prunable"
		}
		fmt.Fprintln(g.out, line)
	}
	return nil
}
