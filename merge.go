package git

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// File-level merge: a port of xdiff's xdl_merge at git's default level
// (XDL_MERGE_ZEALOUS) and style (conflict markers without the base).

type hunk struct{ i1, chg1, i2, chg2 int }

// diffHunks returns the changed regions between a and b.
func diffHunks(a, b []string) []hunk {
	ops := diffLines(a, b)
	var out []hunk
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		h := hunk{i1: ops[i].a, i2: ops[i].b}
		for i < len(ops) && ops[i].kind != ' ' {
			if ops[i].kind == '-' {
				h.chg1++
			} else {
				h.chg2++
			}
			i++
		}
		out = append(out, h)
	}
	return out
}

// mergeHunk is one change in the merged output. Modes: 0 conflict, 1 ours,
// 2 theirs, 4 resolved as identical.
type mergeHunk struct {
	mode                         int
	i0, chg0, i1, chg1, i2, chg2 int // base, ours, theirs
}

func appendMerge(ms []mergeHunk, mode, i0, chg0, i1, chg1, i2, chg2 int) []mergeHunk {
	if n := len(ms); n > 0 {
		m := &ms[n-1]
		if i1 <= m.i1+m.chg1 || i2 <= m.i2+m.chg2 {
			if mode != m.mode {
				m.mode = 0
			}
			m.chg0 = i0 + chg0 - m.i0
			m.chg1 = i1 + chg1 - m.i1
			m.chg2 = i2 + chg2 - m.i2
			return ms
		}
	}
	return append(ms, mergeHunk{mode, i0, chg0, i1, chg1, i2, chg2})
}

func equalLines(a []string, i int, b []string, j, n int) bool {
	for k := 0; k < n; k++ {
		if a[i+k] != b[j+k] {
			return false
		}
	}
	return true
}

// merge3 merges ours and theirs against base. It returns the merged lines
// joined, and whether any conflict remains.
func merge3(base, ours, theirs []byte, oursLabel, theirsLabel string) ([]byte, bool) {
	b, o, t := splitLines(base), splitLines(ours), splitLines(theirs)
	x1, x2 := diffHunks(b, o), diffHunks(b, t)
	var ms []mergeHunk
	for len(x1) > 0 && len(x2) > 0 {
		s1, s2 := x1[0], x2[0]
		if s1.i1+s1.chg1 < s2.i1 {
			ms = appendMerge(ms, 1, s1.i1, s1.chg1, s1.i2, s1.chg2, s2.i2-s2.i1+s1.i1, s1.chg1)
			x1 = x1[1:]
			continue
		}
		if s2.i1+s2.chg1 < s1.i1 {
			ms = appendMerge(ms, 2, s2.i1, s2.chg1, s1.i2-s1.i1+s2.i1, s2.chg1, s2.i2, s2.chg2)
			x2 = x2[1:]
			continue
		}
		if s1.i1 != s2.i1 || s1.chg1 != s2.chg1 || s1.chg2 != s2.chg2 || !equalLines(o, s1.i2, t, s2.i2, s1.chg2) {
			off := s1.i1 - s2.i1
			ffo := off + s1.chg1 - s2.chg1
			i0, i1, i2 := s1.i1, s1.i2, s2.i2
			if off > 0 {
				i0 -= off
				i1 -= off
			} else {
				i2 += off
			}
			chg0 := s1.i1 + s1.chg1 - i0
			chg1 := s1.i2 + s1.chg2 - i1
			chg2 := s2.i2 + s2.chg2 - i2
			if ffo < 0 {
				chg0 -= ffo
				chg1 -= ffo
			} else {
				chg2 += ffo
			}
			ms = appendMerge(ms, 0, i0, chg0, i1, chg1, i2, chg2)
		}
		e1, e2 := s1.i1+s1.chg1, s2.i1+s2.chg1
		if e1 >= e2 {
			x2 = x2[1:]
		}
		if e2 >= e1 {
			x1 = x1[1:]
		}
	}
	for _, s1 := range x1 {
		ms = appendMerge(ms, 1, s1.i1, s1.chg1, s1.i2, s1.chg2, s1.i1+len(t)-len(b), s1.chg1)
	}
	for _, s2 := range x2 {
		ms = appendMerge(ms, 2, s2.i1, s2.chg1, s2.i1+len(o)-len(b), s2.chg1, s2.i2, s2.chg2)
	}
	ms = refineConflicts(ms, o, t)
	ms = simplifyNonConflicts(ms)

	var out strings.Builder
	conflict := false
	copyLines := func(lines []string, from, n int, addNL bool) {
		for k := from; k < from+n; k++ {
			out.WriteString(lines[k])
		}
		if addNL && n > 0 && !strings.HasSuffix(lines[from+n-1], "\n") {
			out.WriteString("\n")
		}
	}
	i := 0
	for _, m := range ms {
		switch {
		case m.mode == 0:
			conflict = true
			copyLines(o, i, m.i1-i, false)
			out.WriteString("<<<<<<< " + oursLabel + "\n")
			copyLines(o, m.i1, m.chg1, true)
			out.WriteString("=======\n")
			copyLines(t, m.i2, m.chg2, true)
			out.WriteString(">>>>>>> " + theirsLabel + "\n")
		case m.mode&3 != 0:
			copyLines(o, i, m.i1-i, false)
			if m.mode&1 != 0 {
				copyLines(o, m.i1, m.chg1, m.mode&2 != 0)
			}
			if m.mode&2 != 0 {
				copyLines(t, m.i2, m.chg2, false)
			}
		default:
			continue
		}
		i = m.i1 + m.chg1
	}
	copyLines(o, i, len(o)-i, false)
	return []byte(out.String()), conflict
}

