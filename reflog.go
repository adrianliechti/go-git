package git

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// Reflogs live in <gitdir>/logs/<refname>, one line per update:
// "<old> <new> <name> <<email>> <unix> <tz>\t<message>".

type reflogEntry struct {
	old, new plumbing.Hash
	who      string // "Name <email>"
	when     time.Time
	msg      string
}

// reflogPath is per worktree for HEAD and shared for other refs.
func (r *repo) reflogPath(name plumbing.ReferenceName) string {
	dir := r.commonDir
	if name == plumbing.HEAD {
		dir = r.gitDir
	}
	return fsName(path.Join(dir, "logs", name.String()))
}

// logsRef reports whether updates to name are logged, following git's
// core.logAllRefUpdates default: on in non-bare repositories for HEAD,
// branches, remote-tracking refs, and notes; the stash is always logged.
func (r *repo) logsRef(name plumbing.ReferenceName) bool {
	if name == "refs/stash" {
		return true
	}
	if r.bare() {
		return false
	}
	return name == plumbing.HEAD || name.IsBranch() || name.IsRemote() || name.IsNote()
}

func (r *repo) readReflog(name plumbing.ReferenceName) []reflogEntry {
	data, err := fs.ReadFile(r.g.fsys, r.reflogPath(name))
	if err != nil {
		return nil
	}
	var out []reflogEntry
	for _, line := range strings.Split(string(data), "\n") {
		head, msg, ok := strings.Cut(line, "\t")
		if !ok || len(head) < 83 {
			continue
		}
		e := reflogEntry{old: plumbing.NewHash(head[:40]), new: plumbing.NewHash(head[41:81]), msg: msg}
		rest := head[82:]
		if i := strings.LastIndex(rest, "> "); i >= 0 {
			e.who = rest[:i+1]
			if t, err := parseDate(rest[i+2:]); err == nil {
				e.when = t
			}
		}
		out = append(out, e)
	}
	return out
}

func (r *repo) writeReflog(name plumbing.ReferenceName, entries []reflogEntry) error {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s %s %d %s\t%s\n", e.old, e.new, e.who, e.when.Unix(), e.when.Format("-0700"), e.msg)
	}
	if err := r.wtless().mkdirAll(path.Dir(r.reflogPath(name))); err != nil {
		return err
	}
	f, err := r.g.fsys.OpenFile(r.reflogPath(name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(b.String())); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// wtless is a billy view of the whole FS, for creating log directories.
func (r *repo) wtless() *billyFS { return newBillyFS(r.g.fsys, ".") }

func (r *repo) appendReflog(name plumbing.ReferenceName, old, new plumbing.Hash, msg string) {
	if !r.logsRef(name) {
		return
	}
	sig, err := r.signature("COMMITTER")
	if err != nil {
		return // git would refuse; without an identity we skip the log
	}
	entries := append(r.readReflog(name), reflogEntry{old: old, new: new, who: ident(*sig), when: sig.When, msg: msg})
	r.writeReflog(name, entries)
}

func (r *repo) deleteReflog(name plumbing.ReferenceName) {
	if err := r.g.fsys.Remove(r.reflogPath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return
	}
}

func (r *repo) refHash(name plumbing.ReferenceName) plumbing.Hash {
	if ref, err := r.Reference(name, true); err == nil {
		return ref.Hash()
	}
	return plumbing.ZeroHash
}

// updateRef points name at h and records it in the reflogs. When name is
// the checked-out branch, HEAD's reflog gets the entry too.
func (r *repo) updateRef(name plumbing.ReferenceName, h plumbing.Hash, msg string) error {
	old := r.refHash(name)
	if err := r.Storer.SetReference(plumbing.NewHashReference(name, h)); err != nil {
		return err
	}
	r.appendReflog(name, old, h, msg)
	if head, err := r.Storer.Reference(plumbing.HEAD); err == nil && head.Type() == plumbing.SymbolicReference && head.Target() == name {
		r.appendReflog(plumbing.HEAD, old, h, msg)
	}
	return nil
}

// deleteRef removes a ref and its reflog.
func (r *repo) deleteRef(name plumbing.ReferenceName) error {
	if err := r.Storer.RemoveReference(name); err != nil {
		return err
	}
	r.deleteReflog(name)
	return nil
}

// setHead moves the current branch (or a detached HEAD) to h.
func (r *repo) setHead(h plumbing.Hash, msg string) error {
	ref, err := r.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return err
	}
	if ref.Type() == plumbing.SymbolicReference {
		return r.updateRef(ref.Target(), h, msg)
	}
	old := ref.Hash()
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, h)); err != nil {
		return err
	}
	r.appendReflog(plumbing.HEAD, old, h, msg)
	return nil
}

