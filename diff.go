package git

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// entry is one file on a side of a comparison: a tree, the index, or the
// worktree.
type entry struct {
	hash plumbing.Hash
	mode filemode.FileMode
	data func() ([]byte, error)
}

type side map[string]entry

func (r *repo) blobData(h plumbing.Hash) func() ([]byte, error) {
	return func() ([]byte, error) {
		b, err := r.BlobObject(h)
		if err != nil {
			return nil, err
		}
		rd, err := b.Reader()
		if err != nil {
			return nil, err
		}
		defer rd.Close()
		return io.ReadAll(rd)
	}
}

// treeSide lists a commit's files; a nil commit is the empty tree.
func (r *repo) treeSide(c *object.Commit) (side, error) {
	s := side{}
	if c == nil {
		return s, nil
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	err = tree.Files().ForEach(func(f *object.File) error {
		s[f.Name] = entry{hash: f.Hash, mode: f.Mode, data: r.blobData(f.Hash)}
		return nil
	})
	return s, err
}

func (r *repo) indexSide(idx *index.Index) side {
	s := side{}
	for _, e := range idx.Entries {
		s[e.Name] = entry{hash: e.Hash, mode: e.Mode, data: r.blobData(e.Hash)}
	}
	return s
}

// worktreeSide reads the given repository paths from the worktree; missing
// files are omitted.
func (r *repo) worktreeSide(paths []string) (side, error) {
	s := side{}
	for _, p := range paths {
		e, ok, err := r.worktreeEntry(p)
		if err != nil {
			return nil, err
		}
		if ok {
			s[p] = e
		}
	}
	return s, nil
}

func (r *repo) worktreeEntry(p string) (entry, bool, error) {
	info, err := r.wt.Stat(p)
	if err != nil || info.IsDir() {
		return entry{}, false, nil
	}
	full, _ := r.wt.full(p)
	data, err := fs.ReadFile(r.g.fsys, full)
	if err != nil {
		return entry{}, false, err
	}
	mode := filemode.Regular
	if info.Mode().Perm()&0111 != 0 {
		mode = filemode.Executable
	}
	return entry{
		hash: plumbing.ComputeHash(plumbing.BlobObject, data),
		mode: mode,
		data: func() ([]byte, error) { return data, nil },
	}, true, nil
}

type change struct {
	path     string
	from, to *entry // nil for an added or deleted file
}

// changes compares two sides, keeping paths selected by specs.
func changes(a, b side, specs []string) []change {
	var out []change
	seen := map[string]bool{}
	for _, s := range []side{a, b} {
		for p := range s {
			if seen[p] || !matchAny(specs, p) {
				continue
			}
			seen[p] = true
			ea, oka := a[p]
			eb, okb := b[p]
			if oka && okb && ea.hash == eb.hash && ea.mode == eb.mode {
				continue
			}
			c := change{path: p}
			if oka {
				c.from = &ea
			}
			if okb {
				c.to = &eb
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// fileDiff holds the computed content difference for one change.
type fileDiff struct {
	change
	binary           bool
	oldData, newData []byte
	ops              []lineOp
	added, deleted   int
}

func computeDiff(c change) (*fileDiff, error) {
	d := &fileDiff{change: c}
	var err error
	if c.from != nil {
		if d.oldData, err = c.from.data(); err != nil {
			return nil, err
		}
	}
	if c.to != nil {
		if d.newData, err = c.to.data(); err != nil {
			return nil, err
		}
	}
	if isBinary(d.oldData) || isBinary(d.newData) {
		d.binary = true
		return d, nil
	}
	d.ops = diffLines(splitLines(d.oldData), splitLines(d.newData))
	for _, op := range d.ops {
		switch op.kind {
		case '+':
			d.added++
		case '-':
			d.deleted++
		}
	}
	return d, nil
}

func isBinary(data []byte) bool {
	if len(data) > 8000 {
		data = data[:8000]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// splitLines keeps each line's trailing newline, so a missing final newline
// is a content difference as in git.
func splitLines(data []byte) []string {
	var lines []string
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			lines = append(lines, string(data))
			break
		}
		lines = append(lines, string(data[:i+1]))
		data = data[i+1:]
	}
	return lines
}

type lineOp struct {
	kind byte // ' ', '-', '+'
	line string
	a, b int // 0-based line numbers in old and new
}

// diffLines computes a Myers diff, then slides each change group as far down
// as possible, like git's xdl_change_compact without the indent heuristic.
func diffLines(a, b []string) []lineOp {
	delA, addB := myers(a, b)
	compact(a, delA)
	compact(b, addB)
	var ops []lineOp
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && delA[i]:
			ops = append(ops, lineOp{'-', a[i], i, j})
			i++
		case j < len(b) && addB[j]:
			ops = append(ops, lineOp{'+', b[j], i, j})
			j++
		default:
			ops = append(ops, lineOp{' ', a[i], i, j})
			i++
			j++
		}
	}
	return ops
}

// myers returns which lines of a are deleted and which of b are added.
func myers(a, b []string) (delA, addB []bool) {
	n, m := len(a), len(b)
	delA, addB = make([]bool, n), make([]bool, m)
	max := n + m
	offset := max + 1
	v := make([]int, 2*max+3)
	var trace [][]int
	for d := 0; d <= max; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				// Walk the trace backwards; trace[d] holds v before step d.
				for d2 := d; d2 > 0; d2-- {
					pv := trace[d2]
					k := x - y
					pk := k - 1
					if k == -d2 || (k != d2 && pv[offset+k-1] < pv[offset+k+1]) {
						pk = k + 1
					}
					px := pv[offset+pk]
					py := px - pk
					if pk == k+1 {
						addB[py] = true // down move: b[py] inserted
					} else {
						delA[px] = true // right move: a[px] deleted
					}
					x, y = px, py
				}
				return delA, addB
			}
		}
	}
	return delA, addB
}

