package git

import (
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// A port of git's graph.c, which draws the history lines of log --graph.
// Colors are omitted; everything else follows git's state machine.

type graphState int

const (
	graphPadding graphState = iota
	graphSkip
	graphPreCommit
	graphCommit
	graphPostMerge
	graphCollapsing
)

type logGraph struct {
	commit          *object.Commit
	parents         []plumbing.Hash // interesting parents of commit
	width           int
	expansionRow    int
	state           graphState
	prevState       graphState
	commitIndex     int
	prevCommitIndex int
	mergeLayout     int
	edgesAdded      int
	prevEdgesAdded  int
	columns         []plumbing.Hash
	newColumns      []plumbing.Hash
	mapping         []int
	oldMapping      []int
	mappingSize     int
	interesting     func(plumbing.Hash) bool
}

func newLogGraph(interesting func(plumbing.Hash) bool) *logGraph {
	return &logGraph{state: graphPadding, prevState: graphPadding, interesting: interesting}
}

func (g *logGraph) setState(s graphState) {
	g.prevState = g.state
	g.state = s
}

// update prepares the graph for the next commit, like graph_update.
func (g *logGraph) update(c *object.Commit) {
	g.commit = c
	g.parents = g.parents[:0]
	seen := map[plumbing.Hash]bool{}
	for _, p := range c.ParentHashes {
		if g.interesting(p) && !seen[p] {
			seen[p] = true
			g.parents = append(g.parents, p)
		}
	}
	g.prevCommitIndex = g.commitIndex
	g.updateColumns()
	g.expansionRow = 0
	switch {
	case g.state != graphPadding:
		g.state = graphSkip
	case g.needsPreCommitLine():
		g.state = graphPreCommit
	default:
		g.state = graphCommit
	}
}

func (g *logGraph) numExpansionRows() int { return (len(g.parents) - 2) * 2 }

func (g *logGraph) needsPreCommitLine() bool {
	return len(g.parents) >= 3 && g.commitIndex < len(g.columns)-1 && g.expansionRow < g.numExpansionRows()
}

func (g *logGraph) findNewColumn(h plumbing.Hash) int {
	for i, c := range g.newColumns {
		if c == h {
			return i
		}
	}
	return -1
}

func (g *logGraph) ensureMapping(n int) {
	for len(g.mapping) < n {
		g.mapping = append(g.mapping, -1)
	}
	for len(g.oldMapping) < n {
		g.oldMapping = append(g.oldMapping, -1)
	}
}

func (g *logGraph) updateColumns() {
	g.columns, g.newColumns = g.newColumns, g.columns[:0]
	maxNew := len(g.columns) + len(g.parents)
	g.mappingSize = 2 * maxNew
	g.ensureMapping(g.mappingSize)
	for i := 0; i < g.mappingSize; i++ {
		g.mapping[i] = -1
	}
	g.width = 0
	g.prevEdgesAdded = g.edgesAdded
	g.edgesAdded = 0
	seenThis := false
	inColumns := true
	for i := 0; i <= len(g.columns); i++ {
		var col plumbing.Hash
		if i == len(g.columns) {
			if seenThis {
				break
			}
			inColumns = false
			col = g.commit.Hash
		} else {
			col = g.columns[i]
		}
		if col == g.commit.Hash {
			seenThis = true
			g.commitIndex = i
			g.mergeLayout = -1
			for _, p := range g.parents {
				_ = inColumns // colors only
				g.insertIntoNewColumns(p, i)
			}
			if len(g.parents) == 0 {
				g.width += 2
			}
		} else {
			g.insertIntoNewColumns(col, -1)
		}
	}
	for g.mappingSize > 1 && g.mapping[g.mappingSize-1] < 0 {
		g.mappingSize--
	}
}

func (g *logGraph) insertIntoNewColumns(h plumbing.Hash, idx int) {
	i := g.findNewColumn(h)
	if i < 0 {
		i = len(g.newColumns)
		g.newColumns = append(g.newColumns, h)
	}
	var mappingIdx int
	switch {
	case len(g.parents) > 1 && idx > -1 && g.mergeLayout == -1:
		dist := idx - i
		shift := 1
		if dist > 1 {
			shift = 2*dist - 3
		}
		g.mergeLayout = 0
		if dist <= 0 {
			g.mergeLayout = 1
		}
		g.edgesAdded = len(g.parents) + g.mergeLayout - 2
		mappingIdx = g.width + (g.mergeLayout-1)*shift
		g.width += 2 * g.mergeLayout
	case g.edgesAdded > 0 && g.width >= 2 && i == g.mapping[g.width-2]:
		mappingIdx = g.width - 2
		g.edgesAdded = -1
	default:
		mappingIdx = g.width
		g.width += 2
	}
	g.ensureMapping(mappingIdx + 1)
	g.mapping[mappingIdx] = i
}

func (g *logGraph) isMappingCorrect() bool {
	for i := 0; i < g.mappingSize; i++ {
		if t := g.mapping[i]; t >= 0 && t != i/2 {
			return false
		}
	}
	return true
}

// nextLine returns the next graph row and whether it was the commit row.
func (g *logGraph) nextLine() (string, bool) {
	var b strings.Builder
	commitRow := false
	switch g.state {
	case graphPadding:
		for range g.newColumns {
			b.WriteString("| ")
		}
	case graphSkip:
		b.WriteString("...")
		if g.needsPreCommitLine() {
			g.setState(graphPreCommit)
		} else {
			g.setState(graphCommit)
		}
	case graphPreCommit:
		g.preCommitLine(&b)
	case graphCommit:
		g.commitLine(&b)
		commitRow = true
	case graphPostMerge:
		g.postMergeLine(&b)
	case graphCollapsing:
		g.collapsingLine(&b)
	}
	return g.pad(b.String()), commitRow
}

func (g *logGraph) pad(s string) string {
	if n := g.width - len(s); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

func (g *logGraph) preCommitLine(b *strings.Builder) {
	seenThis := false
	for i, col := range g.columns {
		switch {
		case col == g.commit.Hash:
			seenThis = true
			b.WriteString("|" + strings.Repeat(" ", g.expansionRow))
		case seenThis && g.expansionRow == 0:
			if g.prevState == graphPostMerge && g.prevCommitIndex < i {
				b.WriteString("\\")
			} else {
				b.WriteString("|")
			}
		case seenThis && g.expansionRow > 0:
			b.WriteString("\\")
		default:
			b.WriteString("|")
		}
		b.WriteString(" ")
	}
	g.expansionRow++
	if !g.needsPreCommitLine() {
		g.setState(graphCommit)
	}
}

func (g *logGraph) commitLine(b *strings.Builder) {
	seenThis := false
	for i := 0; i <= len(g.columns); i++ {
		var col plumbing.Hash
		if i == len(g.columns) {
			if seenThis {
				break
			}
			col = g.commit.Hash
		} else {
			col = g.columns[i]
		}
		switch {
		case col == g.commit.Hash:
			seenThis = true
			b.WriteString("*")
			if n := len(g.parents); n > 2 {
				for k := 0; k < n-2; k++ {
					b.WriteString("-")
					if k == n-3 {
						b.WriteString(".")
					} else {
						b.WriteString("-")
					}
				}
			}
		case seenThis && g.edgesAdded > 1:
			b.WriteString("\\")
		case seenThis && g.edgesAdded == 1:
			if g.prevState == graphPostMerge && g.prevEdgesAdded > 0 && g.prevCommitIndex < i {
				b.WriteString("\\")
			} else {
				b.WriteString("|")
			}
		case g.prevState == graphCollapsing && 2*i+1 < len(g.oldMapping) && g.oldMapping[2*i+1] == i && g.mapping[2*i] < i:
			b.WriteString("/")
		default:
			b.WriteString("|")
		}
		b.WriteString(" ")
	}
	switch {
	case len(g.parents) > 1:
		g.setState(graphPostMerge)
	case g.isMappingCorrect():
		g.setState(graphPadding)
	default:
		g.setState(graphCollapsing)
	}
}

func (g *logGraph) postMergeLine(b *strings.Builder) {
	mergeChars := []string{"/", "|", "\\"}
	seenThis := false
	parentCol := false
	for i := 0; i <= len(g.columns); i++ {
		var col plumbing.Hash
		if i == len(g.columns) {
			if seenThis {
				break
			}
			col = g.commit.Hash
		} else {
			col = g.columns[i]
		}
		switch {
		case col == g.commit.Hash:
			seenThis = true
			idx := g.mergeLayout
			for j := range g.parents {
				b.WriteString(mergeChars[idx])
				if idx == 2 {
					if g.edgesAdded > 0 || j < len(g.parents)-1 {
						b.WriteString(" ")
					}
				} else {
					idx++
				}
			}
			if g.edgesAdded == 0 {
				b.WriteString(" ")
			}
		case seenThis:
			if g.edgesAdded > 0 {
				b.WriteString("\\")
			} else {
				b.WriteString("|")
			}
			b.WriteString(" ")
		default:
			b.WriteString("|")
			if g.mergeLayout != 0 || i != g.commitIndex-1 {
				if parentCol {
					b.WriteString("_")
				} else {
					b.WriteString(" ")
				}
			}
		}
		if len(g.parents) > 0 && col == g.parents[0] {
			parentCol = true
		}
	}
	if g.isMappingCorrect() {
		g.setState(graphPadding)
	} else {
		g.setState(graphCollapsing)
	}
}

func (g *logGraph) collapsingLine(b *strings.Builder) {
	usedHorizontal := false
	horizontalEdge, horizontalTarget := -1, -1
	g.mapping, g.oldMapping = g.oldMapping, g.mapping
	g.ensureMapping(g.mappingSize)
	for i := 0; i < g.mappingSize; i++ {
		g.mapping[i] = -1
	}
	for i := 0; i < g.mappingSize; i++ {
		target := g.oldMapping[i]
		if target < 0 {
			continue
		}
		switch {
		case target*2 == i:
			g.mapping[i] = target
		case g.mapping[i-1] < 0:
			g.mapping[i-1] = target
			if horizontalEdge == -1 {
				horizontalEdge, horizontalTarget = i, target
				for j := target*2 + 3; j < i-2; j += 2 {
					g.mapping[j] = target
				}
			}
		case g.mapping[i-1] == target:
		default:
			g.mapping[i-2] = target
			if horizontalEdge == -1 {
				horizontalTarget, horizontalEdge = target, i-1
				for j := target*2 + 3; j < i-2; j += 2 {
					g.mapping[j] = target
				}
			}
		}
	}
	copy(g.oldMapping, g.mapping[:g.mappingSize])
	if g.mapping[g.mappingSize-1] < 0 {
		g.mappingSize--
	}
	for i := 0; i < g.mappingSize; i++ {
		target := g.mapping[i]
		switch {
		case target < 0:
			b.WriteString(" ")
		case target*2 == i:
			b.WriteString("|")
		case target == horizontalTarget && i != horizontalEdge-1:
			if i != target*2+3 {
				g.mapping[i] = -1
			}
			usedHorizontal = true
			b.WriteString("_")
		default:
			if usedHorizontal && i < horizontalEdge {
				g.mapping[i] = -1
			}
			b.WriteString("/")
		}
	}
	if g.isMappingCorrect() {
		g.setState(graphPadding)
	}
}

// paddingLine is the prefix for lines that belong to the current commit
// without moving the graph, like graph_padding_line.
func (g *logGraph) paddingLine() string {
	if g.state != graphCommit {
		s, _ := g.nextLine()
		return s
	}
	var b strings.Builder
	for _, col := range g.columns {
		b.WriteString("|")
		if col == g.commit.Hash && len(g.parents) > 2 {
			b.WriteString(strings.Repeat(" ", (len(g.parents)-2)*2))
		} else {
			b.WriteString(" ")
		}
	}
	g.prevState = graphPadding
	return g.pad(b.String())
}

func (g *logGraph) finished() bool { return g.state == graphPadding }

// showCommit writes rows up to and including the commit row's prefix.
func (g *logGraph) showCommit(b *strings.Builder) {
	if g.finished() {
		b.WriteString(g.paddingLine())
		return
	}
	for {
		s, commitRow := g.nextLine()
		b.WriteString(s)
		if commitRow || g.finished() {
			return
		}
		b.WriteString("\n")
	}
}

// remainder writes the rows left for the current commit, newline-separated.
func (g *logGraph) remainder(b *strings.Builder) bool {
	shown := false
	for !g.finished() {
		s, _ := g.nextLine()
		b.WriteString(s)
		shown = true
		if !g.finished() {
			b.WriteString("\n")
		}
	}
	return shown
}

// topoOrder sorts commits like git's --topo-order in graph order: children
// before parents, keeping the walk's order for independent tips.
func topoOrder(commits []*object.Commit) []*object.Commit {
	indegree := map[plumbing.Hash]int{}
	for _, c := range commits {
		indegree[c.Hash] = 1
	}
	for _, c := range commits {
		for _, p := range c.ParentHashes {
			if indegree[p] > 0 {
				indegree[p]++
			}
		}
	}
	byHash := map[plumbing.Hash]*object.Commit{}
	for _, c := range commits {
		byHash[c.Hash] = c
	}
	var stack []*object.Commit
	for i := len(commits) - 1; i >= 0; i-- {
		if indegree[commits[i].Hash] == 1 {
			stack = append(stack, commits[i])
		}
	}
	var out []*object.Commit
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range c.ParentHashes {
			if indegree[p] == 0 {
				continue
			}
			indegree[p]--
			if indegree[p] == 1 {
				stack = append(stack, byHash[p])
			}
		}
		indegree[c.Hash] = 0
		out = append(out, c)
	}
	return out
}
