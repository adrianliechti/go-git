package git

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	format "github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Remote configuration

type remoteInfo struct {
	name, url, pushURL string
	fetch              []string
}

func (r *repo) readLocalConfig() (*format.Config, error) { return r.g.readConfig(r.localConfig()) }

func (r *repo) remotes() ([]remoteInfo, error) {
	cfg, err := r.readLocalConfig()
	if err != nil {
		return nil, err
	}
	var out []remoteInfo
	if cfg.HasSection("remote") {
		for _, sub := range cfg.Section("remote").Subsections {
			out = append(out, remoteInfo{
				name: sub.Name, url: sub.Option("url"), pushURL: sub.Option("pushurl"),
				fetch: sub.Options.GetAll("fetch"),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func (r *repo) remote(name string) (*remoteInfo, bool) {
	remotes, err := r.remotes()
	if err != nil {
		return nil, false
	}
	for i := range remotes {
		if remotes[i].name == name {
			return &remotes[i], true
		}
	}
	return nil, false
}

// upstream returns branch's configured remote and merge ref.
func (r *repo) upstream(branch string) (remote string, merge plumbing.ReferenceName) {
	cfg, err := r.readLocalConfig()
	if err != nil || branch == "" {
		return "", ""
	}
	remote, _ = configGet(cfg, "branch."+branch+".remote")
	m, _ := configGet(cfg, "branch."+branch+".merge")
	return remote, plumbing.ReferenceName(m)
}

func (r *repo) setUpstream(branch, remote string, merge plumbing.ReferenceName) error {
	cfg, err := r.readLocalConfig()
	if err != nil {
		return err
	}
	cfg.SetOption("branch", branch, "remote", remote)
	cfg.SetOption("branch", branch, "merge", merge.String())
	return r.g.writeConfig(r.localConfig(), cfg)
}

// trackingRef maps a remote's ref through its fetch refspecs, e.g.
// refs/heads/main to refs/remotes/origin/main. It returns "" if unmapped.
func (r *repo) trackingRef(remote string, ref plumbing.ReferenceName) plumbing.ReferenceName {
	if remote == "." {
		return ref
	}
	info, ok := r.remote(remote)
	if !ok {
		return ""
	}
	for _, f := range info.fetch {
		spec := config.RefSpec(f)
		if spec.Validate() == nil && spec.Match(ref) {
			return spec.Dst(ref)
		}
	}
	return ""
}

// upstreamRef returns the remote-tracking ref of branch's upstream.
func (r *repo) upstreamRef(branch string) plumbing.ReferenceName {
	remote, merge := r.upstream(branch)
	if remote == "" || merge == "" {
		return ""
	}
	return r.trackingRef(remote, merge)
}

// shortRef shortens refs for display: branches and tags lose their prefix,
// remote-tracking refs lose "refs/remotes/".
func shortRef(name plumbing.ReferenceName) string {
	switch {
	case name.IsBranch(), name.IsTag():
		return name.Short()
	case name.IsRemote():
		return strings.TrimPrefix(name.String(), "refs/remotes/")
	}
	return name.String()
}

// endpointFor resolves a remote name or URL.
func (r *repo) endpointFor(nameOrURL string) (endpoint, *remoteInfo, error) {
	if info, ok := r.remote(nameOrURL); ok {
		ep, err := r.g.resolveURL(info.url, r.top)
		return ep, info, err
	}
	if strings.Contains(nameOrURL, "/") || strings.Contains(nameOrURL, ":") || nameOrURL == "." || nameOrURL == ".." {
		ep, err := r.g.resolveURL(nameOrURL, r.g.cwd)
		return ep, nil, err
	}
	return endpoint{}, nil, errNoRemoteRepo(nameOrURL)
}

// Fetching

type fetchLine struct {
	code     byte
	summary  string
	from, to string
	suffix   string
}

type fetchResult struct {
	adv   *remoteRefs
	lines []fetchLine
}

type fetchOptions struct {
	tags  bool // fetch all tags
	prune bool
	force bool
}

// fetch downloads refs matching specs into the repository and returns the
// display lines. Tags pointing into the fetched history follow along.
func (r *repo) fetch(ep endpoint, specs []config.RefSpec, opts fetchOptions, first plumbing.ReferenceName) (*fetchResult, error) {
	conn, err := r.g.connect(ep)
	if err != nil {
		return nil, err
	}
	adv, err := conn.advertised()
	if err != nil {
		return nil, err
	}
	type mapping struct {
		src, dst plumbing.ReferenceName
		hash     plumbing.Hash
		force    bool
	}
	var maps []mapping
	names := make([]plumbing.ReferenceName, 0, len(adv.refs))
	for name := range adv.refs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	for _, name := range names {
		for _, spec := range specs {
			if spec.Match(name) {
				maps = append(maps, mapping{name, spec.Dst(name), adv.refs[name], spec.IsForceUpdate() || opts.force})
				break
			}
		}
	}
	// The upstream of the current branch is listed first, as in FETCH_HEAD.
	sort.SliceStable(maps, func(i, j int) bool { return maps[i].src == first && maps[j].src != first })

	local := r.allRefHashes()
	var wants []plumbing.Hash
	for _, m := range maps {
		if r.Storer.HasEncodedObject(m.hash) != nil {
			wants = append(wants, m.hash)
		}
	}
	if len(wants) > 0 {
		if err := conn.fetch(r, wants, local); err != nil {
			return nil, err
		}
	}
	// Follow tags whose targets are now present, or all tags with --tags.
	var tagWants []plumbing.Hash
	for _, name := range names {
		if !name.IsTag() {
			continue
		}
		if _, err := r.Storer.Reference(name); err == nil {
			continue
		}
		target, ok := adv.peeled[name]
		if !ok {
			target = adv.refs[name]
		}
		if !opts.tags && r.Storer.HasEncodedObject(target) != nil {
			continue
		}
		dup := false
		for _, m := range maps {
			dup = dup || m.dst == name
		}
		if dup {
			continue
		}
		maps = append(maps, mapping{name, name, adv.refs[name], false})
		if r.Storer.HasEncodedObject(adv.refs[name]) != nil {
			tagWants = append(tagWants, adv.refs[name])
		}
	}
	if len(tagWants) > 0 {
		if err := conn.fetch(r, tagWants, r.allRefHashes()); err != nil {
			return nil, err
		}
	}

	res := &fetchResult{adv: adv}
	for _, m := range maps {
		line := fetchLine{from: shortRef(m.src), to: shortRef(m.dst)}
		old := plumbing.ZeroHash
		if ref, err := r.Storer.Reference(m.dst); err == nil {
			old = ref.Hash()
		}
		switch {
		case old == m.hash:
			continue
		case old.IsZero():
			line.code, line.summary = '*', "[new ref]"
			if m.src.IsBranch() {
				line.summary = "[new branch]"
			} else if m.src.IsTag() {
				line.summary = "[new tag]"
			}
		case m.dst.IsTag() && !m.force:
			line.code, line.summary, line.suffix = '!', "[rejected]", "  (would clobber existing tag)"
		case r.isAncestor(old, m.hash):
			line.code, line.summary = ' ', short(old)+".."+short(m.hash)
		case m.force:
			line.code, line.summary, line.suffix = '+', short(old)+"..."+short(m.hash), "  (forced update)"
		default:
			line.code, line.summary, line.suffix = '!', "[rejected]", "  (non-fast-forward)"
		}
		if line.code != '!' {
			if err := r.Storer.SetReference(plumbing.NewHashReference(m.dst, m.hash)); err != nil {
				return nil, err
			}
		}
		res.lines = append(res.lines, line)
	}
	// Like remote.<name>.followRemoteHEAD=create (git 2.48+), record the
	// remote's HEAD when no remote-tracking HEAD exists yet.
	for _, spec := range specs {
		if !spec.IsWildcard() || adv.head == "" || !spec.Match(adv.head) {
			continue
		}
		dst := spec.Dst(adv.head)
		headRef := plumbing.ReferenceName(path.Dir(dst.String()) + "/HEAD")
		if _, err := r.Storer.Reference(headRef); err != nil && dst.IsRemote() {
			if _, err := r.Storer.Reference(dst); err == nil {
				r.Storer.SetReference(plumbing.NewSymbolicReference(headRef, dst))
			}
		}
	}
	if opts.prune {
		for _, spec := range specs {
			iter, err := r.Storer.IterReferences()
			if err != nil {
				return nil, err
			}
			iter.ForEach(func(ref *plumbing.Reference) error {
				if ref.Type() != plumbing.HashReference || !spec.IsWildcard() {
					return nil
				}
				src, ok := reverseRefSpec(spec, ref.Name())
				if !ok {
					return nil
				}
				if _, exists := adv.refs[src]; !exists {
					r.Storer.RemoveReference(ref.Name())
					res.lines = append(res.lines, fetchLine{code: '-', summary: "[deleted]", from: "(none)", to: shortRef(ref.Name())})
				}
				return nil
			})
		}
	}
	return res, nil
}

// reverseRefSpec maps a destination ref back to its source for a wildcard
// refspec such as +refs/heads/*:refs/remotes/origin/*.
func reverseRefSpec(spec config.RefSpec, dst plumbing.ReferenceName) (plumbing.ReferenceName, bool) {
	s := string(spec)
	s = strings.TrimPrefix(s, "+")
	src, d, ok := strings.Cut(s, ":")
	if !ok {
		return "", false
	}
	dPre, dSuf, ok := strings.Cut(d, "*")
	if !ok {
		return "", false
	}
	name := dst.String()
	if !strings.HasPrefix(name, dPre) || !strings.HasSuffix(name, dSuf) || len(name) < len(dPre)+len(dSuf) {
		return "", false
	}
	mid := name[len(dPre) : len(name)-len(dSuf)]
	return plumbing.ReferenceName(strings.Replace(src, "*", mid, 1)), true
}

func (r *repo) allRefHashes() []plumbing.Hash {
	var out []plumbing.Hash
	seen := map[plumbing.Hash]bool{}
	iter, err := r.Storer.IterReferences()
	if err != nil {
		return nil
	}
	iter.ForEach(func(ref *plumbing.Reference) error {
		resolved, err := r.Reference(ref.Name(), true)
		if err == nil && !seen[resolved.Hash()] {
			seen[resolved.Hash()] = true
			out = append(out, resolved.Hash())
		}
		return nil
	})
	return out
}

func (r *repo) isAncestor(a, b plumbing.Hash) bool {
	if a == b {
		return true
	}
	ca, err := r.CommitObject(a)
	if err != nil {
		return false
	}
	cb, err := r.CommitObject(b)
	if err != nil {
		return false
	}
	return mustAncestor(ca, cb)
}

func (g *gitRun) writeFetchLines(url string, lines []fetchLine) {
	if len(lines) == 0 {
		return
	}
	width := 10
	for _, l := range lines {
		width = max(width, len(l.from))
	}
	fmt.Fprintf(g.err, "From %s\n", displayURL(url))
	for _, l := range lines {
		fmt.Fprintf(g.err, " %c %-17s %-*s -> %s%s\n", l.code, l.summary, width, l.from, l.to, l.suffix)
	}
}

func defaultFetchSpec(remote string) string {
	return "+refs/heads/*:refs/remotes/" + remote + "/*"
}

func (g *gitRun) fetchCmd(args []string) error {
	var all, quiet bool
	var opts fetchOptions
	var rest []string
	for _, a := range args {
		switch a {
		case "--all":
			all = true
		case "-q", "--quiet":
			quiet = true
		case "-p", "--prune":
			opts.prune = true
		case "-t", "--tags":
			opts.tags = true
		case "-f", "--force":
			opts.force = true
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
	var names []string
	switch {
	case all:
		remotes, err := r.remotes()
		if err != nil {
			return err
		}
		for _, rm := range remotes {
			names = append(names, rm.name)
		}
	case len(rest) > 0:
		names = rest[:1]
	default:
		branch, _ := r.branchName()
		name, _ := r.upstream(branch)
		if name == "" || name == "." {
			name = "origin"
		}
		if _, ok := r.remote(name); !ok {
			remotes, _ := r.remotes()
			if len(remotes) != 1 {
				return fatalf("No remote repository specified.  Please, specify either a URL or a\nremote name from which new revisions should be fetched.")
			}
			name = remotes[0].name
		}
		names = []string{name}
	}
	for _, name := range names {
		ep, info, err := r.endpointFor(name)
		if err != nil {
			return err
		}
		var specs []config.RefSpec
		if len(rest) > 1 {
			for _, s := range rest[1:] {
				specs = append(specs, argRefSpec(s, name, info))
			}
		} else if info != nil {
			for _, f := range info.fetch {
				specs = append(specs, config.RefSpec(f))
			}
		}
		if opts.tags {
			specs = append(specs, config.RefSpec("refs/tags/*:refs/tags/*"))
		}
		branch, _ := r.branchName()
		_, merge := r.upstream(branch)
		res, err := r.fetch(ep, specs, opts, merge)
		if err != nil {
			return err
		}
		if !quiet {
			g.writeFetchLines(ep.url, res.lines)
		}
	}
	return nil
}

// argRefSpec turns "main" into a spec updating origin/main, as git's
// opportunistic remote-tracking update does.
func argRefSpec(s, remote string, info *remoteInfo) config.RefSpec {
	if strings.Contains(s, ":") {
		return config.RefSpec(s)
	}
	src := s
	if !strings.HasPrefix(src, "refs/") {
		src = "refs/heads/" + src
	}
	if info != nil {
		return config.RefSpec("+" + src + ":refs/remotes/" + remote + "/" + strings.TrimPrefix(src, "refs/heads/"))
	}
	return config.RefSpec(src + ":")
}

// Cloning

func (g *gitRun) clone(args []string) error {
	var quiet, bare, noCheckout bool
	branch, origin := "", "origin"
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--bare":
			bare = true
		case a == "-n" || a == "--no-checkout":
			noCheckout = true
		case (a == "-b" || a == "--branch") && i+1 < len(args):
			i++
			branch = args[i]
		case (a == "-o" || a == "--origin") && i+1 < len(args):
			i++
			origin = args[i]
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-"):
			return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 || len(rest) > 2 {
		return &exitError{code: 129, msg: "fatal: You must specify a repository to clone.\n\n" + commandUsage("clone")}
	}
	src := rest[0]
	ep, err := g.resolveURL(src, g.cwd)
	if err != nil {
		return err
	}
	dir := ""
	if len(rest) == 2 {
		dir = rest[1]
	} else {
		dir = path.Base(strings.TrimSuffix(strings.TrimSuffix(src, "/"), "/.git"))
		dir = strings.TrimSuffix(dir, ".git")
		if bare {
			dir += ".git"
		}
	}
	target := g.abs(dir)
	if entries, err := fs.ReadDir(g.fsys, fsName(target)); err == nil && len(entries) > 0 {
		return fatalf("destination path '%s' already exists and is not an empty directory.", dir)
	} else if info, err := fs.Stat(g.fsys, fsName(target)); err == nil && !info.IsDir() {
		return fatalf("destination path '%s' already exists and is not an empty directory.", dir)
	}
	conn, err := g.connect(ep)
	if err != nil {
		if ep.local != "" {
			return fatalf("repository '%s' does not exist", src)
		}
		return err
	}
	adv, err := conn.advertised()
	if err != nil {
		return err
	}
	if !quiet {
		if bare {
			fmt.Fprintf(g.err, "Cloning into bare repository '%s'...\n", dir)
		} else {
			fmt.Fprintf(g.err, "Cloning into '%s'...\n", dir)
		}
	}
	head := adv.head
	if branch != "" {
		head = plumbing.NewBranchReferenceName(branch)
		if _, ok := adv.refs[head]; !ok {
			return fatalf("Remote branch %s not found in upstream %s", branch, origin)
		}
	}
	initial := head.Short()
	if head == "" {
		initial = "master"
	}
	initArgs := []string{"-q", "--initial-branch", initial, dir}
	if bare {
		initArgs = append([]string{"--bare"}, initArgs...)
	}
	if err := g.init(initArgs); err != nil {
		return err
	}
	cwd := g.cwd
	g.cwd = target
	defer func() { g.cwd = cwd }()
	r, err := g.openAny()
	if err != nil {
		return err
	}
	cfg, err := r.readLocalConfig()
	if err != nil {
		return err
	}
	url := src
	if ep.local != "" {
		url = ep.local
	}
	cfg.SetOption("remote", origin, "url", url)
	spec := "+refs/heads/*:refs/heads/*"
	if !bare {
		spec = defaultFetchSpec(origin)
		cfg.SetOption("remote", origin, "fetch", spec)
	}
	if err := g.writeConfig(r.localConfig(), cfg); err != nil {
		return err
	}
	if _, err := r.fetch(ep, []config.RefSpec{config.RefSpec(spec)}, fetchOptions{tags: true}, ""); err != nil {
		return err
	}
	if len(adv.refs) == 0 {
		if !quiet {
			fmt.Fprintln(g.err, "warning: You appear to have cloned an empty repository.")
		}
	}
	if !bare && head != "" {
		if tip, ok := adv.refs[head]; ok {
			if err := r.Storer.SetReference(plumbing.NewSymbolicReference(
				plumbing.ReferenceName("refs/remotes/"+origin+"/HEAD"),
				plumbing.ReferenceName("refs/remotes/"+origin+"/"+head.Short()))); err != nil {
				return err
			}
			// Check out while HEAD is still unborn, then create the branch.
			if !noCheckout {
				c, err := r.CommitObject(tip)
				if err != nil {
					return err
				}
				if err := r.switchTo(c, "checkout", true); err != nil {
					return err
				}
			}
			if err := r.Storer.SetReference(plumbing.NewHashReference(head, tip)); err != nil {
				return err
			}
		}
		if err := r.setUpstream(head.Short(), origin, head); err != nil {
			return err
		}
	}
	if !quiet && ep.local != "" {
		fmt.Fprintln(g.err, "done.")
	}
	return nil
}

// Pushing

type pushLine struct {
	code            byte
	summary, refs   string
	suffix          string
	rejected        bool
	remoteRejection bool
}

func (g *gitRun) push(args []string) error {
	var setUpstream, force, tags, deleteRefs, quiet, all bool
	var rest []string
	for _, a := range args {
		switch a {
		case "-u", "--set-upstream":
			setUpstream = true
		case "-f", "--force":
			force = true
		case "--tags":
			tags = true
		case "-d", "--delete":
			deleteRefs = true
		case "-q", "--quiet":
			quiet = true
		case "--all", "--branches":
			all = true
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
	current, _ := r.branchName()
	remoteName := ""
	if len(rest) > 0 {
		remoteName, rest = rest[0], rest[1:]
	} else {
		remoteName, _ = r.upstream(current)
		if remoteName == "" {
			if _, ok := r.remote("origin"); ok {
				remoteName = "origin"
			} else if remotes, _ := r.remotes(); len(remotes) == 1 {
				remoteName = remotes[0].name
			}
		}
		if remoteName == "" {
			return fatalf("No configured push destination.\nEither specify the URL from the command-line or configure a remote repository using\n\n" +
				"    git remote add <name> <url>\n\nand then push using the remote name\n\n    git push <name>\n")
		}
	}
	ep, info, err := r.endpointFor(remoteName)
	if err != nil {
		return err
	}
	if info != nil && info.pushURL != "" {
		if ep, err = g.resolveURL(info.pushURL, r.top); err != nil {
			return err
		}
	}

	type spec struct {
		src      plumbing.ReferenceName // "" for deletes
		dst      plumbing.ReferenceName
		hash     plumbing.Hash
		force    bool
		srcShort string
	}
	var specs []spec
	addLocal := func(s string, forced bool) error {
		srcName, dstName, hasDst := strings.Cut(s, ":")
		if srcName == "" && hasDst {
			specs = append(specs, spec{dst: expandRef(dstName, "refs/heads/")})
			return nil
		}
		if srcName == "HEAD" {
			if current == "" {
				return fatalf("You are not currently on a branch.")
			}
			srcName = current
		}
		var src plumbing.ReferenceName
		for _, cand := range []plumbing.ReferenceName{plumbing.ReferenceName(srcName), plumbing.NewBranchReferenceName(srcName), plumbing.NewTagReferenceName(srcName)} {
			if _, err := r.Storer.Reference(cand); err == nil && strings.HasPrefix(cand.String(), "refs/") {
				src = cand
				break
			}
		}
		if src == "" {
			return failf(1, "error: src refspec %s does not match any\nerror: failed to push some refs to '%s'\n", srcName, ep.url)
		}
		dst := src
		if hasDst {
			prefix := "refs/heads/"
			if src.IsTag() {
				prefix = "refs/tags/"
			}
			dst = expandRef(dstName, prefix)
		}
		ref, err := r.Reference(src, true)
		if err != nil {
			return err
		}
		specs = append(specs, spec{src: src, dst: dst, hash: ref.Hash(), force: forced || force, srcShort: srcName})
		return nil
	}
	for _, s := range rest {
		forced := strings.HasPrefix(s, "+")
		s = strings.TrimPrefix(s, "+")
		if deleteRefs {
			s = ":" + s
		}
		if err := addLocal(s, forced); err != nil {
			return err
		}
	}
	if all || tags {
		iter, err := r.Storer.IterReferences()
		if err != nil {
			return err
		}
		var names []string
		iter.ForEach(func(ref *plumbing.Reference) error {
			if (all && ref.Name().IsBranch()) || (tags && ref.Name().IsTag()) {
				names = append(names, ref.Name().String())
			}
			return nil
		})
		sort.Strings(names)
		for _, n := range names {
			if err := addLocal(n, false); err != nil {
				return err
			}
		}
	}
	if len(rest) == 0 && !all && !tags {
		if current == "" {
			return fatalf("You are not currently on a branch.\nTo push the history leading to the current (detached HEAD)\nstate now, use\n\n    git push %s HEAD:<name-of-remote-branch>\n", remoteName)
		}
		upRemote, merge := r.upstream(current)
		dst := plumbing.NewBranchReferenceName(current)
		if upRemote == remoteName && merge != "" {
			dst = merge
		} else if len(args) == 0 && !setUpstream {
			return fatalf("The current branch %s has no upstream branch.\nTo push the current branch and set the remote as upstream, use\n\n"+
				"    git push --set-upstream %s %s\n\nTo have this happen automatically for branches without a tracking\n"+
				"upstream, see 'push.autoSetupRemote' in 'git help config'.\n", current, remoteName, current)
		}
		if err := addLocal(current+":"+dst.String(), false); err != nil {
			return err
		}
	}

	conn, err := g.connect(ep)
	if err != nil {
		return err
	}
	adv, err := conn.advertised()
	if err != nil {
		return err
	}
	var updates []refUpdate
	var lines []pushLine
	var fetchFirst, nonFF bool
	for _, s := range specs {
		old := adv.refs[s.dst]
		refs := s.srcShort + " -> " + shortRef(s.dst)
		switch {
		case s.src == "":
			if old.IsZero() {
				fmt.Fprintf(g.err, "error: unable to delete '%s': remote ref does not exist\n", shortRef(s.dst))
				lines = append(lines, pushLine{rejected: true})
				continue
			}
			updates = append(updates, refUpdate{name: s.dst, old: old})
			lines = append(lines, pushLine{code: '-', summary: "[deleted]", refs: shortRef(s.dst)})
		case old == s.hash:
			continue
		case old.IsZero():
			summary := "[new branch]"
			if s.dst.IsTag() {
				summary = "[new tag]"
			} else if !s.dst.IsBranch() {
				summary = "[new reference]"
			}
			updates = append(updates, refUpdate{name: s.dst, new: s.hash})
			lines = append(lines, pushLine{code: '*', summary: summary, refs: refs})
		case s.dst.IsTag() && !s.force:
			lines = append(lines, pushLine{code: '!', summary: "[rejected]", refs: refs, suffix: " (already exists)", rejected: true})
		case r.Storer.HasEncodedObject(old) != nil && !s.force:
			fetchFirst = true
			lines = append(lines, pushLine{code: '!', summary: "[rejected]", refs: refs, suffix: " (fetch first)", rejected: true})
		case r.isAncestor(old, s.hash):
			updates = append(updates, refUpdate{name: s.dst, old: old, new: s.hash})
			lines = append(lines, pushLine{code: ' ', summary: short(old) + ".." + short(s.hash), refs: refs})
		case s.force:
			updates = append(updates, refUpdate{name: s.dst, old: old, new: s.hash})
			lines = append(lines, pushLine{code: '+', summary: short(old) + "..." + short(s.hash), refs: refs, suffix: " (forced update)"})
		default:
			nonFF = true
			lines = append(lines, pushLine{code: '!', summary: "[rejected]", refs: refs, suffix: " (non-fast-forward)", rejected: true})
		}
	}
	remoteRejected := map[plumbing.ReferenceName]string{}
	if len(updates) > 0 {
		if remoteRejected, err = conn.push(r, updates, r.valuesOf(adv)); err != nil {
			return err
		}
	}
	if reason, ok := remoteRejected[plumbing.NewBranchReferenceName(firstBranch(updates, remoteRejected))]; ok && reason == "branch is currently checked out" {
		fmt.Fprint(g.err, deniedCurrentBranch(updates, remoteRejected))
	}
	// Mark remote rejections on their lines.
	i := 0
	for li := range lines {
		if lines[li].rejected || lines[li].code == 0 {
			continue
		}
		u := updates[i]
		i++
		if reason, ok := remoteRejected[u.name]; ok {
			lines[li].code, lines[li].summary, lines[li].suffix = '!', "[remote rejected]", " ("+reason+")"
			lines[li].rejected, lines[li].remoteRejection = true, true
		}
	}
	failed := false
	var shown []pushLine
	for _, l := range lines {
		if l.code != 0 {
			shown = append(shown, l)
		}
		failed = failed || l.rejected
	}
	if len(shown) == 0 && !failed {
		fmt.Fprintln(g.err, "Everything up-to-date")
	} else if len(shown) > 0 && (!quiet || failed) {
		fmt.Fprintf(g.err, "To %s\n", ep.url)
		for _, l := range shown {
			fmt.Fprintf(g.err, " %c %-17s %s%s\n", l.code, l.summary, l.refs, l.suffix)
		}
	}
	// Update remote-tracking refs and upstream configuration.
	for _, u := range updates {
		if _, rejected := remoteRejected[u.name]; rejected || info == nil {
			continue
		}
		if tracking := r.trackingRef(remoteName, u.name); tracking != "" {
			if u.new.IsZero() {
				r.Storer.RemoveReference(tracking)
			} else {
				r.Storer.SetReference(plumbing.NewHashReference(tracking, u.new))
			}
		}
	}
	if setUpstream && !failed {
		for _, s := range specs {
			if s.src.IsBranch() && s.dst.IsBranch() {
				if err := r.setUpstream(s.src.Short(), remoteName, s.dst); err != nil {
					return err
				}
				if !quiet {
					fmt.Fprintf(g.out, "branch '%s' set up to track '%s/%s'.\n", s.src.Short(), remoteName, s.dst.Short())
				}
			}
		}
	}
	if failed {
		msg := fmt.Sprintf("error: failed to push some refs to '%s'\n", ep.url)
		switch {
		case fetchFirst:
			msg += "hint: Updates were rejected because the remote contains work that you do not\n" +
				"hint: have locally. This is usually caused by another repository pushing to\n" +
				"hint: the same ref. If you want to integrate the remote changes, use\n" +
				"hint: 'git pull' before pushing again.\n" +
				"hint: See the 'Note about fast-forwards' in 'git push --help' for details.\n"
		case nonFF:
			msg += "hint: Updates were rejected because the tip of your current branch is behind\n" +
				"hint: its remote counterpart. If you want to integrate the remote changes,\n" +
				"hint: use 'git pull' before pushing again.\n" +
				"hint: See the 'Note about fast-forwards' in 'git push --help' for details.\n"
		}
		return failf(1, "%s", msg)
	}
	return nil
}

func expandRef(name, prefix string) plumbing.ReferenceName {
	if strings.HasPrefix(name, "refs/") {
		return plumbing.ReferenceName(name)
	}
	return plumbing.ReferenceName(prefix + name)
}

func (r *repo) valuesOf(adv *remoteRefs) []plumbing.Hash {
	var out []plumbing.Hash
	for _, h := range adv.refs {
		out = append(out, h)
	}
	return out
}

func firstBranch(updates []refUpdate, rejected map[plumbing.ReferenceName]string) string {
	for _, u := range updates {
		if _, ok := rejected[u.name]; ok && u.name.IsBranch() {
			return u.name.Short()
		}
	}
	return ""
}

// deniedCurrentBranch is receive-pack's message for pushing into the
// checked-out branch of a non-bare repository.
func deniedCurrentBranch(updates []refUpdate, rejected map[plumbing.ReferenceName]string) string {
	name := plumbing.NewBranchReferenceName(firstBranch(updates, rejected))
	lines := []string{
		"error: refusing to update checked out branch: " + name.String(),
		"error: By default, updating the current branch in a non-bare repository",
		"is denied, because it will make the index and work tree inconsistent",
		"with what you pushed, and will require 'git reset --hard' to match",
		"the work tree to HEAD.",
		"",
		"You can set the 'receive.denyCurrentBranch' configuration variable",
		"to 'ignore' or 'warn' in the remote repository to allow pushing into",
		"its current branch; however, this is not recommended unless you",
		"arranged to update its work tree to match what you pushed in some",
		"other way.",
		"",
		"To squelch this message and still keep the default behaviour, set",
		"'receive.denyCurrentBranch' configuration variable to 'refuse'.",
	}
	var b strings.Builder
	// Without a terminal, git pads sideband lines with eight spaces.
	for _, l := range lines {
		if l == "" {
			b.WriteString("remote: \n")
		} else {
			b.WriteString("remote: " + l + "        \n")
		}
	}
	return b.String()
}

// Pulling

func (g *gitRun) pull(args []string) error {
	var ffOnly, noRebase, quiet bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--ff-only":
			ffOnly = true
		case "--no-rebase", "--ff", "--no-ff":
			noRebase = true
		case "-q", "--quiet":
			quiet = true
		case "-r", "--rebase":
			return fatalf("pull --rebase is not supported by this git")
		default:
			if strings.HasPrefix(a, "-") {
				return usagef("error: unknown option `%s'", strings.TrimLeft(a, "-"))
			}
			rest = append(rest, a)
		}
	}
	r, err := g.openRepo()
	if err != nil {
		return err
	}
	current, _ := r.branchName()
	remoteName, merge := r.upstream(current)
	if len(rest) > 0 {
		remoteName = rest[0]
		merge = ""
		if len(rest) > 1 {
			merge = expandRef(rest[1], "refs/heads/")
		}
	}
	if remoteName == "" || (merge == "" && len(rest) == 0) {
		return failf(1, "There is no tracking information for the current branch.\n"+
			"Please specify which branch you want to merge with.\n"+
			"See git-pull(1) for details.\n\n    git pull <remote> <branch>\n\n"+
			"If you wish to set tracking information for this branch you can do so with:\n\n"+
			"    git branch --set-upstream-to=origin/<branch> %s\n\n", current)
	}
	ep, info, err := r.endpointFor(remoteName)
	if err != nil {
		return err
	}
	var specs []config.RefSpec
	if info != nil {
		for _, f := range info.fetch {
			specs = append(specs, config.RefSpec(f))
		}
	}
	res, err := r.fetch(ep, specs, fetchOptions{}, merge)
	if err != nil {
		return err
	}
	if !quiet {
		g.writeFetchLines(ep.url, res.lines)
	}
	if merge == "" {
		if res.adv.head == "" {
			return fatalf("no candidates for merging")
		}
		merge = res.adv.head
	}
	tip, ok := res.adv.refs[merge]
	if !ok {
		return fatalf("couldn't find remote ref %s", merge)
	}
	target, err := r.CommitObject(tip)
	if err != nil {
		return err
	}
	head, err := r.headCommit()
	if err != nil {
		return err
	}
	if head != nil && !r.isAncestor(head.Hash, tip) && !r.isAncestor(tip, head.Hash) {
		pullFF, _ := r.g.configLookup("pull.ff", r.localConfig())
		pullRebase, hasRebase := r.g.configLookup("pull.rebase", r.localConfig())
		if ffOnly || pullFF == "only" {
			return failf(128, "hint: Diverging branches can't be fast-forwarded, you need to either:\n"+
				"hint:\nhint: \tgit merge --no-ff\nhint:\nhint: or:\nhint:\nhint: \tgit rebase\nhint:\n"+
				"hint: Disable this message with \"git config set advice.diverging false\"\n"+
				"fatal: Not possible to fast-forward, aborting.\n")
		}
		if !noRebase && !hasRebase && pullFF == "" {
			return failf(128, "hint: You have divergent branches and need to specify how to reconcile them.\n"+
				"hint: You can do so by running one of the following commands sometime before\n"+
				"hint: your next pull:\nhint:\n"+
				"hint:   git config pull.rebase false  # merge\n"+
				"hint:   git config pull.rebase true   # rebase\n"+
				"hint:   git config pull.ff only       # fast-forward only\nhint:\n"+
				"hint: You can replace \"git config\" with \"git config --global\" to set a default\n"+
				"hint: preference for all repositories. You can also pass --rebase, --no-rebase,\n"+
				"hint: or --ff-only on the command line to override the configured default per\n"+
				"hint: invocation.\n"+
				"fatal: Need to specify how to reconcile divergent branches.\n")
		}
		if pullRebase == "true" {
			return fatalf("pull --rebase is not supported by this git")
		}
		return r.threeWayMerge(target, "Merge branch '"+merge.Short()+"' of "+displayURL(ep.url), mergeOptions{label: shortRef(r.trackingRef(remoteName, merge)), quiet: quiet})
	}
	return r.fastForward(head, target, quiet)
}

func mustConfig(r *repo) *format.Config {
	cfg, err := r.readLocalConfig()
	if err != nil {
		return format.New()
	}
	return cfg
}

// fastForward moves HEAD's branch to target like git merge's fast-forward.
func (r *repo) fastForward(head, target *object.Commit, quiet bool) error {
	g := r.g
	if head != nil && r.isAncestor(target.Hash, head.Hash) {
		fmt.Fprintln(g.out, "Already up to date.")
		return nil
	}
	if err := r.switchTo(target, "merge", false); err != nil {
		return err
	}
	if err := r.setHead(target.Hash); err != nil {
		return err
	}
	if quiet || head == nil {
		return nil
	}
	fmt.Fprintf(g.out, "Updating %s..%s\nFast-forward\n", short(head.Hash), short(target.Hash))
	a, err := r.treeSide(head)
	if err != nil {
		return err
	}
	b, err := r.treeSide(target)
	if err != nil {
		return err
	}
	diffs, err := computeDiffs(changes(a, b, nil), false)
	if err != nil {
		return err
	}
	writeStat(g.out, diffs)
	writeModeSummary(g.out, diffs)
	return nil
}

// Remote management

func (g *gitRun) remoteCmd(args []string) error {
	r, err := g.openAny()
	if err != nil {
		return err
	}
	verbose := false
	if len(args) > 0 && (args[0] == "-v" || args[0] == "--verbose") {
		verbose, args = true, args[1:]
	}
	if len(args) == 0 {
		remotes, err := r.remotes()
		if err != nil {
			return err
		}
		for _, rm := range remotes {
			if !verbose {
				fmt.Fprintln(g.out, rm.name)
				continue
			}
			push := rm.url
			if rm.pushURL != "" {
				push = rm.pushURL
			}
			fmt.Fprintf(g.out, "%s\t%s (fetch)\n%s\t%s (push)\n", rm.name, rm.url, rm.name, push)
		}
		return nil
	}
	cfg, err := r.readLocalConfig()
	if err != nil {
		return err
	}
	save := func() error { return g.writeConfig(r.localConfig(), cfg) }
	exists := func(name string) bool {
		_, ok := r.remote(name)
		return ok
	}
	need := func(n int) error {
		if len(args) != n+1 {
			return &exitError{code: 129, msg: commandUsage("remote")}
		}
		return nil
	}
	switch args[0] {
	case "add":
		fetchAfter := false
		if len(args) > 1 && args[1] == "-f" {
			fetchAfter, args = true, append(args[:1:1], args[2:]...)
		}
		if err := need(2); err != nil {
			return err
		}
		name, url := args[1], args[2]
		if exists(name) {
			return failf(3, "error: remote %s already exists.\n", name)
		}
		cfg.SetOption("remote", name, "url", url)
		cfg.SetOption("remote", name, "fetch", defaultFetchSpec(name))
		if err := save(); err != nil {
			return err
		}
		if fetchAfter {
			return g.fetchCmd([]string{name})
		}
	case "remove", "rm":
		if err := need(1); err != nil {
			return err
		}
		name := args[1]
		if !exists(name) {
			return failf(2, "error: No such remote: '%s'\n", name)
		}
		cfg.RemoveSubsection("remote", name)
		if cfg.HasSection("branch") {
			for _, sub := range cfg.Section("branch").Subsections {
				if sub.Option("remote") == name {
					sub.RemoveOption("remote")
					sub.RemoveOption("merge")
				}
			}
		}
		if err := save(); err != nil {
			return err
		}
		r.removeRefsWithPrefix("refs/remotes/" + name + "/")
	case "rename":
		if err := need(2); err != nil {
			return err
		}
		old, name := args[1], args[2]
		if !exists(old) {
			return failf(2, "error: No such remote: '%s'\n", old)
		}
		if exists(name) {
			return failf(3, "error: remote %s already exists.\n", name)
		}
		sub := cfg.Section("remote").Subsection(old)
		cfg.SetOption("remote", name, "url", sub.Option("url"))
		for _, f := range sub.Options.GetAll("fetch") {
			cfg.AddOption("remote", name, "fetch", strings.ReplaceAll(f, "refs/remotes/"+old+"/", "refs/remotes/"+name+"/"))
		}
		if p := sub.Option("pushurl"); p != "" {
			cfg.SetOption("remote", name, "pushurl", p)
		}
		cfg.RemoveSubsection("remote", old)
		if cfg.HasSection("branch") {
			for _, b := range cfg.Section("branch").Subsections {
				if b.Option("remote") == old {
					b.SetOption("remote", name)
				}
			}
		}
		if err := save(); err != nil {
			return err
		}
		r.renameRefsWithPrefix("refs/remotes/"+old+"/", "refs/remotes/"+name+"/")
	case "get-url":
		if err := need(1); err != nil {
			return err
		}
		info, ok := r.remote(args[1])
		if !ok {
			return failf(2, "error: No such remote '%s'\n", args[1])
		}
		fmt.Fprintln(g.out, info.url)
	case "set-url":
		if err := need(2); err != nil {
			return err
		}
		if !exists(args[1]) {
			return failf(2, "error: No such remote '%s'\n", args[1])
		}
		cfg.SetOption("remote", args[1], "url", args[2])
		return save()
	default:
		return &exitError{code: 129, msg: "error: unknown subcommand: `" + args[0] + "'\n" + commandUsage("remote")}
	}
	return nil
}

func (r *repo) removeRefsWithPrefix(prefix string) {
	iter, err := r.Storer.IterReferences()
	if err != nil {
		return
	}
	var names []plumbing.ReferenceName
	iter.ForEach(func(ref *plumbing.Reference) error {
		if strings.HasPrefix(ref.Name().String(), prefix) {
			names = append(names, ref.Name())
		}
		return nil
	})
	for _, n := range names {
		r.Storer.RemoveReference(n)
	}
}

func (r *repo) renameRefsWithPrefix(old, new string) {
	iter, err := r.Storer.IterReferences()
	if err != nil {
		return
	}
	var refs []*plumbing.Reference
	iter.ForEach(func(ref *plumbing.Reference) error {
		if strings.HasPrefix(ref.Name().String(), old) {
			refs = append(refs, ref)
		}
		return nil
	})
	rename := func(n plumbing.ReferenceName) plumbing.ReferenceName {
		return plumbing.ReferenceName(new + strings.TrimPrefix(n.String(), old))
	}
	for _, ref := range refs {
		var moved *plumbing.Reference
		if ref.Type() == plumbing.SymbolicReference {
			moved = plumbing.NewSymbolicReference(rename(ref.Name()), rename(ref.Target()))
		} else {
			moved = plumbing.NewHashReference(rename(ref.Name()), ref.Hash())
		}
		r.Storer.SetReference(moved)
		r.Storer.RemoveReference(ref.Name())
	}
}