func compact(lines []string, changed []bool) {
	for start := 0; start < len(changed); {
		if !changed[start] {
			start++
			continue
		}
		end := start
		for end < len(changed) && changed[end] {
			end++
		}
		for end < len(changed) && lines[start] == lines[end] {
			changed[start], changed[end] = false, true
			start++
			end++
			for end < len(changed) && changed[end] {
				end++
			}
		}
		start = end
	}
}

// Output formats

func mode6(m filemode.FileMode) string { return fmt.Sprintf("%06o", uint32(m)) }

func (d *fileDiff) writePatch(w io.Writer) {
	p := d.path
	fmt.Fprintf(w, "diff --git a/%s b/%s\n", p, p)
	from, to := d.from, d.to
	switch {
	case from == nil:
		fmt.Fprintf(w, "new file mode %s\nindex 0000000..%s\n", mode6(to.mode), short(to.hash))
	case to == nil:
		fmt.Fprintf(w, "deleted file mode %s\nindex %s..0000000\n", mode6(from.mode), short(from.hash))
	default:
		if from.mode != to.mode {
			fmt.Fprintf(w, "old mode %s\nnew mode %s\n", mode6(from.mode), mode6(to.mode))
		}
		if from.hash != to.hash {
			fmt.Fprintf(w, "index %s..%s", short(from.hash), short(to.hash))
			if from.mode == to.mode {
				fmt.Fprintf(w, " %s", mode6(from.mode))
			}
			fmt.Fprintln(w)
		}
	}
	oldName, newName := "a/"+p, "b/"+p
	if from == nil {
		oldName = "/dev/null"
	}
	if to == nil {
		newName = "/dev/null"
	}
	if d.binary {
		fmt.Fprintf(w, "Binary files %s and %s differ\n", oldName, newName)
		return
	}
	if len(d.ops) == 0 || (d.added == 0 && d.deleted == 0) {
		return
	}
	fmt.Fprintf(w, "--- %s\n+++ %s\n", oldName, newName)
	oldLines := splitLines(d.oldData)
	for _, h := range hunks(d.ops, 3) {
		ops := d.ops[h[0]:h[1]]
		aStart, bStart := ops[0].a, ops[0].b
		var aCount, bCount int
		for _, op := range ops {
			if op.kind != '+' {
				aCount++
			}
			if op.kind != '-' {
				bCount++
			}
		}
		fmt.Fprintf(w, "@@ -%s +%s @@", hunkRange(aStart, aCount), hunkRange(bStart, bCount))
		if fn := funcName(oldLines, aStart); fn != "" {
			fmt.Fprintf(w, " %s", fn)
		}
		fmt.Fprintln(w)
		for _, op := range ops {
			fmt.Fprintf(w, "%c%s", op.kind, op.line)
			if !strings.HasSuffix(op.line, "\n") {
				fmt.Fprint(w, "\n\\ No newline at end of file\n")
			}
		}
	}
}

func hunkRange(start, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}

// hunks groups ops into [start, end) ranges with ctx lines of context,
// merging hunks whose context would overlap or touch.
func hunks(ops []lineOp, ctx int) [][2]int {
	var out [][2]int
	for i := 0; i < len(ops); i++ {
		if ops[i].kind == ' ' {
			continue
		}
		start := max(i-ctx, 0)
		end := i + 1
		for j := i + 1; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				end = j + 1
			} else if j-end >= 2*ctx {
				break
			}
		}
		end = min(end+ctx, len(ops))
		if n := len(out); n > 0 && out[n-1][1] >= start {
			out[n-1][1] = end
		} else {
			out = append(out, [2]int{start, end})
		}
		i = end - 1
	}
	return out
}

