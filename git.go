package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	format "github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
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

type gitRun struct {
	ctx      context.Context
	fsys     FS
	envs     map[string]string
	stdin    io.Reader
	cwd      string // absolute virtual path, changed by -C
	out, err io.Writer
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
	"init":        (*gitRun).init,
	"add":         (*gitRun).add,
	"rm":          (*gitRun).rm,
	"commit":      (*gitRun).commit,
	"status":      (*gitRun).status,
	"log":         (*gitRun).log,
	"show":        (*gitRun).show,
	"diff":        (*gitRun).diff,
	"branch":      (*gitRun).branch,
	"checkout":    (*gitRun).checkout,
	"switch":      (*gitRun).switchBranch,
	"restore":     (*gitRun).restore,
	"reset":       (*gitRun).reset,
	"merge":       (*gitRun).merge,
	"tag":         (*gitRun).tag,
	"config":      (*gitRun).config,
	"rev-parse":   (*gitRun).revParse,
	"cat-file":    (*gitRun).catFile,
	"ls-files":    (*gitRun).lsFiles,
	"hash-object": (*gitRun).hashObject,
	"version":     (*gitRun).version,
	"mv":          (*gitRun).mv,
	"clean":       (*gitRun).clean,
}

// Commands that real git has but this implementation deliberately omits.
var unsupported = []string{
	"am", "apply", "bisect", "blame", "cherry-pick", "clone", "fetch",
	"gc", "grep", "notes", "pull", "push", "rebase", "reflog", "remote",
	"revert", "stash", "submodule", "worktree",
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
			args = args[2:] // accepted for compatibility; no effect
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
	if name == "help" {
		run, ok = (*gitRun).help, true
	}
	if ok && name != "help" && len(args) > 1 {
		switch args[1] {
		case "-h":
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
	fmt.Fprintln(g.out, "git version 2.0.0 (go-git v5)")
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
	wt     *billyFS
	top    string // absolute path of the worktree root
	prefix string // cwd relative to top, "" or ending in "/"
}

func (g *gitRun) openRepo() (*repo, error) {
	for dir := g.cwd; ; dir = path.Dir(dir) {
		if info, err := fs.Stat(g.fsys, fsName(path.Join(dir, ".git"))); err == nil && info.IsDir() {
			return g.openAt(dir)
		}
		if dir == "/" {
			return nil, fatalf("not a git repository (or any of the parent directories): .git")
		}
	}
}

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
	if g.cwd != top {
		prefix = strings.TrimPrefix(g.cwd, top+"/") + "/"
	}
	return &repo{Repository: r, g: g, wt: wt, top: top, prefix: prefix}, nil
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

// resolveCommit resolves a revision such as HEAD~1, a branch, a tag, or a hash.
func (r *repo) resolveCommit(rev string) (*object.Commit, error) {
	h, err := r.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, errAmbiguous(rev)
	}
	c, err := r.CommitObject(*h)
	if err != nil {
		// ResolveRevision may return an annotated tag; peel it.
		if t, terr := r.TagObject(*h); terr == nil {
			return t.Commit()
		}
		return nil, fatalf("%s is not a commit", rev)
	}
	return c, nil
}

func (r *repo) readIndex() (*index.Index, error) {
	return r.Storer.Index()
}

func (r *repo) writeIndex(idx *index.Index) error {
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Name < idx.Entries[j].Name })
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

func (r *repo) localConfig() string { return fsName(path.Join(r.top, ".git", "config")) }

// configValue returns the effective value of key, with local overriding global.
func (r *repo) configValue(key string) string {
	var value string
	for _, file := range []string{r.g.globalConfig(), r.localConfig()} {
		cfg, err := r.g.readConfig(file)
		if err != nil {
			continue
		}
		if v, ok := configGet(cfg, key); ok {
			value = v
		}
	}
	return value
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
