package git

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// trackInfo compares a branch with its upstream.
type trackInfo struct {
	upstream      string // e.g. "origin/main"
	gone          bool
	ahead, behind int
}

// tracking returns branch's upstream status, or nil without an upstream.
func (r *repo) tracking(branch string) *trackInfo {
	remote, merge := r.upstream(branch)
	if remote == "" || merge == "" {
		return nil
	}
	ref := r.trackingRef(remote, merge)
	if ref == "" {
		return nil
	}
	t := &trackInfo{upstream: shortRef(ref)}
	up, err := r.Reference(ref, true)
	if err != nil {
		t.gone = true
		return t
	}
	local, err := r.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return t
	}
	a, b := r.ancestors(local.Hash()), r.ancestors(up.Hash())
	for h := range a {
		if !b[h] {
			t.ahead++
		}
	}
	for h := range b {
		if !a[h] {
			t.behind++
		}
	}
	return t
}

// ancestors returns h and every commit reachable from it.
func (r *repo) ancestors(h plumbing.Hash) map[plumbing.Hash]bool {
	seen := map[plumbing.Hash]bool{}
	stack := []plumbing.Hash{h}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[h] {
			continue
		}
		c, err := r.CommitObject(h)
		if err != nil {
			continue
		}
		seen[h] = true
		stack = append(stack, c.ParentHashes...)
	}
	return seen
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// counts is the "ahead 1, behind 2" or "gone" summary; "" when in sync.
func (t *trackInfo) counts() string {
	switch {
	case t.gone:
		return "gone"
	case t.ahead > 0 && t.behind > 0:
		return fmt.Sprintf("ahead %d, behind %d", t.ahead, t.behind)
	case t.ahead > 0:
		return fmt.Sprintf("ahead %d", t.ahead)
	case t.behind > 0:
		return fmt.Sprintf("behind %d", t.behind)
	}
	return ""
}

// longStatus is the tracking paragraph of git status, with its blank line.
func (t *trackInfo) longStatus() string {
	switch {
	case t.gone:
		return fmt.Sprintf("Your branch is based on '%s', but the upstream is gone.\n"+
			"  (use \"git branch --unset-upstream\" to fixup)\n", t.upstream)
	case t.ahead > 0 && t.behind > 0:
		return fmt.Sprintf("Your branch and '%s' have diverged,\nand have %d and %d different commits each, respectively.\n"+
			"  (use \"git pull\" if you want to integrate the remote branch with yours)\n", t.upstream, t.ahead, t.behind)
	case t.ahead > 0:
		return fmt.Sprintf("Your branch is ahead of '%s' by %s.\n  (use \"git push\" to publish your local commits)\n",
			t.upstream, plural(t.ahead, "commit"))
	case t.behind > 0:
		return fmt.Sprintf("Your branch is behind '%s' by %s, and can be fast-forwarded.\n  (use \"git pull\" to update your local branch)\n",
			t.upstream, plural(t.behind, "commit"))
	}
	return fmt.Sprintf("Your branch is up to date with '%s'.\n", t.upstream)
}

