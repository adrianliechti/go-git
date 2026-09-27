package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	format "github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"
)

// File is a writable file. Seek, ReadAt, and Truncate are optional; files
// without them are buffered when read and cannot be truncated.
type File interface {
	fs.File
	io.Writer
}

// FS is a writable filesystem using io/fs names ("dir/file", "." for the
// root). OpenFile accepts the flags from os.OpenFile; Remove removes one file
// or empty directory. It has the same shape as go-bash's vfs.WriteFS.
type FS interface {
	fs.FS
	OpenFile(name string, flag int, perm fs.FileMode) (File, error)
	Mkdir(name string, perm fs.FileMode) error
	Remove(name string) error
	Rename(oldName, newName string) error
}

// Options describes one git invocation.
type Options struct {
	Args   []string          // arguments after "git"
	Dir    string            // absolute slash-separated working directory in FS; "/" by default
	Env    map[string]string // GIT_AUTHOR_*, GIT_COMMITTER_*, HOME, EMAIL
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	FS     FS

	// DisableNetwork refuses http(s) remotes. Local paths and file:// URLs
	// inside FS always work.
	DisableNetwork bool
	// HTTPClient is used for http(s) remotes; nil means http.DefaultClient.
	HTTPClient *http.Client
}

// Run executes a subset of the git CLI on top of go-git and returns git's exit
// status. It is a sandbox: every file access goes through opts.FS, and the
// host's environment, config files, network, and processes (editor, pager,
// hooks) are never used. Only go-git APIs that take explicit storage are
// called; its Plain* functions and config-scope loaders read the host.
// The global config is $GIT_CONFIG_GLOBAL or $HOME/.gitconfig inside FS, with
// both variables taken from opts.Env. The error is non-nil only if ctx is done.
func Run(ctx context.Context, opts Options) (int, error) {
	g := &gitRun{
		ctx: ctx, fsys: opts.FS, envs: opts.Env, stdin: opts.Stdin,
		cwd: path.Clean("/" + opts.Dir), out: opts.Stdout, err: opts.Stderr,
		disableNetwork: opts.DisableNetwork, httpClient: opts.HTTPClient,
	}
	if g.fsys == nil {
		g.fsys = emptyFS{}
	}
	if g.stdin == nil {
		g.stdin = strings.NewReader("")
	}
	if g.out == nil {
		g.out = io.Discard
	}
	if g.err == nil {
		g.err = io.Discard
	}
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	code := g.main(opts.Args)
	return code, ctx.Err()
}

// emptyFS stands in for a missing Options.FS: nothing exists and nothing
// can be written.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}
func (emptyFS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}
func (emptyFS) Mkdir(name string, perm fs.FileMode) error {
	return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrPermission}
}
func (emptyFS) Remove(name string) error {
	return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrPermission}
}
func (emptyFS) Rename(oldName, newName string) error {
	return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrPermission}
}

type gitRun struct {
	ctx      context.Context
	fsys     FS
	envs     map[string]string
	stdin    io.Reader
	cwd      string // absolute virtual path, changed by -C
	out, err io.Writer

	disableNetwork bool
	httpClient     *http.Client
	overrides      []string // -c name=value, in order
	reflogAction   string   // command line for reflog messages, e.g. "fetch -q"
}

// configOverride returns the last -c value for key.
func (g *gitRun) configOverride(key string) (string, bool) {
	value, found := "", false
	for _, kv := range g.overrides {
		k, v, hasValue := strings.Cut(kv, "=")
		if !hasValue {
			v = "true" // "-c name" alone means true
		}
		if sameKey(k, key) {
			value, found = v, true
		}
	}
	return value, found
}

// sameKey compares config keys; section and name ignore case.
func sameKey(a, b string) bool {
	sa, suba, na, ok1 := splitKey(a)
	sb, subb, nb, ok2 := splitKey(b)
	return ok1 && ok2 && strings.EqualFold(sa, sb) && suba == subb && strings.EqualFold(na, nb)
}

