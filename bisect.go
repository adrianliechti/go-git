package git

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// git bisect, with git's state files (BISECT_START, BISECT_TERMS,
// BISECT_LOG, refs/bisect/*) and its choice of the next commit to test.

type bisectTerms struct{ bad, good string }

func (r *repo) bisectTerms() bisectTerms {
	s, ok := r.gitFile("BISECT_TERMS")
	if !ok {
		return bisectTerms{"bad", "good"}
	}
	f := strings.Fields(s)
	if len(f) != 2 {
		return bisectTerms{"bad", "good"}
	}
	return bisectTerms{f[0], f[1]}
}

func (r *repo) bisectLog(lines ...string) {
	old, _ := r.gitFile("BISECT_LOG")
	r.writeGitFile("BISECT_LOG", old+strings.Join(lines, "\n")+"\n")
}

func (g *gitRun) bisect(args []string) error {
	if len(args) == 0 {
		return &exitError{code: 129, msg: commandUsage("bisect")}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	sub, args := args[0], args[1:]
	if sub == "start" {
		return r.bisectStart(args)
	}
	if _, ok := r.gitFile("BISECT_START"); !ok {
		if sub == "reset" {
			return nil
		}
		return failf(1, "You need to start by \"git bisect start\"\n\n")
	}
	terms := r.bisectTerms()
	switch {
	case sub == "reset":
		return r.bisectReset(args)
	case sub == "log":
		log, _ := r.gitFile("BISECT_LOG")
		fmt.Fprint(g.out, log)
		return nil
	case sub == "terms":
		fmt.Fprintf(g.out, "Your current terms are %s for the old state\nand %s for the new state.\n", terms.good, terms.bad)
		return nil
	case sub == "visualize" || sub == "view":
		return g.log(append([]string{"--oneline", "refs/bisect/" + terms.bad, "--not"}, r.bisectGoodNames()...))
	case sub == "run":
		return fatalf("git bisect run needs to execute commands, which is not possible in this sandbox")
	case sub == "skip":
		revs := args
		if len(revs) == 0 {
			revs = []string{"HEAD"}
		}
		for _, rev := range revs {
			c, err := r.resolveCommit(rev)
			if err != nil {
				return err
			}
			r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName("refs/bisect/skip-"+c.Hash.String()), c.Hash))
			r.bisectLog("# skip: ["+c.Hash.String()+"] "+subject(c.Message), "git bisect skip "+c.Hash.String())
		}
		return r.bisectNext()
	case sub == terms.bad || sub == "bad" && terms.bad == "bad" || sub == "new" && terms.bad == "bad":
		rev := "HEAD"
		if len(args) > 0 {
			rev = args[0]
		}
		c, err := r.resolveCommit(rev)
		if err != nil {
			return err
		}
		r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName("refs/bisect/"+terms.bad), c.Hash))
		r.bisectLog("# "+terms.bad+": ["+c.Hash.String()+"] "+subject(c.Message), "git bisect "+terms.bad+" "+c.Hash.String())
		return r.bisectNext()
	case sub == terms.good || sub == "good" && terms.good == "good" || sub == "old" && terms.good == "good":
		revs := args
		if len(revs) == 0 {
			revs = []string{"HEAD"}
		}
		for _, rev := range revs {
			c, err := r.resolveCommit(rev)
			if err != nil {
				return err
			}
			r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName("refs/bisect/"+terms.good+"-"+c.Hash.String()), c.Hash))
			r.bisectLog("# "+terms.good+": ["+c.Hash.String()+"] "+subject(c.Message), "git bisect "+terms.good+" "+c.Hash.String())
		}
		return r.bisectNext()
	}
	return &exitError{code: 1, msg: fmt.Sprintf("error: unknown command: '%s'\n", sub) + commandUsage("bisect")}
}