// funcName mimics git's default hunk-header function line: the nearest line
// before the hunk that starts with a letter, '_' or '$'.
func funcName(lines []string, before int) string {
	for i := before - 1; i >= 0; i-- {
		l := lines[i]
		if l == "" {
			continue
		}
		if c := l[0]; c == '_' || c == '$' || (c|0x20 >= 'a' && c|0x20 <= 'z') {
			if len(l) > 80 {
				l = l[:80]
			}
			return strings.TrimRight(l, " \t\r\n")
		}
	}
	return ""
}

// writeStat prints git's --stat block with the 80-column default width.
func writeStat(w io.Writer, diffs []*fileDiff) {
	if len(diffs) == 0 {
		return
	}
	maxName, maxChange, hasBinary := 0, 0, false
	for _, d := range diffs {
		maxName = max(maxName, len(d.path))
		if d.binary {
			hasBinary = true
		} else {
			maxChange = max(maxChange, d.added+d.deleted)
		}
	}
	numberWidth := len(fmt.Sprint(maxChange))
	if hasBinary {
		numberWidth = max(numberWidth, 3)
	}
	width := 80
	graphWidth, nameWidth := maxChange, maxName
	if nameWidth+numberWidth+6+graphWidth > width {
		if graphWidth > width*3/8-numberWidth-6 {
			graphWidth = max(width*3/8-numberWidth-6, 6)
		}
		if nameWidth > width-numberWidth-6-graphWidth {
			nameWidth = width - numberWidth - 6 - graphWidth
		} else {
			graphWidth = width - numberWidth - 6 - nameWidth
		}
	}
	scale := func(n int) int {
		if n == 0 || maxChange <= graphWidth {
			return n
		}
		return 1 + n*(graphWidth-1)/maxChange
	}
	for _, d := range diffs {
		name := d.path
		if len(name) > nameWidth {
			name = "..." + name[len(name)-nameWidth+3:]
		}
		if d.binary {
			fmt.Fprintf(w, " %-*s | %*s", nameWidth, name, numberWidth, "Bin")
			if len(d.oldData) == 0 && len(d.newData) == 0 {
				fmt.Fprintln(w)
			} else {
				fmt.Fprintf(w, " %d -> %d bytes\n", len(d.oldData), len(d.newData))
			}
			continue
		}
		total := d.added + d.deleted
		fmt.Fprintf(w, " %-*s | %*d", nameWidth, name, numberWidth, total)
		if total > 0 {
			add, del := scale(d.added), scale(d.deleted)
			fmt.Fprintf(w, " %s%s", strings.Repeat("+", add), strings.Repeat("-", del))
		}
		fmt.Fprintln(w)
	}
	writeSummary(w, diffs)
}

func writeSummary(w io.Writer, diffs []*fileDiff) {
	ins, del := 0, 0
	for _, d := range diffs {
		ins += d.added
		del += d.deleted
	}
	plural := func(n int, s string) string {
		if n == 1 {
			return s
		}
		return s + "s"
	}
	fmt.Fprintf(w, " %d %s changed", len(diffs), plural(len(diffs), "file"))
	if ins > 0 || del == 0 {
		fmt.Fprintf(w, ", %d %s(+)", ins, plural(ins, "insertion"))
	}
	if del > 0 || ins == 0 {
		fmt.Fprintf(w, ", %d %s(-)", del, plural(del, "deletion"))
	}
	fmt.Fprintln(w)
}

// writeModeSummary prints the create/delete/mode lines after a summary.
func writeModeSummary(w io.Writer, diffs []*fileDiff) {
	for _, d := range diffs {
		switch {
		case d.from == nil:
			fmt.Fprintf(w, " create mode %s %s\n", mode6(d.to.mode), d.path)
		case d.to == nil:
			fmt.Fprintf(w, " delete mode %s %s\n", mode6(d.from.mode), d.path)
		case d.from.mode != d.to.mode:
			fmt.Fprintf(w, " mode change %s => %s %s\n", mode6(d.from.mode), mode6(d.to.mode), d.path)
		}
	}
}

func computeDiffs(cs []change) ([]*fileDiff, error) {
	out := make([]*fileDiff, 0, len(cs))
	for _, c := range cs {
		d, err := computeDiff(c)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// commitDiffs compares a commit with its first parent (or the empty tree).
func (r *repo) commitDiffs(c *object.Commit, specs []string) ([]*fileDiff, error) {
	var parent *object.Commit
	if c.NumParents() > 0 {
		p, err := c.Parent(0)
		if err != nil {
			return nil, err
		}
		parent = p
	}
	a, err := r.treeSide(parent)
	if err != nil {
		return nil, err
	}
	b, err := r.treeSide(c)
	if err != nil {
		return nil, err
	}
	return computeDiffs(changes(a, b, specs))
}