// configLookup returns key from the global config, local (if non-empty),
// and -c overrides, later sources winning.
func (g *gitRun) configLookup(key, local string) (string, bool) {
	value, found := "", false
	for _, file := range []string{g.globalConfig(), local} {
		if file == "" {
			continue
		}
		cfg, err := g.readConfig(file)
		if err != nil {
			continue
		}
		if v, ok := configGet(cfg, key); ok {
			value, found = v, true
		}
	}
	if v, ok := g.configOverride(key); ok {
		value, found = v, true
	}
	return value, found
}

// alias returns alias.<name> from the configuration, if any.
func (g *gitRun) alias(name string) (string, bool) {
	local := ""
	if r, err := g.openAny(); err == nil {
		local = r.localConfig()
	}
	return g.configLookup("alias."+name, local)
}

// splitAlias splits an alias value into words, honoring quotes.
func splitAlias(s string) []string {
	var words []string
	var cur strings.Builder
	quote := byte(0)
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote, inWord = c, true
		case quote == 0 && (c == ' ' || c == '\t'):
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\\' && i+1 < len(s) && quote != '\'':
			i++
			cur.WriteByte(s[i])
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// exitError carries a git-style exit status and message through helpers.
type exitError struct {
	code int
	msg  string // printed to stderr as-is, including its prefix
}

func (e *exitError) Error() string { return e.msg }

func fatalf(format string, args ...any) error {
	return &exitError{code: 128, msg: "fatal: " + fmt.Sprintf(format, args...) + "\n"}
}

func usagef(format string, args ...any) error {
	return &exitError{code: 129, msg: fmt.Sprintf(format, args...) + "\n"}
}

func failf(code int, format string, args ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, args...)}
}

var commands = map[string]func(*gitRun, []string) error{
	"init":          (*gitRun).init,
	"add":           (*gitRun).add,
	"rm":            (*gitRun).rm,
	"commit":        (*gitRun).commit,
	"status":        (*gitRun).status,
	"log":           (*gitRun).log,
	"show":          (*gitRun).show,
	"diff":          (*gitRun).diff,
	"branch":        (*gitRun).branch,
	"checkout":      (*gitRun).checkout,
	"switch":        (*gitRun).switchBranch,
	"restore":       (*gitRun).restore,
	"reset":         (*gitRun).reset,
	"merge":         (*gitRun).merge,
	"tag":           (*gitRun).tag,
	"config":        (*gitRun).config,
	"rev-parse":     (*gitRun).revParse,
	"cat-file":      (*gitRun).catFile,
	"ls-files":      (*gitRun).lsFiles,
	"hash-object":   (*gitRun).hashObject,
	"reflog":        (*gitRun).reflogCmd,
	"stash":         (*gitRun).stash,
	"rebase":        (*gitRun).rebase,
	"ls-tree":       (*gitRun).lsTree,
	"show-ref":      (*gitRun).showRef,
	"for-each-ref":  (*gitRun).forEachRef,
	"rev-list":      (*gitRun).revList,
	"update-ref":    (*gitRun).updateRef,
	"symbolic-ref":  (*gitRun).symbolicRef,
	"write-tree":    (*gitRun).writeTreeCmd,
	"commit-tree":   (*gitRun).commitTree,
	"read-tree":     (*gitRun).readTree,
	"update-index":  (*gitRun).updateIndex,
	"diff-tree":     (*gitRun).diffTree,
	"count-objects": (*gitRun).countObjects,
	"ls-remote":     (*gitRun).lsRemote,
	"merge-file":    (*gitRun).mergeFile,
	"describe":      (*gitRun).describe,
	"shortlog":      (*gitRun).shortlog,
	"grep":          (*gitRun).grep,
	"blame":         (*gitRun).blame,
	"bisect":        (*gitRun).bisect,
	"format-patch":  (*gitRun).formatPatch,
	"apply":         (*gitRun).apply,
	"am":            (*gitRun).am,
	"worktree":      (*gitRun).worktree,
	"version":       (*gitRun).version,
	"mv":            (*gitRun).mv,
	"clean":         (*gitRun).clean,
	"clone":         (*gitRun).clone,
	"fetch":         (*gitRun).fetchCmd,
	"push":          (*gitRun).push,
	"pull":          (*gitRun).pull,
	"remote":        (*gitRun).remoteCmd,
	"merge-base":    (*gitRun).mergeBaseCmd,
	"cherry-pick":   (*gitRun).cherryPick,
	"revert":        (*gitRun).revert,
}