func (r *repo) bisectStart(args []string) error {
	g := r.g
	if _, ok := r.gitFile("BISECT_START"); ok {
		if err := r.bisectReset(nil); err != nil {
			return err
		}
	}
	terms := bisectTerms{"bad", "good"}
	var revs []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--term-new=") || strings.HasPrefix(a, "--term-bad="):
			terms.bad = a[strings.Index(a, "=")+1:]
		case strings.HasPrefix(a, "--term-old=") || strings.HasPrefix(a, "--term-good="):
			terms.good = a[strings.Index(a, "=")+1:]
		case a == "--no-checkout" || a == "--first-parent" || a == "--":
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			revs = append(revs, a)
		}
	}
	start := r.headName()
	r.writeGitFile("BISECT_START", start+"\n")
	r.writeGitFile("BISECT_TERMS", terms.bad+"\n"+terms.good+"\n")
	r.writeGitFile("BISECT_LOG", "")
	startLine := "git bisect start"
	for _, rev := range revs {
		startLine += " '" + rev + "'"
	}
	r.bisectLog(startLine)
	for i, rev := range revs {
		c, err := r.resolveCommit(rev)
		if err != nil {
			return err
		}
		term := terms.good
		name := "refs/bisect/" + terms.good + "-" + c.Hash.String()
		if i == 0 {
			term, name = terms.bad, "refs/bisect/"+terms.bad
		}
		r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(name), c.Hash))
		r.bisectLog("# "+term+": ["+c.Hash.String()+"] "+subject(c.Message), "git bisect "+term+" "+c.Hash.String())
	}
	_ = g
	return r.bisectNext()
}

func (r *repo) bisectGoodNames() []string {
	terms := r.bisectTerms()
	var out []string
	for _, name := range r.sortedRefs() {
		if strings.HasPrefix(name.String(), "refs/bisect/"+terms.good+"-") {
			out = append(out, name.String())
		}
	}
	return out
}

// bisectNext reports the state or checks out the next commit to test.
func (r *repo) bisectNext() error {
	g := r.g
	terms := r.bisectTerms()
	bad := r.refHash(plumbing.ReferenceName("refs/bisect/" + terms.bad))
	goods := r.bisectGoodNames()
	switch {
	case bad.IsZero() && len(goods) == 0:
		// git words the status with good/bad even for custom terms.
		r.bisectStatus("status: waiting for both good and bad commits")
		return nil
	case bad.IsZero():
		n := "1 good commit"
		if len(goods) > 1 {
			n = fmt.Sprintf("%d good commits", len(goods))
		}
		r.bisectStatus("status: waiting for bad commit, " + n + " known")
		return nil
	case len(goods) == 0:
		r.bisectStatus("status: waiting for good commit(s), bad commit known")
		return nil
	}
	// Candidates: reachable from bad, not from any good commit.
	excluded := map[plumbing.Hash]bool{}
	for _, gname := range goods {
		for h := range r.ancestors(r.refHash(plumbing.ReferenceName(gname))) {
			excluded[h] = true
		}
	}
	f := newLogFormat()
	f.revs = []string{bad.String()}
	for _, gname := range goods {
		f.revs = append(f.revs, "^"+gname)
	}
	commits, err := r.walk(f, nil)
	if err != nil {
		return err
	}
	all := len(commits)
	inSet := map[plumbing.Hash]bool{}
	for _, c := range commits {
		inSet[c.Hash] = true
	}
	weight := func(c *object.Commit) int {
		n := 0
		for h := range r.ancestors(c.Hash) {
			if inSet[h] {
				n++
			}
		}
		return n
	}
	skipped := map[plumbing.Hash]bool{}
	for _, name := range r.sortedRefs() {
		if strings.HasPrefix(name.String(), "refs/bisect/skip-") {
			skipped[r.refHash(name)] = true
		}
	}
	if all == 1 {
		return r.bisectFound(commits[0])
	}
	// The best commit halves the candidates; ties go to the oldest.
	type scored struct {
		c        *object.Commit
		weight   int
		distance int
	}
	var list []scored
	for i := len(commits) - 1; i >= 0; i-- {
		w := weight(commits[i])
		d := min(w, all-w)
		list = append(list, scored{commits[i], w, d})
	}
	best := -1
	for i, s := range list {
		if best < 0 || s.distance > list[best].distance {
			best = i
		}
	}
	reaches := list[best].weight
	pick := list[best].c
	if skipped[pick.Hash] {
		// Take the next-best commit that is not skipped.
		order := make([]int, len(list))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return list[order[a]].distance > list[order[b]].distance })
		pick = nil
		for _, i := range order {
			if !skipped[list[i].c.Hash] && list[i].c.Hash != bad {
				pick, reaches = list[i].c, list[i].weight
				break
			}
		}
		if pick == nil {
			var b strings.Builder
			b.WriteString("There are only 'skip'ped commits left to test.\nThe first " + terms.bad + " commit could be any of:\n")
			for _, c := range commits {
				b.WriteString(c.Hash.String() + "\n")
			}
			b.WriteString("We cannot bisect more!\n")
			return failf(2, "%s", b.String())
		}
	}
	nr := all - reaches - 1
	steps := estimateBisectSteps(all)
	revWord, stepWord := "revisions", "steps"
	if nr == 1 {
		revWord = "revision"
	}
	if steps == 1 {
		stepWord = "step"
	}
	if err := r.switchTo(pick, "checkout", false); err != nil {
		return err
	}
	if err := r.pointHead("", pick.Hash, "checkout: moving from "+r.headName()+" to "+pick.Hash.String()); err != nil {
		return err
	}
	fmt.Fprintf(g.out, "Bisecting: %d %s left to test after this (roughly %d %s)\n[%s] %s\n",
		nr, revWord, steps, stepWord, pick.Hash, subject(pick.Message))
	return nil
}

