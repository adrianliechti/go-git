package git

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/index"
)

func (g *gitRun) mv(args []string) error {
	var force, skip, dryRun, verbose bool
	var paths []string
	for i, a := range args {
		if a == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		switch a {
		case "-f", "--force":
			force = true
		case "-k":
			skip = true
		case "-n", "--dry-run":
			dryRun = true
		case "-v", "--verbose":
			verbose = true
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			paths = append(paths, a)
		}
	}
	if len(paths) < 2 {
		return &exitError{code: 129, msg: commandUsage("mv")}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	dst, err := r.repoPath(paths[len(paths)-1])
	if err != nil {
		return err
	}
	srcs := paths[:len(paths)-1]
	info, err := r.wt.Stat(dirOrDot(dst))
	dstIsDir := err == nil && info.IsDir()
	if len(srcs) > 1 && !dstIsDir {
		return fatalf("destination '%s' is not a directory", dst)
	}
	type move struct{ from, to string }
	var moves []move
	for _, arg := range srcs {
		src, err := r.repoPath(arg)
		if err != nil {
			return err
		}
		to := dst
		if dstIsDir {
			to = path.Join(dst, path.Base(src))
		}
		if reason := r.checkMove(idx, src, to, force); reason != "" {
			if skip {
				continue
			}
			if strings.HasPrefix(reason, "renaming ") {
				return fatalf("%s", reason)
			}
			return fatalf("%s, source=%s, destination=%s", reason, src, to)
		}
		moves = append(moves, move{src, to})
	}
	for _, m := range moves {
		if dryRun {
			fmt.Fprintf(g.out, "Checking rename of '%s' to '%s'\n", m.from, m.to)
		}
		if dryRun || verbose {
			fmt.Fprintf(g.out, "Renaming %s to %s\n", m.from, m.to)
		}
		if dryRun {
			continue
		}
		if force {
			if info, err := r.wt.Stat(m.to); err == nil && !info.IsDir() {
				r.wt.Remove(m.to)
				removeEntry(idx, m.to)
			}
		}
		if err := r.wt.Rename(m.from, m.to); err != nil {
			return fatalf("renaming '%s' failed: %v", m.from, err)
		}
		for _, e := range idx.Entries {
			if e.Name == m.from || strings.HasPrefix(e.Name, m.from+"/") {
				e.Name = m.to + strings.TrimPrefix(e.Name, m.from)
			}
		}
	}
	if dryRun {
		return nil
	}
	return r.writeIndex(idx)
}

// checkMove returns git's reason for refusing a move, or "".
func (r *repo) checkMove(idx *index.Index, src, to string, force bool) string {
	info, err := r.wt.Stat(dirOrDot(src))
	if err != nil {
		return "bad source"
	}
	if src == "" || src == to || strings.HasPrefix(to, src+"/") {
		return "can not move directory into itself"
	}
	tracked := false
	for _, e := range idx.Entries {
		if e.Name == src || (info.IsDir() && strings.HasPrefix(e.Name, src+"/")) {
			tracked = true
			break
		}
	}
	if !tracked {
		if info.IsDir() {
			return "source directory is empty"
		}
		return "not under version control"
	}
	if parent := path.Dir(to); parent != "." {
		if pinfo, err := r.wt.Stat(parent); err != nil || !pinfo.IsDir() {
			return "renaming '" + src + "' failed: No such file or directory"
		}
	}
	if dinfo, err := r.wt.Stat(to); err == nil {
		if info.IsDir() || dinfo.IsDir() || !force {
			return "destination exists"
		}
	}
	return ""
}

func (g *gitRun) clean(args []string) error {
	var dryRun, force, dirs, withIgnored, onlyIgnored, quiet bool
	var paths []string
	for i, a := range args {
		if a == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		switch {
		case a == "--dry-run":
			dryRun = true
		case a == "--force":
			force = true
		case a == "--quiet":
			quiet = true
		case len(a) > 1 && a[0] == '-' && a[1] != '-':
			for _, c := range a[1:] {
				switch c {
				case 'n':
					dryRun = true
				case 'f':
					force = true
				case 'd':
					dirs = true
				case 'x':
					withIgnored = true
				case 'X':
					onlyIgnored = true
				case 'q':
					quiet = true
				default:
					return usagef("error: unknown switch `%c'", c)
				}
			}
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			paths = append(paths, a)
		}
	}
	if withIgnored && onlyIgnored {
		return fatalf("options '-x' and '-X' cannot be used together")
	}
	if !force && !dryRun {
		return fatalf("clean.requireForce is true and -f not given: refusing to clean")
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
	tracked := map[string]bool{}
	trackedDirs := map[string]bool{}
	for _, e := range idx.Entries {
		tracked[e.Name] = true
		for d := path.Dir(e.Name); d != "."; d = path.Dir(d) {
			trackedDirs[d] = true
		}
	}
	m, err := r.ignoreMatcher()
	if err != nil {
		return err
	}
	files, err := r.walkFiles("", m, true)
	if err != nil {
		return err
	}
	selected := func(f wtFile) bool {
		if f.ignored {
			return withIgnored || onlyIgnored
		}
		return !onlyIgnored
	}
	// A directory without tracked files is removed whole with -d, unless it
	// holds files that must be kept; then only its selected files go.
	keep := map[string]bool{}
	topDir := func(p string) string {
		top := ""
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if !trackedDirs[d] {
				top = d
			}
		}
		return top
	}
	for _, f := range files {
		if !tracked[f.path] && !selected(f) {
			if d := topDir(f.path); d != "" {
				keep[d] = true
			}
		}
	}
	targets := map[string]bool{}
	for _, f := range files {
		if tracked[f.path] || f.repo || !selected(f) || !matchAny(specs, f.path) {
			continue
		}
		d := topDir(f.path)
		switch {
		case d == "":
			targets[f.path] = true
		case !dirs:
		case keep[d]:
			targets[f.path] = true
		default:
			targets[d+"/"] = true
		}
	}
	names := make([]string, 0, len(targets))
	for t := range targets {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		if !quiet {
			if dryRun {
				fmt.Fprintf(g.out, "Would remove %s\n", q(r.display(t)))
			} else {
				fmt.Fprintf(g.out, "Removing %s\n", q(r.display(t)))
			}
		}
		if dryRun {
			continue
		}
		if strings.HasSuffix(t, "/") {
			err = r.removeAll(strings.TrimSuffix(t, "/"))
		} else {
			err = r.wt.Remove(t)
		}
		if err != nil {
			fmt.Fprintf(g.err, "warning: failed to remove %s: %v\n", t, err)
		}
	}
	return nil
}

// removeAll deletes a worktree directory tree.
func (r *repo) removeAll(p string) error {
	infos, err := r.wt.ReadDir(p)
	if err != nil {
		return err
	}
	for _, info := range infos {
		child := path.Join(p, info.Name())
		if info.IsDir() {
			err = r.removeAll(child)
		} else {
			err = r.wt.Remove(child)
		}
		if err != nil {
			return err
		}
	}
	return r.wt.Remove(p)
}