// Commands that real git has but this implementation deliberately omits.
var unsupported = []string{
	"gc", "notes",
	"submodule",
}

func (g *gitRun) main(args []string) int {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch a := args[0]; {
		case a == "--version" || a == "-v":
			args = []string{"version"}
			continue
		case a == "-h" || a == "--help":
			args = append([]string{"help"}, args[1:]...)
			continue
		case a == "-C" && len(args) > 1:
			g.cwd = g.abs(args[1])
			args = args[2:]
		case a == "-c" && len(args) > 1:
			g.overrides = append(g.overrides, args[1])
			args = args[2:]
		case a == "--no-pager" || a == "-P" || a == "--no-replace-objects":
			args = args[1:]
		default:
			fmt.Fprintf(g.err, "unknown option: %s\n", a)
			return 129
		}
	}
	if len(args) == 0 {
		g.writeMainHelp()
		return 1
	}
	name := args[0]
	run, ok := commands[name]
	var chain []string
	for !ok && name != "help" {
		value, isAlias := g.alias(name)
		if !isAlias {
			break
		}
		if strings.HasPrefix(value, "!") {
			fmt.Fprintf(g.err, "fatal: shell alias '%s' cannot run in this sandbox\n", name)
			return 128
		}
		for _, seen := range chain {
			if seen != name {
				continue
			}
			fmt.Fprintf(g.err, "fatal: alias loop detected: expansion of '%s' does not terminate:\n", chain[0])
			for i, c := range chain {
				mark := ""
				if c == name {
					mark = " <=="
				} else if i == len(chain)-1 {
					mark = " ==>"
				}
				fmt.Fprintf(g.err, "  %s%s\n", c, mark)
			}
			return 128
		}
		chain = append(chain, name)
		args = append(splitAlias(value), args[1:]...)
		if len(args) == 0 {
			fmt.Fprintf(g.err, "fatal: empty alias for %s\n", name)
			return 128
		}
		name = args[0]
		run, ok = commands[name]
	}
	if name == "help" {
		run, ok = (*gitRun).help, true
	}
	if ok && name != "help" && len(args) > 1 {
		switch args[1] {
		case "-h":
			if len(args) > 2 {
				break // e.g. grep -h: -h is only help on its own
			}
			fmt.Fprint(g.out, commandUsage(name))
			return 129
		case "--help":
			run, args = (*gitRun).help, []string{"help", name}
		}
	}
	if !ok {
		for _, u := range unsupported {
			if u == name {
				fmt.Fprintf(g.err, "git: '%s' is not supported by this go-git based git\n", name)
				return 1
			}
		}
		fmt.Fprintf(g.err, "git: '%s' is not a git command. See 'git --help'.\n", name)
		return 1
	}
	g.reflogAction = strings.Join(args, " ")
	err := run(g, args[1:])
	var exit *exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		fmt.Fprint(g.err, exit.msg)
		return exit.code
	default:
		fmt.Fprintf(g.err, "fatal: %v\n", err)
		return 128
	}
}

func (g *gitRun) version([]string) error {
	// The version is the git release whose behavior this package follows.
	fmt.Fprintln(g.out, "git version "+gitVersion)
	return nil
}

// abs resolves a user path against the current directory.
func (g *gitRun) abs(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(g.cwd, p)
}

// fsName converts an absolute virtual path to an io/fs name.
func fsName(abs string) string {
	if abs == "/" {
		return "."
	}
	return strings.TrimPrefix(abs, "/")
}

// repo is an opened repository with its worktree location.
type repo struct {
	*gogit.Repository
	g      *gitRun
	wt     *billyFS // nil for a bare repository
	top    string   // absolute path of the worktree root, or of a bare repository
	gitDir string   // absolute path of the git directory (per worktree)
	// commonDir holds what linked worktrees share: objects, refs, config.
	// It equals gitDir outside linked worktrees.
	commonDir string
	prefix    string // cwd relative to top, "" or ending in "/"
	// quietCheckout switches branches without a reflog entry, as rebase
	// does when given a branch to rebase.
	quietCheckout bool
}

func (r *repo) bare() bool { return r.wt == nil }