func (r *repo) bisectStatus(msg string) {
	fmt.Fprintln(r.g.out, msg)
	r.bisectLog("# " + msg)
}

func estimateBisectSteps(all int) int {
	if all < 3 {
		return 0
	}
	n := 0
	for 1<<(n+1) <= all {
		n++
	}
	e := 1 << n
	x := all - e
	if e < 3*x {
		return n
	}
	return n - 1
}

func (r *repo) bisectFound(c *object.Commit) error {
	g := r.g
	terms := r.bisectTerms()
	fmt.Fprintf(g.out, "%s is the first %s commit\n", c.Hash, terms.bad)
	f := newLogFormat()
	f.stat, f.summary = true, true
	if err := r.writeCommit(g.out, c, f, nil, 0); err != nil {
		return err
	}
	r.bisectLog("# first " + terms.bad + " commit: [" + c.Hash.String() + "] " + subject(c.Message))
	return nil
}

func (r *repo) bisectReset(args []string) error {
	g := r.g
	start, _ := r.gitFile("BISECT_START")
	start = strings.TrimSpace(start)
	target := start
	if len(args) > 0 {
		target = args[0]
	}
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	c, err := r.resolveCommit(target)
	if err != nil {
		return err
	}
	if head != nil && head.Hash != c.Hash {
		fmt.Fprintf(g.err, "Previous HEAD position was %s %s\n", short(head.Hash), subject(head.Message))
	}
	if err := r.switchTo(c, "checkout", false); err != nil {
		return err
	}
	msg := "checkout: moving from " + r.headName() + " to " + target
	if r.isBranch(target) {
		if err := r.pointHead(plumbing.NewBranchReferenceName(target), plumbing.ZeroHash, msg); err != nil {
			return err
		}
		fmt.Fprintf(g.err, "Switched to branch '%s'\n", target)
	} else {
		if err := r.pointHead("", c.Hash, msg); err != nil {
			return err
		}
		fmt.Fprintf(g.err, "HEAD is now at %s %s\n", short(c.Hash), subject(c.Message))
	}
	r.removeRefsWithPrefix("refs/bisect/")
	for _, name := range []string{"BISECT_START", "BISECT_TERMS", "BISECT_LOG", "BISECT_EXPECTED_REV", "BISECT_ANCESTORS_OK", "BISECT_NAMES", "BISECT_RUN"} {
		if err := g.fsys.Remove(fsName(path.Join(r.gitDir, name))); err != nil && !isNotExist(err) {
			return err
		}
	}
	return nil
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
