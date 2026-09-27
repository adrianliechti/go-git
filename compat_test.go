package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The compatibility tests run each scenario twice with /bin/sh: once with the
// host's real git and once with cmd/git first on PATH. Every line is echoed
// with its merged output and exit status, and the transcripts must match
// exactly. Fixed identities and dates make commit hashes comparable.

var ourGit string // directory containing the built cmd/git binary

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "go-git-bin")
	if err != nil {
		panic(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "git"), "./cmd/git")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	ourGit = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type scenario struct {
	name   string
	script string
}

var scenarios = []scenario{
	{"init and first commit", `
git init -q -b main
git status
echo hi > a.txt
mkdir -p d/e
echo x > d/e/f
git status
git status -s
git status --porcelain
git add a.txt
git status
git commit -m first
git commit --allow-empty -m empty
git log
git log --oneline
git cat-file -p HEAD
git cat-file -p HEAD^{tree}
git cat-file -t HEAD
git cat-file -s HEAD:a.txt
git ls-files
git rev-parse HEAD
git rev-parse --short HEAD
git rev-parse --abbrev-ref HEAD
git rev-parse HEAD~1
`},
	{"staging and diffs", `
git init -q -b main
printf 'one\ntwo\nthree\n' > a
echo keep > b
git add .
git commit -q -m base
echo four >> a
echo new > c
git add c
git status
git status -s
git diff
git diff --cached
git diff --stat
git diff --name-only
git diff --name-status HEAD
git diff --numstat HEAD
git diff --quiet
git diff --exit-code --cached
git commit -am second
git show --stat
git show
git diff HEAD~1 HEAD
git diff HEAD~1..HEAD --stat
`},
	{"diff shapes", `
git init -q -b main
seq 1 30 > n
printf 'no newline' > nl
touch empty
git add .
git commit -q -m base
sed -i.bak -e 's/^5$/five/' -e 's/^25$/twenty-five/' n
rm n.bak
printf 'no newline changed' > nl
rm empty
git diff
git diff --stat
git add -A
git commit -q -m changes
git log -p -1
git log --stat --oneline
`},
	{"funcname hunk headers", `
git init -q -b main
printf 'header\n1\n2\n3\n4\n5\n6\n7\n8\n9\n' > f
git add f
git commit -q -m base
printf 'header\n1\n2\n3\n4\n5\n6\n7\nchanged\n9\n' > f
git diff
`},
	{"branches and fast-forward merge", `
git init -q -b main
echo a > a
git add a
git commit -q -m a
git branch feature
git branch
git checkout feature
echo b > b
git add b
git commit -q -m b
git checkout main
git merge feature
git merge feature
git log --oneline
git checkout -b other
git switch main
git branch --show-current
echo c > c
git add c
git commit -q -m c
git checkout other
git merge main
git checkout -q main
git branch -d other
git switch -c topic
echo t > t
git add t
git commit -q -m t
git switch main
git branch -d topic
git branch -D topic
git branch
`},
	{"switching with local changes", `
git init -q -b main
echo 1 > f
echo 1 > g
git add .
git commit -q -m one
git checkout -q -b side
echo 2 > f
git commit -qam two
git checkout -q main
echo local > f
git checkout side
git status -s
git checkout -- f
echo local > g
git checkout side
git status -s
git diff
`},
	{"reset and restore", `
git init -q -b main
echo 1 > f
git add f
git commit -q -m one
echo 2 > f
git commit -qam two
echo 3 > f
git add f
git reset
git status -s
git add f
git reset -q HEAD f
git status -s
git restore f
git status -s
echo 4 > f
git add f
git restore --staged f
git status -s
git checkout -- f
git reset --soft HEAD~1
git status -s
git reset --hard HEAD
git reset --hard HEAD~0
cat f
git log --oneline
`},
	{"removing files", `
git init -q -b main
mkdir dir
echo a > dir/a
echo b > dir/b
echo c > c
git add .
git commit -q -m files
git rm dir
git rm -r dir
git status -s
ls
git reset -q --hard
echo changed > c
git rm c
git rm --cached c
git status -s
git rm missing
git commit -q -m rm
git ls-files
`},
	{"tags", `
git init -q -b main
echo a > a
git add a
git commit -q -m a
git tag v1
git tag -a v2 -m "release two"
git tag
git tag -l "v1*"
git rev-parse v1
git rev-parse v2
git cat-file -t v2
git cat-file -p v2
git show v2 --stat
git tag v1
git tag -d v1
git tag
`},
	{"subdirectories", `
git init -q -b main
mkdir -p src/lib
echo a > src/lib/a
echo t > top
git add .
git commit -q -m tree
cd src && echo b >> lib/a && echo n > new && git status
cd src && git status -s
cd src && git status --porcelain
cd src && git ls-files
cd src/lib && git ls-files
cd src && git rev-parse --show-prefix --show-cdup --is-inside-work-tree
cd src && git add . && git status -s
cd src && git diff --cached --stat
`},
	{"log formats", `
git init -q -b main
echo 1 > a
git add a
printf 'subject line\n\nbody line\n  indented  \n\n\nmore\n\n' > ../msg
GIT_AUTHOR_DATE='@1700000100 +0200' git commit -q -F ../msg
echo 2 > b
git add b
git commit -q -m second
echo 3 >> a
git commit -qam third
git log
git log --format="%H|%h|%T|%t|%P|%p|%an|%ae|%ad|%ai|%aI|%at"
git log --format="%cn %ce %cd %ct"
git log --format="[%s|%b]"
git log --format=%B -1 HEAD~2
git log --pretty=format:%h
git log --pretty=oneline
git log -n 2 --oneline
git log -1 --oneline
git log --oneline --reverse
git log --oneline -- b
git log --oneline HEAD~2..HEAD
git log --oneline HEAD~1..
git show -s --format=%s HEAD~1
git show HEAD~1 --format=%s
git show HEAD:a
`},
	{"ignore rules", `
git init -q -b main
echo "*.log" > .gitignore
echo "build/" >> .gitignore
echo keep > keep.txt
echo noise > app.log
mkdir build
echo out > build/out
git status -s
git add app.log
git add .
git status -s
git add -f app.log
git status -s
`},
	{"config", `
git init -q -b main
git config user.name
git config user.name "Local Name"
git config user.email local@example.com
git config user.name
git config --get user.email
git config --unset user.email
git config user.email
echo a > a
git add a
env -u GIT_AUTHOR_NAME -u GIT_COMMITTER_NAME git commit -q -m configured
git log --format="%an <%ae> / %cn"
`},
	{"errors", `
git status
git log
git init -q -b main
git log
git commit -m nothing
echo a > a
git add a
git commit -q -m "   "
git commit -q -m ok
git commit -m again
git show nope
git rev-parse --verify nope
git rev-parse --verify -q nope
git checkout nope
git add nope
git branch -d main
git branch main
git frobnicate
`},
	{"amend", `
git init -q -b main
echo a > a
git add a
git commit -q -m first
echo b > b
git add b
GIT_COMMITTER_DATE='@1700000500 +0000' git commit -q --amend --no-edit
git log --format="%h %s %ad %cd"
git commit -q --amend -m renamed
git log --stat
`},
	{"hash-object and nested paths", `
git init -q -b main
echo content > f
git hash-object f
echo stdin | git hash-object --stdin
git hash-object -w f
git cat-file -p d95f3ad14dee633a758d2e331151e950dd13e4ed
mkdir -p a/b/c
echo deep > a/b/c/d
echo sib > a/b-c
git add .
git commit -q -m nested
git ls-files -s
git cat-file -p HEAD^{tree}
git cat-file -p HEAD:a
`},
	{"path quoting", `
git init -q -b main
mkdir d
echo a > d/a
echo sp > "with space"
echo u > "\303\274mlaut"
printf 't\n' > "tab	x"
echo q > 'q"uote'
git status -s
git status
git add .
git status -s
git status --porcelain
git ls-files
git commit -q -m init
git show --stat --format=%s
git show --name-only --format=%s
echo more >> 'q"uote'
git diff
git diff --numstat
`},
	{"decorations", `
git init -q -b main
echo a > a
git add a
git commit -q -m one
echo b > b
git add b
git commit -q -m two
git log --oneline --decorate
git tag v1
git tag -a v2 -m annotated HEAD~1
git branch other
git branch zeta HEAD~1
git log --oneline --decorate
git log --decorate -1
git log --format="%h%d|%D"
git checkout -q --detach
git log --oneline --decorate
git checkout -q main
git log --oneline
`},
	{"mv", `
git init -q -b main
mkdir d e
echo a > d/a
echo b > b
echo c > c
echo t > e/t
git add .
git commit -q -m init
git mv b b2
git mv nope x
git mv b2 c
git mv -f b2 c
git mv d/a e
git mv e/a missing/dir/a
git mv -n c e
git mv -v c e
echo u > untracked
git mv untracked x
git mv -k untracked c2 e
git ls-files
git commit -q -m moved
git show --stat --format=%s
`},
	{"clean", `
git init -q -b main
mkdir tracked
echo t > tracked/t
echo "*.log" > .gitignore
git add .
git commit -q -m init
echo x > untracked
echo y > tracked/new
mkdir -p ud/sub
echo z > ud/sub/f
echo l > app.log
mkdir logs
echo l > logs/a.log
git clean
git clean -n
git clean -nd
git clean -ndx
git clean -nX
git clean -ndX
git clean -n tracked
git clean -f
git status -s
git clean -fdq
git status -s --ignored
git clean -fdx
ls
`},
	{"renames", `
git init -q -b main
seq 1 40 > numbers
printf 'alpha\nbeta\ngamma\n' > greek
mkdir -p src/lib
echo code > src/lib/code.go
touch empty
cp numbers numbers.copy
git add .
git commit -q -m init
git mv numbers counted
git mv src/lib src/pkg
git mv greek letters
echo delta >> letters
git mv empty void
git status
git status -s
git diff --cached --stat
git diff --cached --name-status
git diff --cached --numstat
git diff --cached
git diff --cached --no-renames --stat
git commit -m renamed
git show --stat --format=%s
git log --oneline --name-status -1
sed 's/^1[0-9]$/x/' counted > tmp
mv tmp counted
git rm -q numbers.copy
git add counted
git diff --cached --stat
git diff --cached -M --name-status
`},
}