// openRepo opens the repository containing cwd and requires a worktree.
func (g *gitRun) openRepo() (*repo, error) {
	r, err := g.openAny()
	if err == nil && r.bare() {
		return nil, fatalf("this operation must be run in a work tree")
	}
	return r, err
}

// openAny opens the repository containing cwd, which may be bare.
func (g *gitRun) openAny() (*repo, error) {
	for dir := g.cwd; ; dir = path.Dir(dir) {
		if info, err := fs.Stat(g.fsys, fsName(path.Join(dir, ".git"))); err == nil {
			if info.IsDir() {
				return g.openAt(dir)
			}
			return g.openLinked(dir)
		}
		if g.isGitDir(dir) {
			return g.openBare(dir)
		}
		if dir == "/" {
			return nil, fatalf("not a git repository (or any of the parent directories): .git")
		}
	}
}

// isGitDir reports whether dir looks like a git directory (a bare repository).
func (g *gitRun) isGitDir(dir string) bool {
	for _, name := range []string{"objects", "refs"} {
		if info, err := fs.Stat(g.fsys, fsName(path.Join(dir, name))); err != nil || !info.IsDir() {
			return false
		}
	}
	info, err := fs.Stat(g.fsys, fsName(path.Join(dir, "HEAD")))
	return err == nil && !info.IsDir()
}

// openPath opens the repository at dir, as git does for a local remote:
// dir itself, dir/.git, or dir.git.
func (g *gitRun) openPath(dir string) (*repo, error) {
	for _, d := range []string{dir, dir + ".git"} {
		if info, err := fs.Stat(g.fsys, fsName(path.Join(d, ".git"))); err == nil {
			if !info.IsDir() {
				return g.openLinked(d)
			}
			return g.openAt(d)
		}
		if g.isGitDir(d) {
			return g.openBare(d)
		}
	}
	return nil, errNoRepo
}

var errNoRepo = errors.New("not a git repository")

func (g *gitRun) openAt(top string) (*repo, error) {
	wt := newBillyFS(g.fsys, fsName(top))
	dot, err := wt.Chroot(".git")
	if err != nil {
		return nil, err
	}
	st := filesystem.NewStorage(dot, cache.NewObjectLRUDefault())
	r, err := gogit.Open(st, wt)
	if err != nil {
		return nil, fatalf("not a git repository: %s: %v", top, err)
	}
	prefix := ""
	if strings.HasPrefix(g.cwd, top+"/") {
		prefix = strings.TrimPrefix(g.cwd, top+"/") + "/"
	} else if top == "/" && g.cwd != "/" {
		prefix = strings.TrimPrefix(g.cwd, "/") + "/"
	}
	gitDir := path.Join(top, ".git")
	return &repo{Repository: r, g: g, wt: wt, top: top, gitDir: gitDir, commonDir: gitDir, prefix: prefix}, nil
}

// readGitLink reads a ".git" file ("gitdir: <path>") of a linked worktree
// or submodule; relative paths are relative to top.
func (g *gitRun) readGitLink(top string) (string, error) {
	data, err := fs.ReadFile(g.fsys, fsName(path.Join(top, ".git")))
	if err != nil {
		return "", err
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return "", fatalf("invalid gitfile format: %s", path.Join(top, ".git"))
	}
	if !path.IsAbs(target) {
		target = path.Join(top, target)
	}
	return path.Clean(target), nil
}

// openLinked opens a worktree whose .git is a file pointing at its git
// directory: a submodule or a linked worktree (which has a commondir).
func (g *gitRun) openLinked(top string) (*repo, error) {
	gitDir, err := g.readGitLink(top)
	if err != nil {
		return nil, err
	}
	commonDir := gitDir
	if data, err := fs.ReadFile(g.fsys, fsName(path.Join(gitDir, "commondir"))); err == nil {
		c := strings.TrimSpace(string(data))
		if !path.IsAbs(c) {
			c = path.Join(gitDir, c)
		}
		commonDir = path.Clean(c)
	}
	wt := newBillyFS(g.fsys, fsName(top))
	var dot billy.Filesystem = newBillyFS(g.fsys, fsName(gitDir))
	if commonDir != gitDir {
		dot = dotgit.NewRepositoryFilesystem(dot, newBillyFS(g.fsys, fsName(commonDir)))
	}
	st := filesystem.NewStorage(dot, cache.NewObjectLRUDefault())
	r, err := gogit.Open(st, wt)
	if err != nil {
		return nil, fatalf("not a git repository: %s", gitDir)
	}
	prefix := ""
	if strings.HasPrefix(g.cwd, top+"/") {
		prefix = strings.TrimPrefix(g.cwd, top+"/") + "/"
	}
	return &repo{Repository: r, g: g, wt: wt, top: top, gitDir: gitDir, commonDir: commonDir, prefix: prefix}, nil
}

