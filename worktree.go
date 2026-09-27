package git

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// wtFile is a regular file found in the worktree.
type wtFile struct {
	path    string
	ignored bool
}

func (r *repo) ignoreMatcher() (gitignore.Matcher, error) {
	patterns, err := gitignore.ReadPatterns(r.wt, nil)
	if err != nil {
		return nil, err
	}
	if data, err := fs.ReadFile(r.g.fsys, fsName(path.Join(r.gitDir, "info/exclude"))); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				patterns = append(patterns, gitignore.ParsePattern(line, nil))
			}
		}
	}
	return gitignore.NewMatcher(patterns), nil
}

// walkFiles lists worktree files below dir ("" for the top), skipping .git
// directories. Ignored directories are only entered when withIgnored is set.
func (r *repo) walkFiles(dir string, m gitignore.Matcher, withIgnored bool) ([]wtFile, error) {
	var out []wtFile
	var walk func(dir string, ignored bool) error
	walk = func(dir string, ignored bool) error {
		infos, err := r.wt.ReadDir(dirOrDot(dir))
		if err != nil {
			return err
		}
		for _, info := range infos {
			p := path.Join(dir, info.Name())
			if info.Name() == ".git" {
				continue
			}
			ign := ignored || m.Match(strings.Split(p, "/"), info.IsDir())
			switch {
			case info.IsDir():
				if !ign || withIgnored {
					if err := walk(p, ign); err != nil {
						return err
					}
				}
			case info.Mode().IsRegular():
				out = append(out, wtFile{path: p, ignored: ign})
			}
		}
		return nil
	}
	return out, walk(dir, false)
}

func dirOrDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

type fileStatus struct {
	path             string
	staged, unstaged byte   // ' ', 'A', 'M', 'D', 'R'; 'U' etc. when unmerged
	oldPath          string // source of a staged rename
	unmerged         string // long-status label of an unmerged path
}

type repoStatus struct {
	branch    string // "" when detached
	head      *object.Commit
	tracked   []fileStatus
	untracked []string // directories end in "/"
	ignored   []string // only filled by addIgnored
}

func (s *repoStatus) hasStaged() bool {
	for _, f := range s.tracked {
		if f.staged != ' ' && f.unmerged == "" {
			return true
		}
	}
	return false
}

func (s *repoStatus) hasUnmerged() bool {
	for _, f := range s.tracked {
		if f.unmerged != "" {
			return true
		}
	}
	return false
}

func (s *repoStatus) hasUnstaged() bool {
	for _, f := range s.tracked {
		if f.unstaged != ' ' && f.unmerged == "" {
			return true
		}
	}
	return false
}

// computeStatus compares HEAD, the index, and the worktree. With collapse,
// untracked directories without tracked files are reported as "dir/".
func (r *repo) computeStatus(specs []string, collapse, withUntracked bool) (*repoStatus, error) {
	st := &repoStatus{}
	var err error
	if st.branch, err = r.branchName(); err != nil {
		return nil, err
	}
	if st.head, err = r.headCommit(); err != nil {
		return nil, err
	}
	headSide, err := r.treeSide(st.head)
	if err != nil {
		return nil, err
	}
	idx, err := r.readIndex()
	if err != nil {
		return nil, err
	}
	indexSide := r.indexSide(idx)
	paths := make([]string, 0, len(indexSide))
	for p := range indexSide {
		paths = append(paths, p)
	}
	work, err := r.worktreeSide(paths)
	if err != nil {
		return nil, err
	}
	byPath := map[string]*fileStatus{}
	get := func(p string) *fileStatus {
		if byPath[p] == nil {
			byPath[p] = &fileStatus{path: p, staged: ' ', unstaged: ' '}
		}
		return byPath[p]
	}
	staged, err := detectRenames(changes(headSide, indexSide, specs))
	if err != nil {
		return nil, err
	}
	for _, c := range staged {
		f := get(c.path)
		f.staged, f.oldPath = changeCode(c), c.oldPath
	}
	for _, c := range changes(indexSide, work, specs) {
		if c.from != nil { // worktree-only files are untracked, handled below
			get(c.path).unstaged = changeCode(c)
		}
	}
	// Unmerged paths replace whatever the stage-0 comparison found.
	for p, stages := range unmergedPaths(idx) {
		if !matchAny(specs, p) {
			continue
		}
		code, label := unmergedCode(stages)
		byPath[p] = &fileStatus{path: p, staged: code[0], unstaged: code[1], unmerged: label}
	}
	for _, f := range byPath {
		st.tracked = append(st.tracked, *f)
	}
	sort.Slice(st.tracked, func(i, j int) bool { return st.tracked[i].path < st.tracked[j].path })
	if !withUntracked {
		return st, nil
	}

	m, err := r.ignoreMatcher()
	if err != nil {
		return nil, err
	}
	files, err := r.walkFiles("", m, false)
	if err != nil {
		return nil, err
	}
	unmerged := unmergedPaths(idx)
	for p := range unmerged {
		indexSide[p] = entry{}
	}
	trackedDirs := map[string]bool{"": true}
	for p := range indexSide {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			trackedDirs[d] = true
		}
	}
	seen := map[string]bool{}
	for _, f := range files {
		if _, tracked := indexSide[f.path]; tracked || f.ignored || !matchAny(specs, f.path) {
			continue
		}
		name := f.path
		if collapse {
			// Report the outermost directory that holds no tracked files.
			for d := path.Dir(f.path); d != "."; d = path.Dir(d) {
				if !trackedDirs[d] {
					name = d + "/"
				}
			}
		}
		if !seen[name] {
			seen[name] = true
			st.untracked = append(st.untracked, name)
		}
	}
	sort.Strings(st.untracked)
	return st, nil
}

