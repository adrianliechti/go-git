package git

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-billy/v5"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	format "github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// Submodules, laid out like git's: the superproject's index holds a gitlink
// (mode 160000), .gitmodules maps names to paths and URLs, the submodule's
// git directory lives in <common>/modules/<name>, and its worktree's .git
// file points there.

type submoduleInfo struct {
	name, path, url, branch string
}

func (r *repo) readGitmodules() (*format.Config, error) {
	data, err := fs.ReadFile(r.g.fsys, fsName(path.Join(r.top, ".gitmodules")))
	cfg := format.New()
	if err != nil {
		return cfg, nil
	}
	if err := format.NewDecoder(bytes.NewReader(data)).Decode(cfg); err != nil {
		return nil, fatalf("bad config file .gitmodules: %v", err)
	}
	return cfg, nil
}

func (r *repo) submodules() ([]submoduleInfo, error) {
	cfg, err := r.readGitmodules()
	if err != nil {
		return nil, err
	}
	var out []submoduleInfo
	if cfg.HasSection("submodule") {
		for _, s := range cfg.Section("submodule").Subsections {
			out = append(out, submoduleInfo{name: s.Name, path: s.Option("path"), url: s.Option("url"), branch: s.Option("branch")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

func (r *repo) submoduleByPath(p string) (submoduleInfo, bool) {
	subs, _ := r.submodules()
	for _, s := range subs {
		if s.path == p {
			return s, true
		}
	}
	return submoduleInfo{}, false
}

// resolveSubmoduleURL resolves ./ and ../ URLs against the superproject's
// default remote, or its directory when it has none.
func (r *repo) resolveSubmoduleURL(url string) string {
	if !strings.HasPrefix(url, "./") && !strings.HasPrefix(url, "../") {
		return url
	}
	base := r.top
	branch, _ := r.branchName()
	remote, _ := r.upstream(branch)
	if remote == "" || remote == "." {
		remote = "origin"
	}
	if info, ok := r.remote(remote); ok {
		base = info.url
		if !strings.Contains(base, "://") && !path.IsAbs(base) {
			base = path.Join(r.top, base)
		}
	}
	base = strings.TrimSuffix(base, "/")
	for {
		switch {
		case strings.HasPrefix(url, "./"):
			url = url[2:]
		case strings.HasPrefix(url, "../"):
			url = url[3:]
			if i := strings.LastIndex(base, "/"); i >= 0 {
				base = base[:i]
			}
		default:
			if url == "" {
				return base
			}
			return base + "/" + url
		}
	}
}

// relAbs returns the relative path from directory from to to.
func relAbs(from, to string) string {
	a := strings.Split(strings.Trim(from, "/"), "/")
	b := strings.Split(strings.Trim(to, "/"), "/")
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	parts := []string{}
	for range a[i:] {
		parts = append(parts, "..")
	}
	parts = append(parts, b[i:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

func (g *gitRun) submodule(args []string) error {
	sub := "status"
	quiet := false
	for len(args) > 0 && (args[0] == "-q" || args[0] == "--quiet") {
		quiet, args = true, args[1:]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	switch sub {
	case "add":
		return r.submoduleAdd(args, quiet)
	case "status":
		return r.submoduleStatus(args)
	case "init":
		return r.submoduleInit(args, quiet)
	case "update":
		return r.submoduleUpdate(args, quiet)
	case "deinit":
		return r.submoduleDeinit(args, quiet)
	case "sync":
		return r.submoduleSync(args, quiet)
	case "set-url":
		if len(args) != 2 {
			return &exitError{code: 129, msg: "usage: git submodule set-url [--quiet] <path> <newurl>\n"}
		}
		info, ok := r.submoduleByPath(strings.TrimSuffix(args[0], "/"))
		if !ok {
			return fatalf("no submodule mapping found in .gitmodules for path '%s'", args[0])
		}
		cfg, err := r.readGitmodules()
		if err != nil {
			return err
		}
		cfg.SetOption("submodule", info.name, "url", args[1])
		if err := g.writeConfig(fsName(path.Join(r.top, ".gitmodules")), cfg); err != nil {
			return err
		}
		return r.submoduleSync([]string{info.path}, true)
	case "foreach":
		return fatalf("git submodule foreach runs shell commands, which is not possible in this sandbox")
	}
	return &exitError{code: 1, msg: "usage: git submodule [--quiet] [--cached]\n   or: git submodule [--quiet] add [-b <branch>] [--name <name>] [--] <repository> [<path>]\n"}
}

func (r *repo) submoduleAdd(args []string, quiet bool) error {
	g := r.g
	branch, name := "", ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-b" && i+1 < len(args):
			i++
			branch = args[i]
		case a == "--name" && i+1 < len(args):
			i++
			name = args[i]
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "-f" || a == "--force" || a == "--":
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 || len(rest) > 2 {
		return &exitError{code: 129, msg: "usage: git submodule add [<options>] [--] <repository> [<path>]\n"}
	}
	url := rest[0]
	p := strings.TrimSuffix(path.Base(strings.TrimSuffix(url, "/")), ".git")
	if len(rest) == 2 {
		rp, err := r.repoPath(rest[1])
		if err != nil {
			return err
		}
		p = strings.TrimSuffix(rp, "/")
	} else if r.prefix != "" {
		p = strings.TrimSuffix(r.prefix, "/") + "/" + p
	}
	if name == "" {
		name = p
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	if _, err := idx.Entry(p); err == nil {
		return fatalf("'%s' already exists in the index", p)
	}
	resolved := r.resolveSubmoduleURL(url)
	if r.isNestedRepo(p) {
		fmt.Fprintf(g.err, "Adding existing repo at '%s' to the index\n", p)
	} else {
		if entries, err := fs.ReadDir(g.fsys, fsName(path.Join(r.top, p))); err == nil && len(entries) > 0 {
			return fatalf("'%s' already exists and is not a valid git repo", p)
		}
		if _, err := r.cloneSubmodule(name, p, resolved, branch, quiet, false); err != nil {
			return err
		}
	}
	cfg, err := r.readGitmodules()
	if err != nil {
		return err
	}
	cfg.SetOption("submodule", name, "path", p)
	cfg.SetOption("submodule", name, "url", url)
	if branch != "" {
		cfg.SetOption("submodule", name, "branch", branch)
	}
	if err := g.writeConfig(fsName(path.Join(r.top, ".gitmodules")), cfg); err != nil {
		return err
	}
	local, err := r.readLocalConfig()
	if err != nil {
		return err
	}
	local.SetOption("submodule", name, "url", resolved)
	local.SetOption("submodule", name, "active", "true")
	if err := g.writeConfig(r.localConfig(), local); err != nil {
		return err
	}
	if idx, err = r.readIndex(); err != nil {
		return err
	}
	for _, f := range []string{".gitmodules", p} {
		if err := r.stageFile(idx, f); err != nil {
			return err
		}
	}
	return r.writeIndex(idx)
}

// cloneSubmodule clones url into path p with its git directory in
// <common>/modules/<name>. With detach, HEAD is left detached at the
// remote's HEAD commit, as submodule update does before checking out.
func (r *repo) cloneSubmodule(name, p, url, branch string, quiet, detach bool) (*repo, error) {
	g := r.g
	top := path.Join(r.top, p)
	gitDir := path.Join(r.commonDir, "modules", name)
	ep, err := g.resolveURL(url, r.top)
	if err != nil {
		return nil, err
	}
	conn, err := g.connect(ep)
	if err != nil {
		if ep.local != "" {
			return nil, fatalf("repository '%s' does not exist\nfatal: clone of '%s' into submodule path '%s' failed", url, url, top)
		}
		return nil, err
	}
	adv, err := conn.advertised()
	if err != nil {
		return nil, err
	}
	if !quiet {
		fmt.Fprintf(g.err, "Cloning into '%s'...\n", top)
	}
	head := adv.head
	if branch != "" {
		head = plumbing.NewBranchReferenceName(branch)
	}
	initial := head.Short()
	if head == "" {
		initial = "master"
	}
	all := newBillyFS(g.fsys, ".")
	for _, d := range []string{gitDir, top} {
		if err := all.MkdirAll(fsName(d), 0777); err != nil {
			return nil, err
		}
	}
	st := filesystem.NewStorage(newBillyFS(g.fsys, fsName(gitDir)), cache.NewObjectLRUDefault())
	if _, err := gogit.InitWithOptions(st, (billy.Filesystem)(nil), gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(initial)}); err != nil {
		return nil, fatalf("%v", err)
	}
	cfg := format.New()
	cfg.SetOption("core", "", "repositoryformatversion", "0")
	cfg.SetOption("core", "", "filemode", "true")
	cfg.SetOption("core", "", "bare", "false")
	cfg.SetOption("core", "", "logallrefupdates", "true")
	cfg.SetOption("core", "", "worktree", relAbs(gitDir, top))
	if err := g.writeConfig(fsName(path.Join(gitDir, "config")), cfg); err != nil {
		return nil, err
	}
	if err := writeFSFile(g.fsys, fsName(path.Join(top, ".git")), []byte("gitdir: "+relAbs(top, gitDir)+"\n")); err != nil {
		return nil, err
	}
	sr, err := g.openLinked(top)
	if err != nil {
		return nil, err
	}
	action := g.reflogAction
	g.reflogAction = ""
	err = sr.populateClone(ep, adv, url, "origin", head, false, detach)
	g.reflogAction = action
	if err != nil {
		return nil, err
	}
	if !quiet && ep.local != "" {
		fmt.Fprintln(g.err, "done.")
	}
	return sr, nil
}

// gitlinks lists the submodule paths recorded in the index.
func (r *repo) gitlinks(args []string) ([]string, map[string]plumbing.Hash, error) {
	specs, err := r.pathspecs(args)
	if err != nil {
		return nil, nil, err
	}
	idx, err := r.readIndex()
	if err != nil {
		return nil, nil, err
	}
	hashes := map[string]plumbing.Hash{}
	var paths []string
	for _, e := range idx.Entries {
		if e.Mode == filemode.Submodule && matchAny(specs, e.Name) {
			if _, seen := hashes[e.Name]; !seen {
				paths = append(paths, e.Name)
			}
			hashes[e.Name] = e.Hash
		}
	}
	sort.Strings(paths)
	return paths, hashes, nil
}

func (r *repo) submoduleStatus(args []string) error {
	recursive := false
	var rest []string
	for _, a := range args {
		switch a {
		case "--recursive":
			recursive = true
		case "--cached", "--":
		default:
			rest = append(rest, a)
		}
	}
	return r.printSubmoduleStatus(rest, "", recursive)
}

// printSubmoduleStatus lists submodules; prefix is the path of this
// repository inside the top-level superproject for --recursive.
func (r *repo) printSubmoduleStatus(args []string, prefix string, recursive bool) error {
	g := r.g
	paths, hashes, err := r.gitlinks(args)
	if err != nil {
		return err
	}
	for _, p := range paths {
		display := prefix + p
		if prefix == "" {
			display = r.display(p)
		}
		if !r.isNestedRepo(p) {
			fmt.Fprintf(g.out, "-%s %s\n", hashes[p], display)
			continue
		}
		sr, err := g.openAt2(path.Join(r.top, p))
		if err != nil {
			fmt.Fprintf(g.out, "-%s %s\n", hashes[p], display)
			continue
		}
		head := sr.refHash(plumbing.HEAD)
		mark := " "
		if head != hashes[p] {
			mark = "+"
		}
		desc := ""
		if c, err := sr.CommitObject(head); err == nil {
			// Like describe --all --always.
			d, err := sr.describeCommit(c, false, true, false, false, 7, "")
			if err != nil {
				d = short(c.Hash)
			}
			desc = " (" + d + ")"
		}
		fmt.Fprintf(g.out, "%s%s %s%s\n", mark, head, display, desc)
		if recursive {
			if err := sr.printSubmoduleStatus(nil, display+"/", true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *repo) submoduleInit(args []string, quiet bool) error {
	g := r.g
	specs, err := r.pathspecs(args)
	if err != nil {
		return err
	}
	subs, err := r.submodules()
	if err != nil {
		return err
	}
	local, err := r.readLocalConfig()
	if err != nil {
		return err
	}
	changed := false
	for _, s := range subs {
		if !matchAny(specs, s.path) {
			continue
		}
		if _, ok := configGet(local, "submodule."+s.name+".url"); ok {
			continue
		}
		resolved := r.resolveSubmoduleURL(s.url)
		local.SetOption("submodule", s.name, "active", "true")
		local.SetOption("submodule", s.name, "url", resolved)
		changed = true
		if !quiet {
			fmt.Fprintf(g.out, "Submodule '%s' (%s) registered for path '%s'\n", s.name, resolved, r.display(s.path))
		}
	}
	if !changed {
		return nil
	}
	return g.writeConfig(r.localConfig(), local)
}

func (r *repo) submoduleUpdate(args []string, quiet bool) error {
	g := r.g
	var initFirst, recursive bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--init":
			initFirst = true
		case "--recursive":
			recursive = true
		case "-q", "--quiet":
			quiet = true
		case "--checkout", "--":
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			rest = append(rest, a)
		}
	}
	if initFirst {
		if err := r.submoduleInit(rest, quiet); err != nil {
			return err
		}
	}
	paths, hashes, err := r.gitlinks(rest)
	if err != nil {
		return err
	}
	local, err := r.readLocalConfig()
	if err != nil {
		return err
	}
	for _, p := range paths {
		info, ok := r.submoduleByPath(p)
		if !ok {
			return fatalf("No url found for submodule path '%s' in .gitmodules", p)
		}
		url, registered := configGet(local, "submodule."+info.name+".url")
		if !registered {
			continue
		}
		var sr *repo
		fresh := false
		switch {
		case r.isNestedRepo(p):
			if sr, err = g.openAt2(path.Join(r.top, p)); err != nil {
				return err
			}
		default:
			gitDir := path.Join(r.commonDir, "modules", info.name)
			if _, err := fs.Stat(g.fsys, fsName(gitDir)); err == nil {
				// The git directory survived deinit; reconnect the worktree.
				top := path.Join(r.top, p)
				if err := newBillyFS(g.fsys, ".").MkdirAll(fsName(top), 0777); err != nil {
					return err
				}
				if err := writeFSFile(g.fsys, fsName(path.Join(top, ".git")), []byte("gitdir: "+relAbs(top, gitDir)+"\n")); err != nil {
					return err
				}
				if sr, err = g.openLinked(top); err != nil {
					return err
				}
			} else {
				if sr, err = r.cloneSubmodule(info.name, p, url, info.branch, quiet, true); err != nil {
					return err
				}
			}
			fresh = true
		}
		target := hashes[p]
		if fresh || sr.refHash(plumbing.HEAD) != target {
			if _, err := sr.CommitObject(target); err != nil {
				// Fetch in case the recorded commit is newer than the clone.
				if ep, err := g.resolveURL(url, r.top); err == nil {
					sr.fetch(ep, []config.RefSpec{config.RefSpec(defaultFetchSpec("origin"))}, fetchOptions{}, "")
				}
			}
			c, err := sr.CommitObject(target)
			if err != nil {
				return fatalf("Unable to find current revision in submodule path '%s'", p)
			}
			if fresh {
				// A fresh clone has an empty worktree to fill.
				err = sr.resetHard(c)
			} else {
				err = sr.switchTo(c, "checkout", false)
			}
			if err != nil {
				return err
			}
			if err := sr.pointHead("", c.Hash, "checkout: moving from "+sr.headName()+" to "+c.Hash.String()); err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(g.out, "Submodule path '%s': checked out '%s'\n", r.display(p), target)
			}
		}
		if recursive {
			cwd := g.cwd
			g.cwd = sr.top
			err := sr.submoduleUpdate([]string{"--init", "--recursive"}, quiet)
			g.cwd = cwd
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *repo) submoduleDeinit(args []string, quiet bool) error {
	g := r.g
	var force, all bool
	var rest []string
	for _, a := range args {
		switch a {
		case "-f", "--force":
			force = true
		case "--all":
			all = true
		case "--":
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 && !all {
		return fatalf("Use '--all' if you really want to deinitialize all submodules")
	}
	paths, _, err := r.gitlinks(rest)
	if err != nil {
		return err
	}
	local, err := r.readLocalConfig()
	if err != nil {
		return err
	}
	for _, p := range paths {
		info, _ := r.submoduleByPath(p)
		if info.name == "" {
			info.name = p
		}
		top := path.Join(r.top, p)
		if r.isNestedRepo(p) {
			if e, _, _ := r.submoduleEntry(p); (e.dirtyModified || e.dirtyUntracked) && !force {
				return failf(128, "error: the following file has local modifications:\n    %s\n"+
					"(use --cached to keep the file, or -f to force removal)\n"+
					"fatal: Submodule work tree '%s' contains local modifications; use '-f' to discard them\n", p, p)
			}
			d := newBillyFS(g.fsys, fsName(top))
			entries, _ := d.ReadDir(".")
			for _, e := range entries {
				d.removeTree(e.Name())
			}
			if !quiet {
				fmt.Fprintf(g.out, "Cleared directory '%s'\n", r.display(p))
			}
		}
		if local.HasSection("submodule") && local.Section("submodule").HasSubsection(info.name) {
			local.RemoveSubsection("submodule", info.name)
			if !quiet {
				fmt.Fprintf(g.out, "Submodule '%s' (%s) unregistered for path '%s'\n", info.name, info.url, r.display(p))
			}
		}
	}
	return g.writeConfig(r.localConfig(), local)
}

func (r *repo) submoduleSync(args []string, quiet bool) error {
	g := r.g
	var rest []string
	for _, a := range args {
		if a != "--recursive" {
			rest = append(rest, a)
		}
	}
	paths, _, err := r.gitlinks(rest)
	if err != nil {
		return err
	}
	local, err := r.readLocalConfig()
	if err != nil {
		return err
	}
	for _, p := range paths {
		info, ok := r.submoduleByPath(p)
		if !ok {
			continue
		}
		if _, registered := configGet(local, "submodule."+info.name+".url"); !registered {
			continue
		}
		resolved := r.resolveSubmoduleURL(info.url)
		local.SetOption("submodule", info.name, "url", resolved)
		if !quiet {
			fmt.Fprintf(g.out, "Synchronizing submodule url for '%s'\n", r.display(p))
		}
		if r.isNestedRepo(p) {
			if sr, err := g.openAt2(path.Join(r.top, p)); err == nil {
				if cfg, err := sr.readLocalConfig(); err == nil {
					cfg.SetOption("remote", "origin", "url", resolved)
					g.writeConfig(sr.localConfig(), cfg)
				}
			}
		}
	}
	return g.writeConfig(r.localConfig(), local)
}
