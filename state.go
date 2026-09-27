package git

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Files in the git directory that record an operation in progress.
const (
	mergeHeadFile  = "MERGE_HEAD"
	mergeMsgFile   = "MERGE_MSG"
	mergeModeFile  = "MERGE_MODE"
	squashMsgFile  = "SQUASH_MSG"
	cherryPickFile = "CHERRY_PICK_HEAD"
	revertFile     = "REVERT_HEAD"
	origHeadFile   = "ORIG_HEAD"
)

func (r *repo) gitFile(name string) (string, bool) {
	data, err := fs.ReadFile(r.g.fsys, fsName(path.Join(r.gitDir, name)))
	if err != nil {
		return "", false
	}
	return string(data), true
}

func (r *repo) writeGitFile(name, content string) error {
	f, err := r.g.fsys.OpenFile(fsName(path.Join(r.gitDir, name)), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(content)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r *repo) removeGitFiles(names ...string) {
	for _, name := range names {
		if err := r.g.fsys.Remove(fsName(path.Join(r.gitDir, name))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
	}
}

// stateCommit reads a commit recorded in a state file such as MERGE_HEAD.
func (r *repo) stateCommit(name string) *object.Commit {
	s, ok := r.gitFile(name)
	if !ok {
		return nil
	}
	c, err := r.CommitObject(plumbing.NewHash(strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])))
	if err != nil {
		return nil
	}
	return c
}

func (r *repo) clearOperationState() {
	r.removeGitFiles(mergeHeadFile, mergeMsgFile, mergeModeFile, squashMsgFile, cherryPickFile, revertFile)
}

// errUnmerged is git's refusal to start an operation over unresolved paths.
func (r *repo) errUnmerged(action, fatal string) error {
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	if len(unmergedPaths(idx)) == 0 {
		return nil
	}
	return failf(128, "error: %s is not possible because you have unmerged files.\n"+
		"hint: Fix them up in the work tree, and then use 'git add/rm <file>'\n"+
		"hint: as appropriate to mark resolution and make a commit.\n"+
		"fatal: %s\n", action, fatal)
}

// errNeedsMerge is checkout's refusal while paths are unmerged.
func (r *repo) errNeedsMerge() error {
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	un := unmergedPaths(idx)
	if len(un) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("error: you need to resolve your current index first\n")
	for _, p := range sortedKeys(un) {
		b.WriteString(p + ": needs merge\n")
	}
	return failf(1, "%s", b.String())
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// unmergedCode maps the stages present to git's status code and label.
func unmergedCode(s [4]bool) (string, string) {
	switch {
	case s[1] && s[2] && s[3]:
		return "UU", "both modified:"
	case s[2] && s[3]:
		return "AA", "both added:"
	case s[1] && s[3]:
		return "DU", "deleted by us:"
	case s[1] && s[2]:
		return "UD", "deleted by them:"
	case s[2]:
		return "AU", "added by us:"
	case s[3]:
		return "UA", "added by them:"
	}
	return "DD", "both deleted:"
}

// resolveReflog resolves name@{n}; reflogs are added separately.
func (r *repo) resolveReflog(name, spec, rest, orig string) (plumbing.Hash, error) {
	return plumbing.ZeroHash, errAmbiguous(orig)
}