// pointHead makes HEAD a symbolic ref to branch, or detaches it at h, and
// logs the move like git checkout.
func (r *repo) pointHead(branch plumbing.ReferenceName, h plumbing.Hash, msg string) error {
	old := r.refHash(plumbing.HEAD)
	var ref *plumbing.Reference
	if branch != "" {
		ref = plumbing.NewSymbolicReference(plumbing.HEAD, branch)
		h = r.refHash(branch)
	} else {
		ref = plumbing.NewHashReference(plumbing.HEAD, h)
	}
	if err := r.Storer.SetReference(ref); err != nil {
		return err
	}
	r.appendReflog(plumbing.HEAD, old, h, msg)
	return nil
}

// headName is how git names the current position in "moving from" messages.
func (r *repo) headName() string {
	if b, _ := r.branchName(); b != "" {
		return b
	}
	return r.refHash(plumbing.HEAD).String()
}

// reflogRef maps the name in name@{n} to a ref: "" is the current branch.
func (r *repo) reflogRef(name string) (plumbing.ReferenceName, error) {
	if name == "" {
		if b, _ := r.branchName(); b != "" {
			return plumbing.NewBranchReferenceName(b), nil
		}
		return plumbing.HEAD, nil
	}
	if name == "HEAD" {
		return plumbing.HEAD, nil
	}
	for _, cand := range []string{name, "refs/" + name, "refs/heads/" + name, "refs/remotes/" + name, "refs/tags/" + name} {
		if _, err := r.Storer.Reference(plumbing.ReferenceName(cand)); err == nil {
			return plumbing.ReferenceName(cand), nil
		}
	}
	return "", errAmbiguous(name)
}

// resolveReflog resolves name@{n} and @{-n}.
func (r *repo) resolveReflog(name, spec, rest, orig string) (plumbing.Hash, error) {
	if n, err := strconv.Atoi(spec); err == nil && n < 0 && name == "" {
		prev, ok := r.previousBranch(-n)
		if !ok {
			return plumbing.ZeroHash, errAmbiguous(orig)
		}
		return r.resolveRev(prev + rest)
	}
	n, err := strconv.Atoi(spec)
	if err != nil || n < 0 {
		return plumbing.ZeroHash, errAmbiguous(orig)
	}
	ref, err := r.reflogRef(name)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	entries := r.readReflog(ref)
	if n >= len(entries) {
		if len(entries) == 0 {
			return plumbing.ZeroHash, errAmbiguous(orig)
		}
		return plumbing.ZeroHash, fatalf("log for '%s' only has %d entries", strings.TrimPrefix(shortRef(ref), "refs/"), len(entries))
	}
	h := entries[len(entries)-1-n].new
	if rest == "" {
		return h, nil
	}
	return r.resolveRev(h.String() + rest)
}

// previousBranch returns the n-th previously checked-out branch or commit.
func (r *repo) previousBranch(n int) (string, bool) {
	entries := r.readReflog(plumbing.HEAD)
	for i := len(entries) - 1; i >= 0; i-- {
		from, ok := strings.CutPrefix(entries[i].msg, "checkout: moving from ")
		if !ok {
			continue
		}
		if n--; n == 0 {
			name, _, _ := strings.Cut(from, " to ")
			return name, true
		}
	}
	return "", false
}

// reflogCmd implements git reflog [show|exists] [ref].
func (g *gitRun) reflogCmd(args []string) error {
	r, err := g.openAny()
	if err != nil {
		return err
	}
	sub := "show"
	if len(args) > 0 && (args[0] == "show" || args[0] == "exists" || args[0] == "delete" || args[0] == "expire") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "exists":
		if len(args) != 1 {
			return usagef("usage: git reflog exists <ref>")
		}
		// exists takes a full refname; "main" alone does not count.
		if r.readReflog(plumbing.ReferenceName(args[0])) == nil {
			return failf(1, "")
		}
		return nil
	case "delete", "expire":
		return fatalf("git reflog %s is not supported", sub)
	}
	logArgs := []string{"-g", "--oneline"}
	return g.log(append(logArgs, args...))
}

// reflogCommits lists the entries of ref's reflog, newest first, as log -g
// shows them.
type reflogWalk struct {
	ref     plumbing.ReferenceName
	display string // "HEAD", "main"
	entries []reflogEntry
}

func (r *repo) reflogWalk(rev string) (*reflogWalk, error) {
	name := strings.TrimSuffix(rev, "@{0}")
	ref, err := r.reflogRef(name)
	if err != nil {
		return nil, err
	}
	display := ref.String()
	switch {
	case name == "":
		display, ref = "HEAD", plumbing.HEAD
	case ref.IsBranch():
		display = ref.Short()
	}
	entries := r.readReflog(ref)
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return &reflogWalk{ref: ref, display: display, entries: entries}, nil
}