func (g *gitRun) openBare(dir string) (*repo, error) {
	st := filesystem.NewStorage(newBillyFS(g.fsys, fsName(dir)), cache.NewObjectLRUDefault())
	r, err := gogit.Open(st, nil)
	if err != nil {
		return nil, fatalf("not a git repository: %s: %v", dir, err)
	}
	return &repo{Repository: r, g: g, top: dir, gitDir: dir, commonDir: dir}, nil
}

// repoPath converts a user path to a repository path ("" for the top).
func (r *repo) repoPath(p string) (string, error) {
	abs := r.g.abs(p)
	switch {
	case abs == r.top:
		return "", nil
	case strings.HasPrefix(abs, r.top+"/"):
		return strings.TrimPrefix(abs, r.top+"/"), nil
	case r.top == "/":
		return strings.TrimPrefix(abs, "/"), nil
	}
	return "", fatalf("%s: '%s' is outside repository at '%s'", p, p, r.top)
}

// display converts a repository path to one relative to the current directory.
func (r *repo) display(p string) string {
	return relPath(strings.TrimSuffix(r.prefix, "/"), p)
}

func relPath(from, to string) string {
	if from == "" {
		return to
	}
	a, b := strings.Split(from, "/"), strings.Split(to, "/")
	i := 0
	for i < len(a) && i < len(b)-1 && a[i] == b[i] {
		i++
	}
	if i == len(a) {
		return strings.Join(b[i:], "/")
	}
	return strings.Repeat("../", len(a)-i) + strings.Join(b[i:], "/")
}

// matchPath reports whether repository path p is selected by pathspec spec.
func matchPath(spec, p string) bool {
	if spec == "" || spec == p || strings.HasPrefix(p, spec+"/") {
		return true
	}
	if strings.ContainsAny(spec, "*?[") {
		ok, _ := path.Match(spec, p)
		return ok
	}
	return false
}

func (r *repo) pathspecs(args []string) ([]string, error) {
	specs := make([]string, 0, len(args))
	for _, a := range args {
		p, err := r.repoPath(a)
		if err != nil {
			return nil, err
		}
		specs = append(specs, p)
	}
	return specs, nil
}

func matchAny(specs []string, p string) bool {
	if len(specs) == 0 {
		return true
	}
	for _, s := range specs {
		if matchPath(s, p) {
			return true
		}
	}
	return false
}

// headCommit returns the commit at HEAD, or nil on an unborn branch.
func (r *repo) headCommit() (*object.Commit, error) {
	ref, err := r.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.CommitObject(ref.Hash())
}

// branchName returns HEAD's branch, or "" when HEAD is detached.
func (r *repo) branchName() (string, error) {
	ref, err := r.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return "", err
	}
	if ref.Type() == plumbing.SymbolicReference {
		return ref.Target().Short(), nil
	}
	return "", nil
}

func errAmbiguous(rev string) error {
	return fatalf("ambiguous argument '%s': unknown revision or path not in the working tree.\n"+
		"Use '--' to separate paths from revisions, like this:\n'git <command> [<revision>...] -- [<file>...]'", rev)
}

// resolveCommit resolves a revision such as HEAD~1, a branch, a tag, a
// hash, @{upstream}, or :/message.
func (r *repo) resolveCommit(rev string) (*object.Commit, error) {
	h, err := r.resolveRev(rev)
	if err != nil {
		return nil, err
	}
	c, err := r.CommitObject(h)
	if err != nil {
		// A revision may name an annotated tag; peel it.
		if t, terr := r.TagObject(h); terr == nil {
			return t.Commit()
		}
		return nil, fatalf("%s is not a commit", rev)
	}
	return c, nil
}