// refineConflicts splits each conflict by diffing its two sides, so lines
// both sides agree on are no longer part of it (XDL_MERGE_ZEALOUS).
func refineConflicts(ms []mergeHunk, o, t []string) []mergeHunk {
	var out []mergeHunk
	for _, m := range ms {
		if m.mode != 0 || m.chg1 == 0 || m.chg2 == 0 {
			out = append(out, m)
			continue
		}
		xs := diffHunks(o[m.i1:m.i1+m.chg1], t[m.i2:m.i2+m.chg2])
		if len(xs) == 0 {
			m.mode = 4
			out = append(out, m)
			continue
		}
		for _, x := range xs {
			out = append(out, mergeHunk{mode: 0, i0: m.i0, chg0: m.chg0,
				i1: x.i1 + m.i1, chg1: x.chg1, i2: x.i2 + m.i2, chg2: x.chg2})
		}
	}
	return out
}

// simplifyNonConflicts joins conflicts separated by at most three lines.
func simplifyNonConflicts(ms []mergeHunk) []mergeHunk {
	if len(ms) == 0 {
		return ms
	}
	out := []mergeHunk{ms[0]}
	for _, next := range ms[1:] {
		m := &out[len(out)-1]
		begin, end := m.i1+m.chg1, next.i1
		if m.mode != 0 || next.mode != 0 || end-begin > 3 {
			out = append(out, next)
			continue
		}
		m.chg0 = next.i0 + next.chg0 - m.i0
		m.chg1 = next.i1 + next.chg1 - m.i1
		m.chg2 = next.i2 + next.chg2 - m.i2
	}
	return out
}

// Tree-level merge, following merge-ort's handling of each path.

// mergedPath is the outcome for one path.
type mergedPath struct {
	path     string
	result   *entry    // stage-0 result; nil if deleted or conflicted
	stages   [3]*entry // base, ours, theirs for a conflict
	worktree []byte    // file content left in the worktree for a conflict
	conflict string    // "content", "add/add", "modify/delete"
	deleted  bool      // resolved as a deletion
}

type treeMerge struct {
	paths    []mergedPath
	messages []string
}

func (tm *treeMerge) conflicted() []string {
	var out []string
	for _, p := range tm.paths {
		if p.conflict != "" {
			out = append(out, p.path)
		}
	}
	return out
}

func sameEntry(a, b *entry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.hash == b.hash && a.mode == b.mode
}

