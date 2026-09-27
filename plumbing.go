package git

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Low-level commands from git's plumbing, as used in Pro Git's Internals.

func objectKind(m filemode.FileMode) string {
	switch m {
	case filemode.Dir:
		return "tree"
	case filemode.Submodule:
		return "commit"
	}
	return "blob"
}

func (g *gitRun) lsTree(args []string) error {
	var recursive, showTrees, onlyTrees, long, nameOnly bool
	var rest []string
	for _, a := range args {
		switch a {
		case "-r":
			recursive = true
		case "-t":
			showTrees = true
		case "-d":
			onlyTrees = true
		case "-l", "--long":
			long = true
		case "--name-only", "--name-status":
			nameOnly = true
		case "--full-tree", "--":
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return &exitError{code: 129, msg: "usage: git ls-tree [<options>] <tree-ish> [<path>...]\n"}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	h, err := r.resolveObject(rest[0])
	if err != nil {
		return fatalf("Not a valid object name %s", rest[0])
	}
	if c, err := r.CommitObject(h); err == nil {
		h = c.TreeHash
	}
	root, err := r.TreeObject(h)
	if err != nil {
		return fatalf("not a tree object")
	}
	specs := rest[1:]
	print := func(p string, e object.TreeEntry) error {
		if onlyTrees && e.Mode != filemode.Dir {
			return nil
		}
		switch {
		case nameOnly:
			fmt.Fprintln(g.out, q(p))
		case long:
			size := "-"
			if e.Mode != filemode.Dir && e.Mode != filemode.Submodule {
				obj, err := r.Storer.EncodedObject(plumbing.AnyObject, e.Hash)
				if err != nil {
					return err
				}
				size = strconv.FormatInt(obj.Size(), 10)
			}
			fmt.Fprintf(g.out, "%s %s %s %7s\t%s\n", mode6(e.Mode), objectKind(e.Mode), e.Hash, size, q(p))
		default:
			fmt.Fprintf(g.out, "%s %s %s\t%s\n", mode6(e.Mode), objectKind(e.Mode), e.Hash, q(p))
		}
		return nil
	}
	selected := func(p string, isDir bool) (show, descend bool) {
		if len(specs) == 0 {
			return true, recursive && isDir
		}
		for _, s := range specs {
			s = strings.TrimSuffix(s, "/")
			switch {
			case p == s:
				return true, isDir && (recursive || strings.HasSuffix(specs[0], "/"))
			case strings.HasPrefix(s, p+"/"):
				return false, isDir
			case strings.HasPrefix(p, s+"/"):
				return true, recursive && isDir
			}
		}
		return false, false
	}
	var walk func(t *object.Tree, prefix string) error
	walk = func(t *object.Tree, prefix string) error {
		for _, e := range t.Entries {
			p := prefix + e.Name
			isDir := e.Mode == filemode.Dir
			show, descend := selected(p, isDir)
			if show && (!isDir || !descend || showTrees || onlyTrees) {
				if err := print(p, e); err != nil {
					return err
				}
			}
			if descend {
				sub, err := r.TreeObject(e.Hash)
				if err != nil {
					return err
				}
				if err := walk(sub, p+"/"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root, "")
}

func (g *gitRun) showRef(args []string) error {
	var heads, tags, deref, hashOnly, verify, quiet, withHead bool
	abbrev := 40
	var patterns []string
	for _, a := range args {
		switch {
		case a == "--heads" || a == "--branches":
			heads = true
		case a == "--tags":
			tags = true
		case a == "-d" || a == "--dereference":
			deref = true
		case a == "-s" || a == "--hash":
			hashOnly = true
		case strings.HasPrefix(a, "--hash="):
			hashOnly = true
			abbrev, _ = strconv.Atoi(strings.TrimPrefix(a, "--hash="))
		case a == "--verify":
			verify = true
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--head":
			withHead = true
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			patterns = append(patterns, a)
		}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	show := func(name plumbing.ReferenceName, h plumbing.Hash) {
		if quiet {
			return
		}
		s := h.String()[:min(max(abbrev, 4), 40)]
		if hashOnly {
			fmt.Fprintln(g.out, s)
		} else {
			fmt.Fprintf(g.out, "%s %s\n", s, name)
		}
		if deref {
			if t, err := r.TagObject(h); err == nil {
				if hashOnly {
					fmt.Fprintln(g.out, t.Target)
				} else {
					fmt.Fprintf(g.out, "%s %s^{}\n", t.Target, name)
				}
			}
		}
	}
	if verify {
		for _, p := range patterns {
			ref, err := r.Reference(plumbing.ReferenceName(p), true)
			if err != nil || (!strings.HasPrefix(p, "refs/") && p != "HEAD") {
				if quiet {
					return failf(1, "")
				}
				return fatalf("'%s' - not a valid ref", p)
			}
			show(plumbing.ReferenceName(p), ref.Hash())
		}
		return nil
	}
	found := false
	if withHead {
		if h := r.refHash(plumbing.HEAD); !h.IsZero() {
			show(plumbing.HEAD, h)
			found = true
		}
	}
	for _, name := range r.sortedRefs() {
		switch {
		case heads && tags:
			if !name.IsBranch() && !name.IsTag() {
				continue
			}
		case heads && !name.IsBranch(), tags && !name.IsTag():
			continue
		}
		if len(patterns) > 0 && !refMatchesTail(name, patterns) {
			continue
		}
		show(name, r.refHash(name))
		found = true
	}
	if !found {
		return failf(1, "")
	}
	return nil
}

// refMatchesTail matches show-ref patterns against complete trailing path
// components of a refname.
func refMatchesTail(name plumbing.ReferenceName, patterns []string) bool {
	for _, p := range patterns {
		if name.String() == p || strings.HasSuffix(name.String(), "/"+p) {
			return true
		}
	}
	return false
}

// sortedRefs lists all refs except HEAD, sorted by name.
func (r *repo) sortedRefs() []plumbing.ReferenceName {
	iter, err := r.Storer.IterReferences()
	if err != nil {
		return nil
	}
	var names []plumbing.ReferenceName
	iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name() != plumbing.HEAD {
			names = append(names, ref.Name())
		}
		return nil
	})
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

func (g *gitRun) forEachRef(args []string) error {
	format := "%(objectname) %(objecttype)\t%(refname)"
	var sorts, patterns []string
	count := -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		case a == "--format" && i+1 < len(args):
			i++
			format = args[i]
		case strings.HasPrefix(a, "--sort="):
			sorts = append(sorts, strings.TrimPrefix(a, "--sort="))
		case a == "--sort" && i+1 < len(args):
			i++
			sorts = append(sorts, args[i])
		case strings.HasPrefix(a, "--count="):
			count, _ = strconv.Atoi(strings.TrimPrefix(a, "--count="))
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			patterns = append(patterns, a)
		}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	var names []plumbing.ReferenceName
	for _, name := range r.sortedRefs() {
		if len(patterns) == 0 || refMatchesPrefix(name, patterns) {
			names = append(names, name)
		}
	}
	// Sort keys apply last-to-first, so the first key wins, as in git.
	for i := len(sorts) - 1; i >= 0; i-- {
		key, desc := strings.TrimPrefix(sorts[i], "-"), strings.HasPrefix(sorts[i], "-")
		sort.SliceStable(names, func(a, b int) bool {
			va, vb := r.refAtom(names[a], key), r.refAtom(names[b], key)
			if na, err1 := strconv.ParseInt(va, 10, 64); err1 == nil {
				if nb, err2 := strconv.ParseInt(vb, 10, 64); err2 == nil {
					if desc {
						return na > nb
					}
					return na < nb
				}
			}
			if desc {
				return va > vb
			}
			return va < vb
		})
	}
	for i, name := range names {
		if count >= 0 && i >= count {
			break
		}
		fmt.Fprintln(g.out, r.expandRefFormat(name, format))
	}
	return nil
}

func refMatchesPrefix(name plumbing.ReferenceName, patterns []string) bool {
	for _, p := range patterns {
		p = strings.TrimSuffix(p, "/")
		if name.String() == p || strings.HasPrefix(name.String(), p+"/") {
			return true
		}
		if ok, _ := path.Match(p, name.String()); ok {
			return true
		}
	}
	return false
}

func (r *repo) expandRefFormat(name plumbing.ReferenceName, format string) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		switch {
		case strings.HasPrefix(format[i:], "%("):
			end := strings.IndexByte(format[i:], ')')
			if end < 0 {
				b.WriteString(format[i:])
				return b.String()
			}
			b.WriteString(r.refAtom(name, format[i+2:i+end]))
			i += end
		case strings.HasPrefix(format[i:], "%%"):
			b.WriteByte('%')
			i++
		case format[i] == '%' && i+2 < len(format) && isHexByte(format[i+1]) && isHexByte(format[i+2]):
			v, _ := strconv.ParseUint(format[i+1:i+3], 16, 8)
			b.WriteByte(byte(v))
			i += 2
		default:
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// refAtom expands one %(atom) of for-each-ref.
func (r *repo) refAtom(name plumbing.ReferenceName, atom string) string {
	field, modifier, _ := strings.Cut(atom, ":")
	deref := strings.HasPrefix(field, "*")
	field = strings.TrimPrefix(field, "*")
	h := r.refHash(name)
	if ref, err := r.Storer.Reference(name); err == nil && ref.Type() == plumbing.HashReference {
		h = ref.Hash()
	}
	if deref {
		t, err := r.TagObject(h)
		if err != nil {
			return ""
		}
		h = t.Target
	}
	obj, _ := r.Storer.EncodedObject(plumbing.AnyObject, h)
	var c *object.Commit
	var t *object.Tag
	if obj != nil {
		switch obj.Type() {
		case plumbing.CommitObject:
			c, _ = r.CommitObject(h)
		case plumbing.TagObject:
			t, _ = r.TagObject(h)
		}
	}
	date := func(when object.Signature) string {
		switch modifier {
		case "":
			return formatDate(when.When, "default")
		case "short", "iso", "iso8601", "iso-strict", "rfc", "rfc2822", "relative", "unix", "raw":
			return formatDate(when.When, modifier)
		}
		if strings.HasPrefix(modifier, "format:") {
			return formatDate(when.When, modifier)
		}
		return formatDate(when.When, "default")
	}
	message := ""
	if c != nil {
		message = c.Message
	} else if t != nil {
		message = t.Message
	}
	switch field {
	case "refname":
		switch {
		case modifier == "short":
			return shortRef(name)
		case strings.HasPrefix(modifier, "lstrip=") || strings.HasPrefix(modifier, "strip="):
			n, _ := strconv.Atoi(modifier[strings.Index(modifier, "=")+1:])
			parts := strings.Split(name.String(), "/")
			return strings.Join(parts[min(n, len(parts)):], "/")
		}
		return name.String()
	case "objectname":
		if modifier == "short" {
			return short(h)
		}
		return h.String()
	case "objecttype":
		if obj == nil {
			return ""
		}
		return obj.Type().String()
	case "objectsize":
		if obj == nil {
			return ""
		}
		return strconv.FormatInt(obj.Size(), 10)
	case "tree":
		if c != nil {
			return c.TreeHash.String()
		}
	case "parent":
		if c != nil {
			var ps []string
			for _, p := range c.ParentHashes {
				ps = append(ps, p.String())
			}
			return strings.Join(ps, " ")
		}
	case "subject":
		return subject(message)
	case "body":
		return body(message)
	case "contents":
		return message
	case "authorname", "authoremail", "authordate", "committername", "committeremail", "committerdate", "creatordate", "taggername", "taggeremail", "taggerdate":
		var sig *object.Signature
		switch {
		case c != nil && strings.HasPrefix(field, "author"):
			sig = &c.Author
		case c != nil && (strings.HasPrefix(field, "committer") || field == "creatordate"):
			sig = &c.Committer
		case t != nil && (strings.HasPrefix(field, "tagger") || field == "creatordate"):
			sig = &t.Tagger
		}
		if sig == nil {
			return ""
		}
		switch {
		case strings.HasSuffix(field, "name"):
			return sig.Name
		case strings.HasSuffix(field, "email"):
			return "<" + sig.Email + ">"
		}
		return date(*sig)
	case "HEAD":
		if b, _ := r.branchName(); b != "" && plumbing.NewBranchReferenceName(b) == name {
			return "*"
		}
		return " "
	case "upstream":
		if !name.IsBranch() {
			return ""
		}
		up := r.upstreamRef(name.Short())
		switch modifier {
		case "short":
			return shortRef(up)
		case "track":
			if t := r.tracking(name.Short()); t != nil && t.counts() != "" {
				return "[" + t.counts() + "]"
			}
			return ""
		}
		return up.String()
	case "symref":
		if ref, err := r.Storer.Reference(name); err == nil && ref.Type() == plumbing.SymbolicReference {
			return ref.Target().String()
		}
	}
	return ""
}

func (g *gitRun) revList(args []string) error {
	var countOnly, parents, objects bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--count":
			countOnly = true
		case "--parents":
			parents = true
		case "--objects":
			objects = true
		default:
			rest = append(rest, a)
		}
	}
	f, err := parseLogArgs(rest)
	if err != nil {
		return err
	}
	if len(f.remaining) > 0 {
		return usagef("error: unknown option `%s'", strings.TrimLeft(f.remaining[0], "-"))
	}
	if len(f.revs) == 0 && !f.all {
		return &exitError{code: 129, msg: "usage: git rev-list [<options>] <commit>... [--] [<path>...]\n"}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	specs, err := r.pathspecs(f.paths)
	if err != nil {
		return err
	}
	count := f.count
	if f.topo {
		f.count = -1
	}
	commits, err := r.walk(f, specs)
	if err != nil {
		return err
	}
	if f.topo {
		commits = topoOrder(commits)
		if count >= 0 && len(commits) > count {
			commits = commits[:count]
		}
	}
	if f.reverse {
		for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
			commits[i], commits[j] = commits[j], commits[i]
		}
	}
	if countOnly {
		fmt.Fprintln(g.out, len(commits))
		return nil
	}
	for _, c := range commits {
		line := c.Hash.String()
		if parents {
			for _, p := range c.ParentHashes {
				line += " " + p.String()
			}
		}
		fmt.Fprintln(g.out, line)
	}
	if objects {
		seen := map[plumbing.Hash]bool{}
		for h := range f.excluded {
			if c, err := r.CommitObject(h); err == nil {
				r.markTree(c.TreeHash, seen)
			}
		}
		for _, c := range commits {
			if err := r.listTree(g.out, c.TreeHash, "", seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *repo) markTree(h plumbing.Hash, seen map[plumbing.Hash]bool) {
	if seen[h] {
		return
	}
	seen[h] = true
	t, err := r.TreeObject(h)
	if err != nil {
		return
	}
	for _, e := range t.Entries {
		if e.Mode == filemode.Dir {
			r.markTree(e.Hash, seen)
		} else {
			seen[e.Hash] = true
		}
	}
}

// listTree prints a tree and its objects for rev-list --objects.
func (r *repo) listTree(w io.Writer, h plumbing.Hash, name string, seen map[plumbing.Hash]bool) error {
	if seen[h] {
		return nil
	}
	seen[h] = true
	fmt.Fprintf(w, "%s %s\n", h, name)
	fmt.Fprint(w, "")
	t, err := r.TreeObject(h)
	if err != nil {
		return err
	}
	for _, e := range t.Entries {
		p := path.Join(name, e.Name)
		if e.Mode == filemode.Dir {
			if err := r.listTree(w, e.Hash, p, seen); err != nil {
				return err
			}
		} else if !seen[e.Hash] {
			seen[e.Hash] = true
			fmt.Fprintf(w, "%s %s\n", e.Hash, p)
		}
	}
	return nil
}

func (g *gitRun) updateRef(args []string) error {
	var del, noDeref bool
	msg := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-d":
			del = true
		case a == "--no-deref":
			noDeref = true
		case a == "-m" && i+1 < len(args):
			i++
			msg = args[i]
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	if len(rest) == 0 || (!del && len(rest) < 2) {
		return &exitError{code: 129, msg: "usage: git update-ref [<options>] -d <refname> [<old-val>]\n   or: git update-ref [<options>]    <refname> <new-val> [<old-val>]\n"}
	}
	name := plumbing.ReferenceName(rest[0])
	if !noDeref {
		if ref, err := r.Storer.Reference(name); err == nil && ref.Type() == plumbing.SymbolicReference {
			name = ref.Target()
		}
	}
	check := func(want string) error {
		h, err := r.resolveRev(want)
		if err != nil {
			return err
		}
		if cur := r.refHash(name); cur != h {
			return fatalf("cannot lock ref '%s': is at %s but expected %s", name, cur, h)
		}
		return nil
	}
	if del {
		if len(rest) > 1 {
			if err := check(rest[1]); err != nil {
				return err
			}
		}
		return r.deleteRef(name)
	}
	h, err := r.resolveRev(rest[1])
	if err != nil {
		return fatalf("%s: not a valid SHA1", rest[1])
	}
	if len(rest) > 2 {
		if err := check(rest[2]); err != nil {
			return err
		}
	}
	return r.updateRef(name, h, msg)
}

func (g *gitRun) symbolicRef(args []string) error {
	var quiet, shortName, del bool
	msg := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--short":
			shortName = true
		case a == "-d" || a == "--delete":
			del = true
		case a == "-m" && i+1 < len(args):
			i++
			msg = args[i]
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return &exitError{code: 129, msg: "usage: git symbolic-ref [-m <reason>] <name> <ref>\n   or: git symbolic-ref [-q] [--short] <name>\n"}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	name := plumbing.ReferenceName(rest[0])
	switch {
	case del:
		return r.Storer.RemoveReference(name)
	case len(rest) == 2:
		target := plumbing.ReferenceName(rest[1])
		if !strings.HasPrefix(target.String(), "refs/") {
			return fatalf("Refusing to point %s outside of refs/", name)
		}
		old := r.refHash(name)
		if err := r.Storer.SetReference(plumbing.NewSymbolicReference(name, target)); err != nil {
			return err
		}
		if msg != "" {
			r.appendReflog(name, old, r.refHash(target), msg)
		}
		return nil
	}
	ref, err := r.Storer.Reference(name)
	if err != nil || ref.Type() != plumbing.SymbolicReference {
		if quiet {
			return failf(1, "")
		}
		return fatalf("ref %s is not a symbolic ref", name)
	}
	if shortName {
		fmt.Fprintln(g.out, shortRef(ref.Target()))
	} else {
		fmt.Fprintln(g.out, ref.Target())
	}
	return nil
}

func (g *gitRun) writeTreeCmd(args []string) error {
	r, err := g.openAny()
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	if un := unmergedPaths(idx); len(un) > 0 {
		var b strings.Builder
		for _, p := range sortedKeys(un) {
			b.WriteString("error: " + p + ": unmerged\n")
		}
		b.WriteString("fatal: git-write-tree: error building trees\n")
		return failf(128, "%s", b.String())
	}
	h, err := r.writeTree(r.indexSide(idx))
	if err != nil {
		return err
	}
	fmt.Fprintln(g.out, h)
	return nil
}

func (g *gitRun) commitTree(args []string) error {
	var parents []plumbing.Hash
	var messages []string
	haveMessage := false
	tree := ""
	r, err := g.openAny()
	if err != nil {
		return err
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-p" && i+1 < len(args):
			i++
			c, err := r.resolveCommit(args[i])
			if err != nil {
				return fatalf("not a valid object name %s", args[i])
			}
			parents = append(parents, c.Hash)
		case a == "-m" && i+1 < len(args):
			i++
			messages = append(messages, args[i])
			haveMessage = true
		case a == "-F" && i+1 < len(args):
			i++
			var data []byte
			if args[i] == "-" {
				data, err = io.ReadAll(g.stdin)
			} else {
				data, err = fs.ReadFile(g.fsys, fsName(g.abs(args[i])))
			}
			if err != nil {
				return fatalf("could not read log file '%s'", args[i])
			}
			messages = append(messages, string(data))
			haveMessage = true
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			tree = a
		}
	}
	if tree == "" {
		return &exitError{code: 129, msg: "usage: git commit-tree <tree> [(-p <parent>)...]\n"}
	}
	th, err := r.resolveObject(tree)
	if err != nil {
		return fatalf("not a valid object name %s", tree)
	}
	if c, err := r.CommitObject(th); err == nil {
		th = c.TreeHash
	}
	if _, err := r.TreeObject(th); err != nil {
		return fatalf("%s is not a valid 'tree' object", tree)
	}
	msg := ""
	if haveMessage {
		for i, m := range messages {
			if i > 0 {
				msg += "\n"
			}
			if !strings.HasSuffix(m, "\n") {
				m += "\n"
			}
			msg += m
		}
	} else {
		data, err := io.ReadAll(g.stdin)
		if err != nil {
			return err
		}
		msg = string(data)
	}
	author, err := r.signature("AUTHOR")
	if err != nil {
		return err
	}
	committer, err := r.signature("COMMITTER")
	if err != nil {
		return err
	}
	h, err := r.storeObject(&object.Commit{Author: *author, Committer: *committer, Message: msg, TreeHash: th, ParentHashes: parents})
	if err != nil {
		return err
	}
	fmt.Fprintln(g.out, h)
	return nil
}

func (g *gitRun) readTree(args []string) error {
	var update, empty bool
	prefix := ""
	var rest []string
	for _, a := range args {
		switch {
		case a == "-u":
			update = true
		case a == "-m" || a == "--reset" || a == "-i":
		case a == "--empty":
			empty = true
		case strings.HasPrefix(a, "--prefix="):
			prefix = strings.TrimPrefix(a, "--prefix=")
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	if empty {
		idx.Entries = nil
		return r.writeIndex(idx)
	}
	if len(rest) != 1 {
		return &exitError{code: 129, msg: "usage: git read-tree [(-m [--trivial] [--aggressive] | --reset | --prefix=<prefix>) [-u] [--index-output=<file>] (--empty | <tree-ish1> [<tree-ish2> [<tree-ish3>]])\n"}
	}
	h, err := r.resolveObject(rest[0])
	if err != nil {
		return fatalf("Not a valid object name %s", rest[0])
	}
	if c, err := r.CommitObject(h); err == nil {
		h = c.TreeHash
	}
	s := side{}
	if err := r.walkTree(s, h, ""); err != nil {
		return fatalf("failed to unpack tree object %s", rest[0])
	}
	if prefix == "" {
		old := r.indexSide(idx)
		idx.Entries = nil
		for p, e := range s {
			var info fs.FileInfo
			if update {
				if cur, ok := old[p]; !ok || !sameEntry(&cur, &e) {
					if info, err = r.writeFile(p, e); err != nil {
						return err
					}
				}
			}
			setEntry(idx, p, e, info)
		}
		if update {
			for p := range old {
				if _, ok := s[p]; !ok {
					r.removeFile(p)
				}
			}
		}
		return r.writeIndex(idx)
	}
	prefix = strings.TrimSuffix(prefix, "/") + "/"
	for p, e := range s {
		full := prefix + p
		if _, err := idx.Entry(full); err == nil {
			return fatalf("Entry '%s' overlaps with '%s'.  Cannot bind.", full, full)
		}
		var info fs.FileInfo
		if update {
			if info, err = r.writeFile(full, e); err != nil {
				return err
			}
		}
		setEntry(idx, full, e, info)
	}
	return r.writeIndex(idx)
}

func (g *gitRun) updateIndex(args []string) error {
	var add, remove, forceRemove, infoOnly bool
	chmod := ""
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	idx, err := r.readIndex()
	if err != nil {
		return err
	}
	setCache := func(modeStr, hash, p string) error {
		mode, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return fatalf("git update-index: --cacheinfo cannot add %s", p)
		}
		h := plumbing.NewHash(hash)
		if len(hash) != 40 {
			if h, err = r.resolveObject(hash); err != nil {
				return fatalf("git update-index: --cacheinfo cannot add %s", p)
			}
		}
		if _, tracked := r.indexSide(idx)[p]; !tracked && !add {
			return failf(128, "error: %s: cannot add to the index - missing --add option?\nfatal: git update-index: --cacheinfo cannot add %s\n", p, p)
		}
		setEntry(idx, p, entry{hash: h, mode: filemode.FileMode(mode)}, nil)
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--add":
			add = true
		case a == "--remove":
			remove = true
		case a == "--force-remove":
			forceRemove = true
		case a == "--info-only":
			infoOnly = true
		case a == "--refresh" || a == "-q" || a == "--really-refresh" || a == "--assume-unchanged" || a == "--no-assume-unchanged":
		case strings.HasPrefix(a, "--chmod="):
			chmod = strings.TrimPrefix(a, "--chmod=")
		case a == "--cacheinfo" && i+1 < len(args):
			parts := strings.Split(args[i+1], ",")
			if len(parts) == 3 {
				i++
				if err := setCache(parts[0], parts[1], parts[2]); err != nil {
					return err
				}
				continue
			}
			if i+3 >= len(args) {
				return usagef("error: option 'cacheinfo' expects <mode>,<sha1>,<path>")
			}
			if err := setCache(args[i+1], args[i+2], args[i+3]); err != nil {
				return err
			}
			i += 3
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			p, err := r.repoPath(a)
			if err != nil {
				return err
			}
			_, tracked := r.indexSide(idx)[p]
			we, inWork, err := r.worktreeEntry(p)
			if err != nil {
				return err
			}
			switch {
			case forceRemove || (remove && !inWork):
				removeEntry(idx, p)
			case !inWork:
				return failf(128, "error: %s: does not exist and --remove not passed\nfatal: Unable to process path %s\n", p, p)
			case !tracked && !add:
				return failf(128, "error: %s: cannot add to the index - missing --add option?\nfatal: Unable to process path %s\n", p, p)
			default:
				data, _ := we.data()
				mode := we.mode
				switch chmod {
				case "+x":
					mode = filemode.Executable
				case "-x":
					mode = filemode.Regular
				}
				if !infoOnly {
					if _, err := r.writeBlob(data, mode); err != nil {
						return err
					}
				}
				info, _ := r.wt.Stat(p)
				setEntry(idx, p, entry{hash: we.hash, mode: mode}, info)
			}
		}
	}
	return r.writeIndex(idx)
}

func (g *gitRun) diffTree(args []string) error {
	var recursive, noCommitID, root, showTrees bool
	var o diffOutput
	var rest []string
	for _, a := range args {
		switch a {
		case "-r":
			recursive = true
		case "-t":
			recursive, showTrees = true, true
		case "--no-commit-id":
			noCommitID = true
		case "--root":
			root = true
		case "-p", "--patch", "-u":
			o.patch, recursive = true, true
		case "--stat":
			o.stat, recursive = true, true
		case "--name-only":
			o.nameOnly = true
		case "--name-status":
			o.nameStat = true
		case "-M", "--no-renames", "-s", "--":
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return &exitError{code: 129, msg: "usage: git diff-tree [--stdin] [-m] [-s] [-v] [--no-commit-id] [--pretty] [-t] [-r] [--root] [<common-diff-options>] <tree-ish> [<tree-ish>] [<path>...]\n"}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	var a, b plumbing.Hash
	var commit *object.Commit
	if len(rest) >= 2 {
		if a, err = r.treeOf(rest[0]); err != nil {
			return err
		}
		if b, err = r.treeOf(rest[1]); err != nil {
			return err
		}
	} else {
		c, err := r.resolveCommit(rest[0])
		if err != nil {
			return err
		}
		commit, b = c, c.TreeHash
		if len(c.ParentHashes) == 0 && !root {
			return nil
		}
		if len(c.ParentHashes) > 0 {
			p, err := r.CommitObject(c.ParentHashes[0])
			if err != nil {
				return err
			}
			a = p.TreeHash
		}
	}
	if commit != nil && !noCommitID {
		fmt.Fprintln(g.out, commit.Hash)
	}
	if o.patch || o.stat || !recursive {
		if o.patch || o.stat || o.nameOnly || o.nameStat {
			sa, sb := side{}, side{}
			if !a.IsZero() {
				r.walkTree(sa, a, "")
			}
			r.walkTree(sb, b, "")
			diffs, err := computeDiffs(changes(sa, sb, nil), true)
			if err != nil {
				return err
			}
			if !recursive && (o.nameOnly || o.nameStat) {
				diffs = topLevel(diffs)
			}
			writeDiffs(g.out, diffs, o)
			return nil
		}
	}
	return r.writeRawDiff(g.out, a, b, "", recursive, showTrees, o)
}

// topLevel collapses file diffs to their top-level entries.
func topLevel(diffs []*fileDiff) []*fileDiff {
	seen := map[string]bool{}
	var out []*fileDiff
	for _, d := range diffs {
		top, _, nested := strings.Cut(d.path, "/")
		if seen[top] {
			continue
		}
		seen[top] = true
		if nested {
			d = &fileDiff{change: change{path: top, from: &entry{}, to: &entry{}}}
		}
		out = append(out, d)
	}
	return out
}

func (r *repo) treeOf(rev string) (plumbing.Hash, error) {
	h, err := r.resolveObject(rev)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if c, err := r.CommitObject(h); err == nil {
		return c.TreeHash, nil
	}
	return h, nil
}

// writeRawDiff prints diff-tree's raw format for trees a and b.
func (r *repo) writeRawDiff(w io.Writer, a, b plumbing.Hash, prefix string, recursive, showTrees bool, o diffOutput) error {
	entries := func(h plumbing.Hash) map[string]object.TreeEntry {
		out := map[string]object.TreeEntry{}
		if h.IsZero() {
			return out
		}
		if t, err := r.TreeObject(h); err == nil {
			for _, e := range t.Entries {
				out[e.Name] = e
			}
		}
		return out
	}
	ea, eb := entries(a), entries(b)
	names := map[string]bool{}
	for n := range ea {
		names[n] = true
	}
	for n := range eb {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	zero := "0000000000000000000000000000000000000000"
	for _, n := range sorted {
		x, inA := ea[n]
		y, inB := eb[n]
		if inA && inB && x.Hash == y.Hash && x.Mode == y.Mode {
			continue
		}
		p := prefix + n
		isDir := (inA && x.Mode == filemode.Dir) || (inB && y.Mode == filemode.Dir)
		if recursive && isDir {
			if showTrees {
				r.rawLine(w, x, y, inA, inB, p, zero, o)
			}
			var ha, hb plumbing.Hash
			if inA && x.Mode == filemode.Dir {
				ha = x.Hash
			}
			if inB && y.Mode == filemode.Dir {
				hb = y.Hash
			}
			if err := r.writeRawDiff(w, ha, hb, p+"/", recursive, showTrees, o); err != nil {
				return err
			}
			continue
		}
		r.rawLine(w, x, y, inA, inB, p, zero, o)
	}
	return nil
}

func (r *repo) rawLine(w io.Writer, x, y object.TreeEntry, inA, inB bool, p, zero string, o diffOutput) {
	status := "M"
	oldMode, newMode, oldHash, newHash := "000000", "000000", zero, zero
	if inA {
		oldMode, oldHash = mode6(x.Mode), x.Hash.String()
	}
	if inB {
		newMode, newHash = mode6(y.Mode), y.Hash.String()
	}
	switch {
	case !inA:
		status = "A"
	case !inB:
		status = "D"
	}
	switch {
	case o.nameOnly:
		fmt.Fprintln(w, q(p))
	case o.nameStat:
		fmt.Fprintf(w, "%s\t%s\n", status, q(p))
	default:
		fmt.Fprintf(w, ":%s %s %s %s %s\t%s\n", oldMode, newMode, oldHash, newHash, status, q(p))
	}
}

func (g *gitRun) countObjects(args []string) error {
	verbose := len(args) > 0 && (args[0] == "-v" || args[0] == "--verbose")
	r, err := g.openAny()
	if err != nil {
		return err
	}
	d := newBillyFS(g.fsys, fsName(path.Join(r.commonDir, "objects")))
	count, blocks := 0, int64(0)
	dirs, _ := d.ReadDir(".")
	for _, dir := range dirs {
		if !dir.IsDir() || len(dir.Name()) != 2 || !isHex(dir.Name()) {
			continue
		}
		files, _ := d.ReadDir(dir.Name())
		for _, f := range files {
			count++
			blocks += (f.Size() + 4095) / 4096 * 4 // on-disk KiB in 4 KiB blocks
		}
	}
	if !verbose {
		fmt.Fprintf(g.out, "%d objects, %d kilobytes\n", count, blocks)
		return nil
	}
	fmt.Fprintf(g.out, "count: %d\nsize: %d\nin-pack: 0\npacks: 0\nsize-pack: 0\nprune-packable: 0\ngarbage: 0\nsize-garbage: 0\n", count, blocks)
	return nil
}

func (g *gitRun) lsRemote(args []string) error {
	var heads, tags, refsOnly, quiet bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--heads", "--branches", "-h", "-b":
			heads = true
		case "--tags", "-t":
			tags = true
		case "--refs":
			refsOnly = true
		case "-q", "--quiet":
			quiet = true
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			rest = append(rest, a)
		}
	}
	r, err := g.openAny()
	if err != nil {
		return err
	}
	name := ""
	if len(rest) > 0 {
		name, rest = rest[0], rest[1:]
	} else {
		branch, _ := r.branchName()
		if name, _ = r.upstream(branch); name == "" || name == "." {
			name = "origin"
		}
	}
	ep, _, err := r.endpointFor(name)
	if err != nil {
		return err
	}
	conn, err := g.connect(ep)
	if err != nil {
		return err
	}
	adv, err := conn.advertised()
	if err != nil {
		return err
	}
	var names []plumbing.ReferenceName
	for n := range adv.refs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	show := func(n plumbing.ReferenceName, h plumbing.Hash) {
		if len(rest) > 0 && !refMatchesTail(n, rest) {
			return
		}
		if !quiet {
			fmt.Fprintf(g.out, "%s\t%s\n", h, n)
		}
	}
	if !heads && !tags && !refsOnly && adv.head != "" {
		if h, ok := adv.refs[adv.head]; ok {
			show(plumbing.HEAD, h)
		}
	}
	for _, n := range names {
		if (heads || tags) && !(heads && n.IsBranch() || tags && n.IsTag()) {
			continue
		}
		show(n, adv.refs[n])
		if p, ok := adv.peeled[n]; ok && !refsOnly {
			show(plumbing.ReferenceName(n.String()+"^{}"), p)
		}
	}
	return nil
}

func (g *gitRun) mergeFile(args []string) error {
	var toStdout bool
	var labels, files []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-p" || a == "--stdout":
			toStdout = true
		case a == "-q" || a == "--quiet":
		case a == "-L" && i+1 < len(args):
			i++
			labels = append(labels, args[i])
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			files = append(files, a)
		}
	}
	if len(files) != 3 {
		return &exitError{code: 129, msg: "usage: git merge-file [<options>] [-L <name1> [-L <orig> [-L <name2>]]] <file1> <orig-file> <file2>\n"}
	}
	var data [3][]byte
	for i, f := range files {
		b, err := fs.ReadFile(g.fsys, fsName(g.abs(f)))
		if err != nil {
			return failf(255, "error: Could not stat %s\n", f)
		}
		data[i] = b
	}
	ours, theirs := files[0], files[2]
	if len(labels) > 0 {
		ours = labels[0]
	}
	if len(labels) > 2 {
		theirs = labels[2]
	}
	merged, conflicts := merge3Count(data[1], data[0], data[2], ours, theirs)
	if toStdout {
		g.out.Write(merged)
	} else {
		f, err := g.fsys.OpenFile(fsName(g.abs(files[0])), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			return err
		}
		f.Write(merged)
		f.Close()
	}
	if conflicts > 0 {
		return &exitError{code: min(conflicts, 127)}
	}
	return nil
}