// resolveRev handles the revision syntax go-git lacks and delegates the
// rest (names, hashes, ~n, ^n) to go-git.
func (r *repo) resolveRev(rev string) (plumbing.Hash, error) {
	orig := rev
	rev = strings.TrimSuffix(strings.TrimSuffix(rev, "^{commit}"), "^{}")
	if rev == "@" || strings.HasPrefix(rev, "@~") || strings.HasPrefix(rev, "@^") {
		rev = "HEAD" + rev[1:]
	}
	if pattern, ok := strings.CutPrefix(rev, ":/"); ok {
		return r.searchMessage(pattern, orig)
	}
	if base, suffix, ok := strings.Cut(rev, "@{"); ok {
		spec, rest, _ := strings.Cut(suffix, "}")
		switch strings.ToLower(spec) {
		case "u", "upstream":
			branch := base
			if branch == "" || branch == "HEAD" {
				branch, _ = r.branchName()
			}
			up := r.upstreamRef(branch)
			if up == "" {
				return plumbing.ZeroHash, fatalf("no upstream configured for branch '%s'", branch)
			}
			ref, err := r.Reference(up, true)
			if err != nil {
				return plumbing.ZeroHash, fatalf("upstream branch '%s' not stored as a remote-tracking branch", shortRef(up))
			}
			if rest == "" {
				return ref.Hash(), nil
			}
			return r.resolveRev(ref.Hash().String() + rest)
		default:
			return r.resolveReflog(base, spec, rest, orig)
		}
	}
	// Parent and ancestry suffixes (~n, ^n, ^{...}) are applied here;
	// go-git only resolves the base name.
	if i := strings.IndexAny(rev, "~^"); i > 0 {
		h, err := r.resolveRev(rev[:i])
		if err != nil {
			return plumbing.ZeroHash, errAmbiguous(orig)
		}
		return r.applySuffixes(h, rev[i:], orig)
	}
	// git needs at least four hex digits for an abbreviated hash; go-git
	// would match "c" against any hash starting with c.
	if len(rev) < 4 && isHex(rev) {
		for _, name := range []string{rev, "refs/" + rev, "refs/tags/" + rev, "refs/heads/" + rev, "refs/remotes/" + rev, "refs/remotes/" + rev + "/HEAD"} {
			if ref, err := r.Reference(plumbing.ReferenceName(name), true); err == nil {
				return ref.Hash(), nil
			}
		}
		return plumbing.ZeroHash, errAmbiguous(orig)
	}
	h, err := r.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return plumbing.ZeroHash, errAmbiguous(orig)
	}
	return *h, nil
}

// applySuffixes walks ~n and ^n from h; ^{} and ^{commit} peel tags.
func (r *repo) applySuffixes(h plumbing.Hash, ops, orig string) (plumbing.Hash, error) {
	for len(ops) > 0 {
		op := ops[0]
		ops = ops[1:]
		if op == '^' && strings.HasPrefix(ops, "{") {
			end := strings.IndexByte(ops, '}')
			if end < 0 {
				return plumbing.ZeroHash, errAmbiguous(orig)
			}
			switch ops[1:end] {
			case "", "commit":
				c, err := r.peelCommit(h)
				if err != nil {
					return plumbing.ZeroHash, errAmbiguous(orig)
				}
				h = c
			case "tree":
				c, err := r.CommitObject(h)
				if err != nil {
					return plumbing.ZeroHash, errAmbiguous(orig)
				}
				h = c.TreeHash
			default:
				return plumbing.ZeroHash, errAmbiguous(orig)
			}
			ops = ops[end+1:]
			continue
		}
		n, digits := 1, 0
		for digits < len(ops) && ops[digits] >= '0' && ops[digits] <= '9' {
			digits++
		}
		if digits > 0 {
			n, _ = strconv.Atoi(ops[:digits])
			ops = ops[digits:]
		}
		ch, err := r.peelCommit(h)
		if err != nil {
			return plumbing.ZeroHash, errAmbiguous(orig)
		}
		c, err := r.CommitObject(ch)
		if err != nil {
			return plumbing.ZeroHash, errAmbiguous(orig)
		}
		if op == '~' {
			for ; n > 0; n-- {
				if len(c.ParentHashes) == 0 {
					return plumbing.ZeroHash, errAmbiguous(orig)
				}
				if c, err = r.CommitObject(c.ParentHashes[0]); err != nil {
					return plumbing.ZeroHash, err
				}
			}
			h = c.Hash
			continue
		}
		if n == 0 {
			h = c.Hash
			continue
		}
		if n > len(c.ParentHashes) {
			return plumbing.ZeroHash, errAmbiguous(orig)
		}
		h = c.ParentHashes[n-1]
	}
	return h, nil
}