func (g *gitRun) branch(args []string) error {
	var del, forceDel, showCurrent, all, remotes, move, unset bool
	verbose := 0
	upstream := ""
	var names []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-d" || a == "--delete":
			del = true
		case a == "-D":
			del, forceDel = true, true
		case a == "--show-current":
			showCurrent = true
		case a == "-a" || a == "--all":
			all = true
		case a == "-r" || a == "--remotes":
			remotes = true
		case a == "-v" || a == "--verbose":
			verbose++
		case a == "-vv":
			verbose += 2
		case a == "-m" || a == "-M" || a == "--move":
			move = true
		case a == "-u" && i+1 < len(args):
			i++
			upstream = args[i]
		case strings.HasPrefix(a, "--set-upstream-to="):
			upstream = strings.TrimPrefix(a, "--set-upstream-to=")
		case a == "--unset-upstream":
			unset = true
		case a == "-l" || a == "--list":
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			names = append(names, a)
		}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	current, err := r.branchName()
	if err != nil {
		return err
	}
	switch {
	case showCurrent:
		if current != "" {
			fmt.Fprintln(g.out, current)
		}
		return nil
	case upstream != "" || unset:
		name := current
		if len(names) > 0 {
			name = names[0]
		}
		if !r.isBranch(name) {
			return fatalf("branch '%s' does not exist", name)
		}
		if unset {
			cfg, err := r.readLocalConfig()
			if err != nil {
				return err
			}
			if !cfg.HasSection("branch") || !cfg.Section("branch").HasSubsection(name) {
				return fatalf("branch '%s' has no upstream information", name)
			}
			cfg.Section("branch").Subsection(name).RemoveOption("remote").RemoveOption("merge")
			return g.writeConfig(r.localConfig(), cfg)
		}
		return r.trackUpstream(name, upstream)
	case move:
		if len(names) == 0 || len(names) > 2 {
			return fatalf("branch name required")
		}
		old, name := current, names[0]
		if len(names) == 2 {
			old, name = names[0], names[1]
		}
		return r.renameBranch(old, name)
	case del:
		if len(names) == 0 {
			return fatalf("branch name required")
		}
		head, err := r.headCommit()
		if err != nil {
			return err
		}
		for _, name := range names {
			ref, err := r.Storer.Reference(plumbing.NewBranchReferenceName(name))
			if err != nil {
				return failf(1, "error: branch '%s' not found\n", name)
			}
			if name == current {
				return failf(1, "error: cannot delete branch '%s' used by worktree at '%s'\n", name, r.top)
			}
			if !forceDel {
				// Like git, a branch merged into its upstream counts as merged.
				base := plumbing.ZeroHash
				if head != nil {
					base = head.Hash
				}
				if up := r.upstreamRef(name); up != "" {
					if u, err := r.Reference(up, true); err == nil {
						base = u.Hash()
					}
				}
				if base.IsZero() || !r.isAncestor(ref.Hash(), base) {
					return failf(1, "error: the branch '%s' is not fully merged\n"+
						"hint: If you are sure you want to delete it, run 'git branch -D %s'\n"+
						"hint: Disable this message with \"git config set advice.forceDeleteBranch false\"\n", name, name)
				}
			}
			if err := r.deleteRef(ref.Name()); err != nil {
				return err
			}
			cfg, err := r.readLocalConfig()
			if err == nil && cfg.HasSection("branch") && cfg.Section("branch").HasSubsection(name) {
				cfg.RemoveSubsection("branch", name)
				g.writeConfig(r.localConfig(), cfg)
			}
			fmt.Fprintf(g.out, "Deleted branch %s (was %s).\n", name, short(ref.Hash()))
		}
		return nil
	case len(names) > 0 && !all && !remotes:
		if len(names) > 2 {
			return usagef("usage: git branch <name> [<start-point>]")
		}
		start, from := "HEAD", current
		if len(names) == 2 {
			start, from = names[1], names[1]
		}
		if from == "" {
			from = "HEAD"
		}
		if err := r.createBranch(names[0], start, from); err != nil {
			return err
		}
		// Branching from a remote-tracking branch sets it as upstream.
		if ref, err := r.Storer.Reference(expandRef(start, "refs/remotes/")); err == nil && ref.Name().IsRemote() {
			return r.trackUpstream(names[0], start)
		}
		return nil
	}
	return r.listBranches(current, all || !remotes, all || remotes, verbose)
}

// trackUpstream sets branch's upstream to a remote-tracking or local branch.
func (r *repo) trackUpstream(branch, upstream string) error {
	ref, err := r.Storer.Reference(plumbing.ReferenceName("refs/remotes/" + upstream))
	if err == nil {
		remote, rest, _ := strings.Cut(upstream, "/")
		if _, ok := r.remote(remote); ok {
			if err := r.setUpstream(branch, remote, plumbing.NewBranchReferenceName(rest)); err != nil {
				return err
			}
			fmt.Fprintf(r.g.out, "branch '%s' set up to track '%s'.\n", branch, upstream)
			return nil
		}
		_ = ref
	}
	if r.isBranch(upstream) {
		if err := r.setUpstream(branch, ".", plumbing.NewBranchReferenceName(upstream)); err != nil {
			return err
		}
		fmt.Fprintf(r.g.out, "branch '%s' set up to track '%s'.\n", branch, upstream)
		return nil
	}
	return fatalf("the requested upstream branch '%s' does not exist", upstream)
}