// addIgnored lists ignored untracked files. With collapse, a directory that
// holds only ignored untracked files is reported as "dir/".
func (r *repo) addIgnored(st *repoStatus, specs []string, collapse bool) error {
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	keep := map[string]bool{} // directories with tracked or non-ignored files
	for _, e := range idx.Entries {
		tracked[e.Name] = true
		for d := path.Dir(e.Name); d != "."; d = path.Dir(d) {
			keep[d] = true
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
	for _, f := range files {
		if !tracked[f.path] && !f.ignored {
			for d := path.Dir(f.path); d != "."; d = path.Dir(d) {
				keep[d] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, f := range files {
		if tracked[f.path] || !f.ignored || !matchAny(specs, f.path) {
			continue
		}
		name := f.path
		if collapse {
			for d := path.Dir(f.path); d != "."; d = path.Dir(d) {
				if !keep[d] {
					name = d + "/"
				}
			}
		}
		if !seen[name] {
			seen[name] = true
			st.ignored = append(st.ignored, name)
		}
	}
	sort.Strings(st.ignored)
	return nil
}

func changeCode(c change) byte {
	switch {
	case c.oldPath != "":
		return 'R'
	case c.from == nil:
		return 'A'
	case c.to == nil:
		return 'D'
	}
	return 'M'
}

// Writing the worktree and index

func (r *repo) writeFile(p string, e entry) (os.FileInfo, error) {
	data, err := e.data()
	if err != nil {
		return nil, err
	}
	if info, err := r.wt.Stat(p); err == nil && info.IsDir() {
		return nil, fmt.Errorf("cannot overwrite directory %s", p)
	}
	perm := os.FileMode(0666)
	if e.mode == filemode.Executable {
		perm = 0777
		r.wt.Remove(p) // a new file picks up the executable permission
	}
	f, err := r.wt.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return r.wt.Stat(p)
}

// removeFile deletes a worktree file and any parent directories it empties.
func (r *repo) removeFile(p string) error {
	if err := r.wt.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		if infos, err := r.wt.ReadDir(d); err != nil || len(infos) > 0 || r.wt.Remove(d) != nil {
			break
		}
	}
	return nil
}

// setEntry stores p at stage 0, resolving any conflict stages.
func setEntry(idx *index.Index, p string, e entry, info os.FileInfo) {
	var ie *index.Entry
	kept := idx.Entries[:0]
	for _, x := range idx.Entries {
		switch {
		case x.Name != p:
		case x.Stage == stageMerged:
			ie = x
		default:
			continue
		}
		kept = append(kept, x)
	}
	idx.Entries = kept
	if ie == nil {
		ie = &index.Entry{Name: p}
		idx.Entries = append(idx.Entries, ie)
	}
	ie.Hash, ie.Mode = e.hash, e.mode
	ie.Size = 0
	if info != nil {
		ie.Size = uint32(info.Size())
		ie.ModifiedAt = info.ModTime()
	}
}

// stageMerged is the on-disk stage of a normal entry. go-git's
// index.Merged constant is 1, which collides with the base stage.
const stageMerged index.Stage = 0

// removeEntry drops p from the index at every stage.
func removeEntry(idx *index.Index, p string) {
	kept := idx.Entries[:0]
	for _, x := range idx.Entries {
		if x.Name != p {
			kept = append(kept, x)
		}
	}
	idx.Entries = kept
}

// addStage adds a conflict stage (1 base, 2 ours, 3 theirs) for p.
func addStage(idx *index.Index, p string, e entry, stage int) {
	idx.Entries = append(idx.Entries, &index.Entry{Name: p, Hash: e.hash, Mode: e.mode, Stage: index.Stage(stage)})
}

// unmergedPaths returns the conflict stages present for each unmerged path.
func unmergedPaths(idx *index.Index) map[string][4]bool {
	out := map[string][4]bool{}
	for _, e := range idx.Entries {
		if e.Stage != stageMerged {
			s := out[e.Name]
			s[e.Stage] = true
			out[e.Name] = s
		}
	}
	return out
}

// writeBlob stores data as a blob and returns its entry.
func (r *repo) writeBlob(data []byte, mode filemode.FileMode) (entry, error) {
	obj := r.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return entry{}, err
	}
	if _, err := w.Write(data); err != nil {
		return entry{}, err
	}
	if err := w.Close(); err != nil {
		return entry{}, err
	}
	h, err := r.Storer.SetEncodedObject(obj)
	if err != nil {
		return entry{}, err
	}
	return entry{hash: h, mode: mode, data: func() ([]byte, error) { return data, nil }}, nil
}

// stageFile writes the worktree file p to the object store and index.
func (r *repo) stageFile(idx *index.Index, p string) error {
	e, ok, err := r.worktreeEntry(p)
	if err != nil {
		return err
	}
	if !ok {
		removeEntry(idx, p)
		return nil
	}
	if cur, err := idx.Entry(p); err == nil && cur.Hash == e.hash && cur.Mode == e.mode {
		return nil
	}
	data, _ := e.data()
	if _, err := r.writeBlob(data, e.mode); err != nil {
		return err
	}
	info, _ := r.wt.Stat(p)
	setEntry(idx, p, e, info)
	return nil
}

// switchTo moves the index and worktree from HEAD to target using git's
// two-way merge rules: paths that differ between the two trees must be
// clean, and untracked files may not be overwritten. It does not move HEAD.
func (r *repo) switchTo(target *object.Commit, op string, force bool) error {
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	from, err := r.treeSide(head)
	if err != nil {
		return err
	}
	to, err := r.treeSide(target)
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	cur := r.indexSide(idx)
	moves := changes(from, to, nil)
	if !force {
		var dirty, untracked []string
		for _, c := range moves {
			ie, inIndex := cur[c.path]
			we, inWork, err := r.worktreeEntry(c.path)
			if err != nil {
				return err
			}
			switch {
			case c.from == nil && !inIndex && inWork:
				if c.to == nil || we.hash != c.to.hash {
					untracked = append(untracked, c.path)
				}
			case c.from != nil && (!inIndex || ie.hash != c.from.hash || !inWork || we.hash != ie.hash):
				// Local changes are fine if they already match the target.
				if !(c.to != nil && inIndex && ie.hash == c.to.hash && inWork && we.hash == c.to.hash) {
					dirty = append(dirty, c.path)
				}
			case c.from == nil && inIndex:
				if c.to == nil || ie.hash != c.to.hash {
					dirty = append(dirty, c.path)
				}
			}
		}
		if len(dirty) > 0 {
			return failf(1, "error: Your local changes to the following files would be overwritten by %s:\n\t%s\n"+
				"Please commit your changes or stash them before you %s.\nAborting\n",
				op, strings.Join(dirty, "\n\t"), map[string]string{"checkout": "switch branches", "merge": "merge"}[op])
		}
		if len(untracked) > 0 {
			return failf(1, "error: The following untracked working tree files would be overwritten by %s:\n\t%s\n"+
				"Please move or remove them before you %s.\nAborting\n",
				op, strings.Join(untracked, "\n\t"), map[string]string{"checkout": "switch branches", "merge": "merge"}[op])
		}
	}
	for _, c := range moves {
		if c.to == nil {
			removeEntry(idx, c.path)
			if err := r.removeFile(c.path); err != nil {
				return err
			}
			continue
		}
		info, err := r.writeFile(c.path, *c.to)
		if err != nil {
			return err
		}
		setEntry(idx, c.path, *c.to, info)
	}
	return r.writeIndex(idx)
}

// resetIndex makes the index match target for the selected paths.
func (r *repo) resetIndex(target *object.Commit, specs []string) error {
	to, err := r.treeSide(target)
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	for _, c := range changes(r.indexSide(idx), to, specs) {
		if c.to == nil {
			removeEntry(idx, c.path)
		} else {
			info, _ := r.wt.Stat(c.path)
			setEntry(idx, c.path, *c.to, info)
		}
	}
	return r.writeIndex(idx)
}

// resetHard makes the index and tracked worktree files match target.
func (r *repo) resetHard(target *object.Commit) error {
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	headSide, err := r.treeSide(head)
	if err != nil {
		return err
	}
	to, err := r.treeSide(target)
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	paths := map[string]bool{}
	for _, s := range []side{headSide, r.indexSide(idx), to} {
		for p := range s {
			paths[p] = true
		}
	}
	for p := range unmergedPaths(idx) {
		paths[p] = true
	}
	idx.Entries = nil
	for p := range paths {
		e, ok := to[p]
		if !ok {
			if err := r.removeFile(p); err != nil {
				return err
			}
			continue
		}
		we, inWork, err := r.worktreeEntry(p)
		if err != nil {
			return err
		}
		var info os.FileInfo
		if inWork && we.hash == e.hash && we.mode == e.mode {
			info, _ = r.wt.Stat(p)
		} else if info, err = r.writeFile(p, e); err != nil {
			return err
		}
		setEntry(idx, p, e, info)
	}
	return r.writeIndex(idx)
}