func TestCompatibility(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("real git not found on PATH")
	}
	realDir := filepath.Dir(realGit)
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			want := transcript(t, realDir, sc.script)
			got := transcript(t, ourGit, sc.script)
			if got != want {
				t.Errorf("transcripts differ\n%s", lineDiff(want, got))
			}
		})
	}
}

// transcript runs script line by line in a fresh repository directory with
// gitDir first on PATH.
func transcript(t *testing.T, gitDir, script string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var sh strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
		quoted := "'" + strings.ReplaceAll("$ "+line, "'", `'\''`) + "'"
		// Each line runs in a subshell so "cd" does not leak.
		sh.WriteString("printf '%s\\n' " + quoted + "\n(" + line + ") 2>&1; echo \"[exit $?]\"\n")
	}
	cmd := exec.Command("/bin/sh", "-c", sh.String())
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + gitDir + ":/usr/bin:/bin",
		"HOME=" + dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"TZ=UTC", "LC_ALL=C",
		"GIT_AUTHOR_NAME=Ada Author", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Cody Committer", "GIT_COMMITTER_EMAIL=cody@example.com",
		"GIT_AUTHOR_DATE=@1700000000 +0100", "GIT_COMMITTER_DATE=@1700000000 +0000",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
	return strings.ReplaceAll(string(out), dir, "$ROOT")
}

// lineDiff shows the first differing lines with some context.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	start := max(i-6, 0)
	var b strings.Builder
	b.WriteString("--- real git\n")
	for _, l := range w[start:min(i+12, len(w))] {
		b.WriteString("  " + l + "\n")
	}
	b.WriteString("--- go-git\n")
	for _, l := range g[start:min(i+12, len(g))] {
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}
