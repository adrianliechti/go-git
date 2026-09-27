# go-git CLI

A subset of the `git` command line, implemented in Go on top of
[go-git](https://github.com/go-git/go-git). It runs entirely in-process against
a pluggable filesystem: no host `git`, no child processes, no network.

It was built for [go-bash](https://github.com/adrianliechti/go-bash), where it
is registered as a virtual `git` command, but it works with any writable
filesystem.

```go
code, err := git.Run(ctx, git.Options{
    Args:   []string{"log", "--oneline"},
    Dir:    "/work",           // working directory inside FS
    Env:    env,               // GIT_AUTHOR_*, GIT_COMMITTER_*, HOME, ...
    Stdout: os.Stdout,
    Stderr: os.Stderr,
    FS:     fsys,              // git.FS: fs.FS plus OpenFile/Mkdir/Remove/Rename
})
```

## Sandbox

`Run` never touches the host filesystem: every read and write goes through
`Options.FS`, and environment variables come only from `Options.Env`. The
global config is `$GIT_CONFIG_GLOBAL` or `$HOME/.gitconfig` resolved inside
the `FS`; system config, the host's home directory, and child processes
(editor, pager, hooks, ssh) are never used. go-git's `Plain*` functions,
config-scope loaders, and `Remote` type read host files, so this package only
uses APIs with explicit storage and implements remotes itself.
`sandbox_test.go` checks this with an in-memory `FS` and a decoy host config.

Remotes:

- Local paths and `file://` URLs name repositories inside the `FS`; objects
  are copied between storages in-process.
- `http://` and `https://` use go-git's smart HTTP client with
  `Options.HTTPClient` (default `http.DefaultClient`). Set
  `Options.DisableNetwork` to refuse them.
- ssh and other transports are refused.

`git.OpenDir(path)` is the one opt-in exception: it provides an `FS` for a host
directory, confined with `os.Root`. `cmd/git` uses it rooted at `/`.

## Command-line tool

`cmd/git` runs the same code against the host filesystem, so it can stand in
for `git` in scripts that stay within the supported subset:

```sh
go build -o bin/git ./cmd/git
PATH=$PWD/bin:$PATH git init -q demo && cd demo && git status
```

## Compatibility

`compat_test.go` runs each scenario twice with `/bin/sh`: once with the real
`git` and once with `cmd/git` first on `PATH`. Each command is echoed with its
merged stdout/stderr and exit status, and the two transcripts must be
identical, including commit and tree hashes. The scenarios cover commits,
status (long, short, porcelain, ignored), diffs and stats, renames, path
quoting, branches and upstream tracking, fast-forward and three-way merges
with conflicts, cherry-pick, revert, reset/restore,
mv, rm, clean, tags, decorations, subdirectories, log formats, ignore rules,
config, amend, clone/fetch/push/pull between local and bare repositories, and
error messages. They were written against git 2.54. `http_test.go` clones,
pushes, and pulls over smart HTTP against the real `git http-backend`.

```sh
go test ./...
```

Supported commands: `init`, `clone`, `add`, `mv`, `rm`, `restore`, `clean`,
`commit`, `status`, `log`, `show`, `diff`, `branch`, `checkout`, `switch`,
`reset`, `merge`, `merge-base`, `cherry-pick`, `revert`, `tag`, `remote`,
`fetch`, `pull`, `push`, `config`,
`rev-parse`, `cat-file`, `ls-files`, `hash-object`, `help`, `version`. Only the
common options of each are implemented; `git help <command>` lists them.

Known differences from git:

- Merges follow merge-ort's per-path rules and xdiff's conflict output, but
  without rename detection across branches or recursive merge bases.
- No `stash`, `rebase`, `blame`, `grep`, reflog, or `log --graph` yet.
- No editor, pager, hooks, or colors; `-m`/`-F` are required for messages.
- Diffs use Myers with git's slide-down compaction but without the indent
  heuristic, so hunk placement can differ for indented code.
- No symlinks; short hashes are always 7 characters.
- `-c name=value` is accepted and ignored.
