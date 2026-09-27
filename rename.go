package git

import (
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// Rename detection follows git's diffcore-rename: exact renames first, then
// inexact pairs scored with diffcore-delta's span hashing. Scores use git's
// scale where maxScore is 100%; the default threshold is 50%.
const (
	maxScore     = 60000
	minimumScore = 30000
	hashBase     = 107927
)

// detectRenames pairs deleted and added files in cs. The result is sorted
// by the new path, like git's diff queue.
func detectRenames(cs []change) ([]change, error) {
	var srcs, dsts []int
	for i, c := range cs {
		switch {
		case c.to == nil && isRegular(c.from.mode):
			srcs = append(srcs, i)
		case c.from == nil && isRegular(c.to.mode):
			dsts = append(dsts, i)
		}
	}
	if len(srcs) == 0 || len(dsts) == 0 {
		return cs, nil
	}
	usedSrc := map[int]bool{}
	pairs := map[int]int{} // dst index -> src index
	scores := map[int]int{}

	// Exact renames, preferring a source with the same basename.
	for _, d := range dsts {
		best := -1
		for _, s := range srcs {
			if usedSrc[s] || cs[s].from.hash != cs[d].to.hash {
				continue
			}
			if best == -1 || (path.Base(cs[s].path) == path.Base(cs[d].path) && path.Base(cs[best].path) != path.Base(cs[d].path)) {
				best = s
			}
		}
		if best >= 0 {
			usedSrc[best] = true
			pairs[d] = best
			scores[d] = maxScore
		}
	}

	// Inexact renames among the rest.
	type candidate struct{ dst, src, score, nameScore int }
	var cands []candidate
	spans := map[int]map[uint32]int{}
	spanOf := func(i int, e *entry) (map[uint32]int, int, error) {
		data, err := e.data()
		if err != nil {
			return nil, 0, err
		}
		if spans[i] == nil {
			spans[i] = hashChars(data, !isBinary(data))
		}
		return spans[i], len(data), nil
	}
	for _, d := range dsts {
		if _, done := pairs[d]; done {
			continue
		}
		for _, s := range srcs {
			if usedSrc[s] {
				continue
			}
			dstSpans, dstSize, err := spanOf(d, cs[d].to)
			if err != nil {
				return nil, err
			}
			srcSpans, srcSize, err := spanOf(s, cs[s].from)
			if err != nil {
				return nil, err
			}
			score := similarity(srcSpans, dstSpans, srcSize, dstSize)
			if score < minimumScore {
				continue
			}
			nameScore := 0
			if path.Base(cs[s].path) == path.Base(cs[d].path) {
				nameScore = 1
			}
			cands = append(cands, candidate{d, s, score, nameScore})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].nameScore > cands[j].nameScore
	})
	for _, c := range cands {
		if _, done := pairs[c.dst]; done || usedSrc[c.src] {
			continue
		}
		usedSrc[c.src] = true
		pairs[c.dst] = c.src
		scores[c.dst] = c.score
	}

	var out []change
	for i, c := range cs {
		if usedSrc[i] {
			continue
		}
		if s, ok := pairs[i]; ok {
			c.oldPath = cs[s].path
			c.from = cs[s].from
			c.score = scores[i]
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

func isRegular(m filemode.FileMode) bool {
	return m == filemode.Regular || m == filemode.Executable || m == filemode.Deprecated
}

// hashChars splits data into spans ending at a newline or after 64 bytes and
// counts bytes per span hash, as diffcore-delta does.
func hashChars(data []byte, text bool) map[uint32]int {
	out := map[uint32]int{}
	var accum1, accum2 uint32
	n := 0
	for i := 0; i < len(data); i++ {
		c := uint32(data[i])
		old1 := accum1
		if text && c == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			continue
		}
		accum1 = (accum1 << 7) ^ (accum2 >> 25)
		accum2 = (accum2 << 7) ^ (old1 >> 25)
		accum1 += c
		n++
		if n < 64 && c != '\n' {
			continue
		}
		out[(accum1+accum2*0x61)%hashBase] += n
		n, accum1, accum2 = 0, 0, 0
	}
	if n > 0 {
		out[(accum1+accum2*0x61)%hashBase] += n
	}
	return out
}

// similarity returns git's rename score for a source and destination.
func similarity(src, dst map[uint32]int, srcSize, dstSize int) int {
	maxSize, baseSize := max(srcSize, dstSize), min(srcSize, dstSize)
	if maxSize*(maxScore-minimumScore) < (maxSize-baseSize)*maxScore {
		return 0
	}
	if dstSize == 0 {
		return 0
	}
	copied := 0
	for h, sc := range src {
		copied += min(sc, dst[h])
	}
	return int(int64(copied) * maxScore / int64(maxSize))
}

func similarityIndex(score int) int { return score * 100 / maxScore }

// renameName formats "old => new" with git's {prefix/suffix} compaction.
func renameName(a, b string) string {
	if qa, qb := q(a), q(b); qa != a || qb != b {
		return qa + " => " + qb
	}
	// at treats the end of a string as git's NUL terminator.
	at := func(s string, i int) byte {
		if i >= len(s) {
			return 0
		}
		return s[i]
	}
	pfx := 0
	for i := 0; i < len(a) && i < len(b) && a[i] == b[i]; i++ {
		if a[i] == '/' {
			pfx = i + 1
		}
	}
	adjust := 0
	if pfx > 0 {
		adjust = 1
	}
	sfx := 0
	for i, j := len(a), len(b); pfx-adjust <= i && pfx-adjust <= j && at(a, i) == at(b, j); i, j = i-1, j-1 {
		if at(a, i) == '/' {
			sfx = len(a) - i
		}
		if i == 0 || j == 0 {
			break
		}
	}
	aMid, bMid := max(len(a)-pfx-sfx, 0), max(len(b)-pfx-sfx, 0)
	var s strings.Builder
	if pfx+sfx > 0 {
		s.WriteString(a[:pfx] + "{")
	}
	s.WriteString(a[pfx:pfx+aMid] + " => " + b[pfx:pfx+bMid])
	if pfx+sfx > 0 {
		s.WriteString("}" + a[len(a)-sfx:])
	}
	return s.String()
}