// peelCommit follows annotated tags to a commit.
func (r *repo) peelCommit(h plumbing.Hash) (plumbing.Hash, error) {
	for range 10 {
		if _, err := r.CommitObject(h); err == nil {
			return h, nil
		}
		t, err := r.TagObject(h)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		h = t.Target
	}
	return plumbing.ZeroHash, fmt.Errorf("tag chain too long")
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

// searchMessage resolves :/pattern to the newest commit reachable from any
// ref whose message matches.
func (r *repo) searchMessage(pattern, orig string) (plumbing.Hash, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return plumbing.ZeroHash, errAmbiguous(orig)
	}
	f := newLogFormat()
	f.all = true
	commits, err := r.walk(f, nil)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	for _, c := range commits {
		if re.MatchString(c.Message) {
			return c.Hash, nil
		}
	}
	return plumbing.ZeroHash, errAmbiguous(orig)
}

func (r *repo) readIndex() (*index.Index, error) {
	return r.Storer.Index()
}

func (r *repo) writeIndex(idx *index.Index) error {
	sort.Slice(idx.Entries, func(i, j int) bool {
		a, b := idx.Entries[i], idx.Entries[j]
		return a.Name < b.Name || (a.Name == b.Name && a.Stage < b.Stage)
	})
	return r.Storer.SetIndex(idx)
}

func short(h plumbing.Hash) string { return h.String()[:7] }

func subject(msg string) string {
	para, _, _ := strings.Cut(strings.TrimLeft(msg, "\n"), "\n\n")
	return strings.Join(strings.Fields(strings.ReplaceAll(para, "\n", " ")), " ")
}

// cleanupMessage applies git's default "whitespace" cleanup for -m and -F.
func cleanupMessage(msg string) string {
	var lines []string
	for _, l := range strings.Split(msg, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if l == "" && (len(lines) == 0 || lines[len(lines)-1] == "") {
			continue
		}
		lines = append(lines, l)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// splitFlags expands clusters of short options such as "-qb name" into
// "-q -b name". flags lists the letters accepted; valued those taking a
// value, which may be attached ("-bname"). Unknown clusters are kept.
func splitFlags(args []string, flags, valued string) []string {
	var out []string
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		if len(a) < 3 || a[0] != '-' || a[1] == '-' {
			out = append(out, a)
			continue
		}
		var parts []string
		ok := true
		for j := 1; j < len(a); j++ {
			c := a[j]
			if strings.IndexByte(flags+valued, c) < 0 {
				ok = false
				break
			}
			parts = append(parts, "-"+string(c))
			if strings.IndexByte(valued, c) >= 0 {
				if j+1 < len(a) {
					parts = append(parts, a[j+1:])
				}
				break
			}
		}
		if ok {
			out = append(out, parts...)
		} else {
			out = append(out, a)
		}
	}
	return out
}

// Identity and configuration

func (g *gitRun) env(key string) (string, bool) {
	v, ok := g.envs[key]
	return v, ok
}

func (r *repo) signature(kind string) (*object.Signature, error) {
	name, _ := r.g.env("GIT_" + kind + "_NAME")
	email, _ := r.g.env("GIT_" + kind + "_EMAIL")
	if name == "" {
		name = r.configValue("user.name")
	}
	if email == "" {
		email = r.configValue("user.email")
	}
	if email == "" {
		email, _ = r.g.env("EMAIL")
	}
	if name == "" || email == "" {
		label := "Author"
		if kind == "COMMITTER" {
			label = "Committer"
		}
		return nil, failf(128, "%s identity unknown\n\n*** Please tell me who you are.\n\nRun\n\n"+
			"  git config --global user.email \"you@example.com\"\n"+
			"  git config --global user.name \"Your Name\"\n\n"+
			"to set your account's default identity.\n"+
			"Omit --global to set the identity only in this repository.\n\n"+
			"fatal: unable to auto-detect email address\n", label)
	}
	when := time.Now().UTC().Truncate(time.Second)
	if d, ok := r.g.env("GIT_" + kind + "_DATE"); ok && d != "" {
		t, err := parseDate(d)
		if err != nil {
			return nil, fatalf("invalid date format: %s", d)
		}
		when = t
	}
	return &object.Signature{Name: name, Email: email, When: when}, nil
}

// parseDate accepts git's internal "<unix> <tz>" form (optionally with '@'),
// RFC 2822, and ISO 8601. Dates without a zone use UTC.
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if f := strings.Fields(strings.TrimPrefix(s, "@")); len(f) <= 2 {
		if sec, err := strconv.ParseInt(f[0], 10, 64); err == nil && (len(f) == 1 || len(f[1]) == 5) {
			loc := time.UTC
			if len(f) == 2 {
				tz, err := strconv.Atoi(f[1][1:])
				if err != nil || (f[1][0] != '+' && f[1][0] != '-') {
					return time.Time{}, fmt.Errorf("bad zone %q", f[1])
				}
				off := (tz/100*60 + tz%100) * 60
				if f[1][0] == '-' {
					off = -off
				}
				loc = time.FixedZone("", off)
			}
			return time.Unix(sec, 0).In(loc), nil
		}
	}
	for _, layout := range []string{
		time.RFC1123Z, "Mon, 2 Jan 2006 15:04:05 -0700", time.RFC3339,
		"2006-01-02 15:04:05 -0700", "2006-01-02T15:04:05", "2006-01-02 15:04:05",
		"Mon Jan 2 15:04:05 2006 -0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q", s)
}