func (r *repo) renameBranch(old, name string) error {
	from := plumbing.NewBranchReferenceName(old)
	ref, err := r.Storer.Reference(from)
	if err != nil {
		return fatalf("no branch named '%s'", old)
	}
	to := plumbing.NewBranchReferenceName(name)
	if err := to.Validate(); err != nil {
		return fatalf("'%s' is not a valid branch name", name)
	}
	if _, err := r.Storer.Reference(to); err == nil && old != name {
		return fatalf("a branch named '%s' already exists", name)
	}
	if old != name {
		// The reflog moves with the branch, plus a rename entry.
		entries := r.readReflog(from)
		if err := r.Storer.SetReference(plumbing.NewHashReference(to, ref.Hash())); err != nil {
			return err
		}
		r.Storer.RemoveReference(from)
		r.deleteReflog(from)
		if len(entries) > 0 {
			r.writeReflog(to, entries)
		}
		r.appendReflog(to, ref.Hash(), ref.Hash(), "Branch: renamed "+from.String()+" to "+to.String())
	}
	if current, _ := r.branchName(); current == old {
		if err := r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, to)); err != nil {
			return err
		}
	}
	cfg, err := r.readLocalConfig()
	if err == nil && cfg.HasSection("branch") && cfg.Section("branch").HasSubsection(old) && old != name {
		sub := cfg.Section("branch").Subsection(old)
		for _, o := range sub.Options {
			cfg.AddOption("branch", name, o.Key, o.Value)
		}
		cfg.RemoveSubsection("branch", old)
		return r.g.writeConfig(r.localConfig(), cfg)
	}
	return nil
}

func (r *repo) listBranches(current string, local, remote bool, verbose int) error {
	g := r.g
	type item struct {
		name, label, target string
		hash                plumbing.Hash
		current             bool
		branch              string // local branch name, for -vv
	}
	var items []item
	if local && current == "" {
		if head, err := r.headCommit(); err == nil && head != nil {
			items = append(items, item{label: "(HEAD detached at " + short(head.Hash) + ")", hash: head.Hash, current: true})
		}
	}
	iter, err := r.Storer.IterReferences()
	if err != nil {
		return err
	}
	var refs []*plumbing.Reference
	iter.ForEach(func(ref *plumbing.Reference) error {
		refs = append(refs, ref)
		return nil
	})
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name() < refs[j].Name() })
	for _, ref := range refs {
		name := ref.Name()
		switch {
		case local && name.IsBranch():
			items = append(items, item{label: name.Short(), hash: ref.Hash(), current: name.Short() == current, branch: name.Short()})
		case remote && name.IsRemote():
			label := strings.TrimPrefix(name.String(), "refs/remotes/")
			if local {
				label = "remotes/" + label
			}
			it := item{label: label}
			if ref.Type() == plumbing.SymbolicReference {
				it.target = shortRef(ref.Target())
			} else {
				it.hash = ref.Hash()
			}
			items = append(items, it)
		}
	}
	width := 0
	for _, it := range items {
		if it.target == "" {
			width = max(width, len(it.label))
		}
	}
	for _, it := range items {
		mark := "  "
		if it.current {
			mark = "* "
		}
		switch {
		case it.target != "":
			fmt.Fprintf(g.out, "%s%s -> %s\n", mark, it.label, it.target)
		case verbose == 0:
			fmt.Fprintf(g.out, "%s%s\n", mark, it.label)
		default:
			c, err := r.CommitObject(it.hash)
			if err != nil {
				return err
			}
			track := ""
			if verbose > 1 && it.branch != "" {
				if t := r.tracking(it.branch); t != nil {
					track = "[" + t.upstream
					if s := t.counts(); s != "" {
						track += ": " + s
					}
					track += "] "
				}
			} else if it.branch != "" {
				if t := r.tracking(it.branch); t != nil && t.counts() != "" {
					track = "[" + t.counts() + "] "
				}
			}
			fmt.Fprintf(g.out, "%s%-*s %s %s%s\n", mark, width, it.label, short(it.hash), track, subject(c.Message))
		}
	}
	return nil
}