// mergeTrees merges ours and theirs relative to base. Labels name the
// sides in conflict markers and messages.
func (r *repo) mergeTrees(base, ours, theirs side, oursLabel, theirsLabel string) (*treeMerge, error) {
	names := map[string]bool{}
	for _, s := range []side{base, ours, theirs} {
		for p := range s {
			names[p] = true
		}
	}
	paths := make([]string, 0, len(names))
	for p := range names {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	tm := &treeMerge{}
	get := func(s side, p string) *entry {
		if e, ok := s[p]; ok {
			return &e
		}
		return nil
	}
	for _, p := range paths {
		b, o, t := get(base, p), get(ours, p), get(theirs, p)
		mp := mergedPath{path: p}
		switch {
		case sameEntry(o, t):
			mp.result, mp.deleted = o, o == nil
		case sameEntry(b, o):
			mp.result, mp.deleted = t, t == nil
		case sameEntry(b, t):
			mp.result, mp.deleted = o, o == nil
		case o == nil || t == nil:
			// Modified on one side, deleted on the other.
			mp.conflict, mp.stages = "modify/delete", [3]*entry{b, o, t}
			kept, deletedIn, modifiedIn := t, oursLabel, theirsLabel
			if t == nil {
				kept, deletedIn, modifiedIn = o, theirsLabel, oursLabel
			}
			data, err := kept.data()
			if err != nil {
				return nil, err
			}
			mp.worktree = data
			tm.messages = append(tm.messages, fmt.Sprintf("CONFLICT (modify/delete): %s deleted in %s and modified in %s.  Version %s of %s left in tree.",
				p, deletedIn, modifiedIn, modifiedIn, p))
		default:
			var baseData []byte
			if b != nil {
				d, err := b.data()
				if err != nil {
					return nil, err
				}
				baseData = d
			}
			od, err := o.data()
			if err != nil {
				return nil, err
			}
			td, err := t.data()
			if err != nil {
				return nil, err
			}
			tm.messages = append(tm.messages, "Auto-merging "+p)
			kind := "content"
			if b == nil {
				kind = "add/add"
			}
			if isBinary(baseData) || isBinary(od) || isBinary(td) {
				tm.messages = append(tm.messages, fmt.Sprintf("warning: Cannot merge binary files: %s (%s vs. %s)", p, oursLabel, theirsLabel),
					fmt.Sprintf("CONFLICT (%s): Merge conflict in %s", kind, p))
				mp.conflict, mp.stages, mp.worktree = kind, [3]*entry{b, o, t}, od
				break
			}
			merged, conflict := merge3(baseData, od, td, oursLabel, theirsLabel)
			mode := o.mode
			if b != nil && o.mode == b.mode {
				mode = t.mode
			}
			if conflict {
				tm.messages = append(tm.messages, fmt.Sprintf("CONFLICT (%s): Merge conflict in %s", kind, p))
				mp.conflict, mp.stages, mp.worktree = kind, [3]*entry{b, o, t}, merged
				break
			}
			e, err := r.writeBlob(merged, mode)
			if err != nil {
				return nil, err
			}
			mp.result = &e
		}
		tm.paths = append(tm.paths, mp)
	}
	return tm, nil
}

// mergeBase returns the best common ancestor of a and b, or nil.
func (r *repo) mergeBase(a, b plumbing.Hash) (*object.Commit, error) {
	common := r.ancestors(a)
	other := r.ancestors(b)
	var cands []plumbing.Hash
	for h := range other {
		if common[h] {
			cands = append(cands, h)
		}
	}
	// Drop candidates that are ancestors of other candidates.
	var best []*object.Commit
	for _, h := range cands {
		dominated := false
		for _, o := range cands {
			if o != h && r.ancestors(o)[h] {
				dominated = true
				break
			}
		}
		if !dominated {
			c, err := r.CommitObject(h)
			if err != nil {
				return nil, err
			}
			best = append(best, c)
		}
	}
	if len(best) == 0 {
		return nil, nil
	}
	sort.Slice(best, func(i, j int) bool { return best[i].Committer.When.After(best[j].Committer.When) })
	return best[0], nil
}

// Applying a merge to the index and worktree.

// applyMerge checks that no local change is overwritten, then writes the
// merge result to the index and worktree. op names the command in errors.
func (r *repo) applyMerge(tm *treeMerge, ours side, op string) error {
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	cur := r.indexSide(idx)
	var dirty, untracked []string
	for _, mp := range tm.paths {
		o := ours[mp.path]
		oursEntry := &o
		if _, ok := ours[mp.path]; !ok {
			oursEntry = nil
		}
		if mp.conflict == "" && sameEntry(mp.result, oursEntry) {
			continue
		}
		ie, inIndex := cur[mp.path]
		we, inWork, err := r.worktreeEntry(mp.path)
		if err != nil {
			return err
		}
		switch {
		case oursEntry == nil && !inIndex && inWork:
			if mp.result == nil || we.hash != mp.result.hash {
				untracked = append(untracked, mp.path)
			}
		case oursEntry != nil && (!inIndex || ie.hash != oursEntry.hash || !inWork || we.hash != ie.hash):
			dirty = append(dirty, mp.path)
		case oursEntry == nil && inIndex:
			dirty = append(dirty, mp.path)
		}
	}
	verb := map[string]string{"merge": "merge", "cherry-pick": "merge", "revert": "merge"}[op]
	if len(dirty) > 0 {
		return failf(1, "error: Your local changes to the following files would be overwritten by %s:\n\t%s\n"+
			"Please commit your changes or stash them before you %s.\nAborting\n", verb, strings.Join(dirty, "\n\t"), verb)
	}
	if len(untracked) > 0 {
		return failf(1, "error: The following untracked working tree files would be overwritten by %s:\n\t%s\n"+
			"Please move or remove them before you %s.\nAborting\n", verb, strings.Join(untracked, "\n\t"), verb)
	}
	for _, mp := range tm.paths {
		o, inOurs := ours[mp.path]
		switch {
		case mp.conflict != "":
			removeEntry(idx, mp.path)
			for stage, e := range mp.stages {
				if e != nil {
					addStage(idx, mp.path, *e, stage+1)
				}
			}
			mode := filemode.Regular
			if mp.stages[1] != nil {
				mode = mp.stages[1].mode
			} else if mp.stages[2] != nil {
				mode = mp.stages[2].mode
			}
			data := mp.worktree
			if _, err := r.writeFile(mp.path, entry{mode: mode, data: func() ([]byte, error) { return data, nil }}); err != nil {
				return err
			}
		case mp.deleted:
			if inOurs {
				removeEntry(idx, mp.path)
				if err := r.removeFile(mp.path); err != nil {
					return err
				}
			}
		case !inOurs || !sameEntry(mp.result, &o):
			info, err := r.writeFile(mp.path, *mp.result)
			if err != nil {
				return err
			}
			setEntry(idx, mp.path, *mp.result, info)
		}
	}
	return r.writeIndex(idx)
}

// mergedSide is the stage-0 result of a clean merge.
func (tm *treeMerge) mergedSide() side {
	s := side{}
	for _, mp := range tm.paths {
		if mp.result != nil {
			s[mp.path] = *mp.result
		}
	}
	return s
}