// globalConfig names the global config file inside FS: $GIT_CONFIG_GLOBAL,
// else $HOME/.gitconfig. It returns "" when there is none.
func (g *gitRun) globalConfig() string {
	if file, ok := g.env("GIT_CONFIG_GLOBAL"); ok {
		if file == "" || file == "/dev/null" {
			return ""
		}
		return fsName(g.abs(file))
	}
	home, _ := g.env("HOME")
	if home == "" {
		return ""
	}
	return fsName(path.Join(g.abs(home), ".gitconfig"))
}

func (g *gitRun) readConfig(name string) (*format.Config, error) {
	cfg := format.New()
	if name == "" {
		return cfg, nil
	}
	data, err := fs.ReadFile(g.fsys, name)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := format.NewDecoder(bytes.NewReader(data)).Decode(cfg); err != nil {
		return nil, fatalf("bad config file %s: %v", name, err)
	}
	return cfg, nil
}

func (g *gitRun) writeConfig(name string, cfg *format.Config) error {
	var buf bytes.Buffer
	if err := format.NewEncoder(&buf).Encode(cfg); err != nil {
		return err
	}
	f, err := g.fsys.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r *repo) localConfig() string { return fsName(path.Join(r.commonDir, "config")) }

// configValue returns the effective value of key, with local overriding global.
func (r *repo) configValue(key string) string {
	v, _ := r.g.configLookup(key, r.localConfig())
	return v
}

func splitKey(key string) (section, subsection, name string, ok bool) {
	first, last := strings.Index(key, "."), strings.LastIndex(key, ".")
	if first <= 0 || last == len(key)-1 {
		return "", "", "", false
	}
	section, name = key[:first], key[last+1:]
	if first != last {
		subsection = key[first+1 : last]
	}
	return section, subsection, name, true
}

func configGet(cfg *format.Config, key string) (string, bool) {
	sec, sub, name, ok := splitKey(key)
	if !ok || !cfg.HasSection(sec) {
		return "", false
	}
	opts := cfg.Section(sec).Options
	if sub != "" {
		if !cfg.Section(sec).HasSubsection(sub) {
			return "", false
		}
		opts = cfg.Section(sec).Subsection(sub).Options
	}
	if !opts.Has(name) {
		return "", false
	}
	return opts.Get(name), true
}
