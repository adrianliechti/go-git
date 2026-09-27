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
	{"remotes", `
git init -q --bare -b main server.git
git clone server.git work
cd work && git status
cd work && git remote
cd work && git remote -v
cd work && git remote get-url origin
cd work && git branch -a
cd work && echo a > a && git add a && git commit -q -m a
cd work && git status
cd work && git status -sb
cd work && git push
cd work && git push -u origin main
cd work && git status
cd work && git status -sb
cd work && git branch -vv
cd work && git log --oneline --decorate
git clone server.git other
git clone -q server.git quiet
cd other && git log --oneline --decorate
cd other && git branch -a
cd other && git branch -r
cd other && echo b > b && git add b && git commit -q -m b
cd other && git status -sb
cd other && git push
cd other && git checkout -q -b feature && echo f > f && git add f && git commit -q -m f
cd other && git push origin feature
cd other && git push origin feature:renamed
cd other && git push origin --delete renamed
cd other && git push origin --delete renamed
cd work && git fetch
cd work && git status
cd work && git status -sb
cd work && git branch -vv
cd work && git pull
cd work && git fetch origin
cd work && git branch -a
cd work && git log --oneline --decorate --all
cd work && git checkout feature
cd work && git branch -vv
cd work && git switch -q main
cd work && echo c > c && git add c && git commit -q -m c
cd work && git push
cd other && git checkout -q main
cd other && echo d > d && git add d && git commit -q -m d
cd other && git push
cd other && git pull --ff-only
cd other && git pull
cd other && git status
cd other && git status -sb
cd other && git push --force
cd work && git fetch
cd work && git status -sb
cd other && git tag v1 && git push origin v1
cd other && git push --tags
cd work && git fetch
cd work && git tag
cd other && git remote add second ../server.git
cd other && git remote
cd other && git remote rename second third
cd other && git remote -v
cd other && git remote remove third
cd other && git remote
cd other && git remote remove nope
cd other && git fetch nope
cd other && git remote add origin x
git clone server.git work
git clone missing.git x
git clone --bare server.git mirror.git
cd mirror.git && git log --oneline
cd mirror.git && git branch
cd quiet && git log --oneline
cd quiet && git pull -q
cd quiet && git log --oneline -1
`},
	{"push to a checked-out branch", `
git init -q -b main upstream
cd upstream && echo a > a && git add a && git commit -q -m a
git clone -q upstream down
cd down && echo b > b && git add b && git commit -q -m b
cd down && git push
cd down && git push origin main:other
cd upstream && git branch
cd down && git branch -u origin/other
cd down && git status -sb
cd down && git branch --unset-upstream
cd down && git status -sb
cd down && git branch -m renamed
cd down && git branch
`},
	{"merge conflicts", `
git init -q -b main
printf '1\n2\n3\n4\n5\n6\n7\n8\n9\n' > f
echo keep > k
echo del > d
git add .
git commit -q -m base
git checkout -q -b feature
printf '1\nTWO\n3\n4\n5\n6\n7\nEIGHT\n9\n' > f
echo x > both
echo modified >> d
git add .
git commit -q -m feature
git checkout -q main
printf '1\nzwei\n3\n4\n5\n6\n7\n8\n9\nten\n' > f
echo y > both
git rm -q d
git add .
git commit -q -m main
git merge feature
cat f
cat both
git status
git status -s
git ls-files -s
git commit -m x
git checkout feature
git merge feature
git merge --abort
git status -s
git merge feature
printf '1\nboth\n3\n4\n5\n6\n7\nEIGHT\n9\nten\n' > f
git add f
git status
echo x > both
git add both
git rm -q d
git status
git commit --no-edit
git log --format="%h %p %s"
git merge --abort
`},
	{"clean merges", `
git init -q -b main
printf 'a\nb\nc\nd\ne\nf\ng\nh\n' > f
git add f
git commit -q -m base
git checkout -q -b feature
sed 's/^b$/B/' f > t && mv t f
echo new > n
git add .
git commit -q -m feature
git checkout -q main
sed 's/^g$/G/' f > t && mv t f
git commit -qam main
git merge feature
git log --format="%h %p %s" -3
cat f
git checkout -q -b topic HEAD~1
echo t > t
git add t
git commit -q -m t
git merge main -q
git log -1 --format=%s
git checkout -q main
git merge topic --no-ff -m "custom message"
git log -1 --format="%s %p"
git merge topic
git merge-base main feature
git merge-base --is-ancestor feature main
git merge-base --is-ancestor main feature
`},
	{"merge options", `
git init -q -b main
echo a > a
git add a
git commit -q -m a
git checkout -q -b side
echo s > s
git add s
git commit -q -m s
echo t > t
git add t
git commit -q -m t
git checkout -q main
git merge --squash side
git status -s
cat .git/SQUASH_MSG
git commit -q -m squashed
git log --oneline
git reset -q --hard HEAD~1
echo m > m
git add m
git commit -q -m m
git merge --no-commit side
git status
cat .git/MERGE_HEAD
git commit -q --no-edit
git log --format="%s|%p" -1
git merge --ff-only side
git merge nope
`},
	{"cherry-pick and revert", `
git init -q -b main
printf '1\n2\n3\n' > f
git add .
git commit -q -m base
git checkout -q -b side
echo s > s
git add s
git commit -q -m "add s"
printf '1\nX\n3\n' > f
git commit -qam "change f"
git checkout -q main
GIT_COMMITTER_DATE="@1700000500 +0000" git cherry-pick side~1
git log --format="%h %an %ad %cd %s" -1
git cherry-pick side~1
git cherry-pick --skip
git revert HEAD
git log --format=%B -1
printf '1\nY\n3\n' > f
git commit -qam "main f"
git cherry-pick side
git status
git status -s
cat f
echo resolved > f
git add f
git cherry-pick --continue
git log --oneline -1
git revert HEAD~1 --no-edit
cat f
git status -s
git revert --abort
git status -s
git cherry-pick -x side~1
git log --format=%B -1
git cherry-pick --continue
`},
	{"log graph", `
git init -q -b main
echo 1 > a
git add a
git commit -q -m one
git log --graph --oneline
echo 2 >> a
git commit -qam two
git checkout -q -b feature
echo f > f
git add f
git commit -q -m f1
echo f >> f
git commit -qam f2
git checkout -q main
echo 3 >> a
git commit -qam three
git log --graph --oneline --all
git merge -q feature -m "merge feature"
git log --graph --oneline
git log --graph --oneline --decorate --all
git checkout -q -b other HEAD~2
echo o > o
git add o
git commit -q -m o1
git checkout -q -b third main~1
echo t > t
git add t
git commit -q -m t1
git log --graph --oneline --all
git checkout -q main
git merge -q other -m "merge other"
git merge -q third -m "merge third"
git log --graph --oneline
git log --graph --format="%h %s"
git log --graph -2
git log --graph --stat -2
git log --graph --oneline --stat -3
git log --graph --oneline -3
git log --topo-order --oneline
git log --graph --reverse
`},
	{"log options", `
git init -q -b main
echo a > a
git add a
git commit -q -m "first subject" -m "first body"
echo b > b
git add b
GIT_AUTHOR_NAME=Bob GIT_AUTHOR_EMAIL=bob@example.com GIT_AUTHOR_DATE="@1700100000 +0000" git commit -q -m "add b (fixes #12)"
echo aa >> a
GIT_COMMITTER_DATE="@1700200000 +0200" git commit -qam "grow a"
git mv b c
git commit -q -m "rename b"
echo cc >> c
git commit -qam "grow c"
for p in short full fuller raw reference email; do git log -1 --pretty=$p; done
git log --format=short -1
git log -1 --abbrev-commit
git log -1 --oneline --abbrev=10
for d in iso iso-strict rfc short raw unix format:%Y/%m/%d; do git log -1 --format=%ad --date=$d; done
git log -2 --date=iso
git log --format="%as|%cs|%aD|%at|%ae|%an|%al|%cn|%ce|%ci|%cI|%f|%x41|%%" -3
git log --oneline --author=Bob
git log --oneline --author=bob
git log --oneline -i --author=bob
git log --oneline --grep=fixes
git log --oneline --grep=a --grep=b
git log --oneline --grep=a --grep=b --all-match
git log --oneline --grep=grow --invert-grep
git log --oneline --since=@1700150000
git log --oneline --until=@1700150000
git log --oneline --skip=2
git log --oneline --skip=1 -2
git log --oneline -S aa
git log --oneline -G "^c"
git log --oneline --no-merges
git log --oneline --merges
git log --oneline c
git log --oneline --follow c
git log --oneline --follow --stat c
git log --oneline nope
git log --shortstat --oneline -2
git log --numstat --oneline -2
git log --summary --oneline
git log --oneline HEAD~3..
git log --oneline ^HEAD~2 HEAD
git log --oneline HEAD --not HEAD~2
git log --oneline --first-parent
`},
	{"revisions and aliases", `
git init -q -b main
echo a > a
git add a
git commit -q -m one
git checkout -q -b side
echo s > s
git add s
git commit -q -m "side work"
git checkout -q main
echo b > b
git add b
git commit -q -m two
git log --oneline main...side
git log --oneline --left-right main...side
git log --format="%m %s" main...side
git diff --stat main...side
git rev-parse @
git rev-parse @~1
git rev-parse ":/side"
git rev-parse "HEAD^{commit}"
git show -s --format=%s ":/one"
git rev-parse "main@{u}"
git -c user.name=Override log -1 --format=%an
git -c alias.lg="log --oneline" lg
git config alias.st "status -s"
git config alias.last "log -1 --format=%s"
echo u > u
git st
git last
git -c alias.nested=st nested
git config alias.loop loop2
git config alias.loop2 loop
git loop
git -c color.ui=never -c foo.bar config --list | grep -v "^core\."
git restore -s HEAD~1 b
git status -s
git restore --source main -- b
git status -s
`},
	{"reflog", `
git init -q -b main
echo a > a
git add a
git commit -q -m one
echo b >> a
git commit -qam two
git commit -q --amend -m "two amended"
git checkout -q -b feature
echo f > f
git add f
git commit -qm feat
git checkout -q main
git merge -q feature
git reset -q --hard HEAD~1
git checkout -q HEAD~1
git checkout -q main
git branch topic
git branch -m topic renamed
git switch -q feature
git switch -q -
git cherry-pick feature
git revert --no-edit HEAD
git reflog
git reflog show main
git reflog show feature
git reflog renamed
cat .git/logs/HEAD
git rev-parse HEAD@{2} main@{1} @{1} @{-1}
git rev-parse HEAD@{99}
git log -g --oneline -3
git log -g -1
git log -g --format="%h %gd %gs" -2
git checkout -q -
git branch --show-current
git checkout -
git reflog exists main
git reflog exists nope
git branch -D renamed
git reflog renamed
`},
	{"remote reflogs", `
git init -q -b main src
cd src && echo a > a && git add a && git commit -qm one
git clone -q src dst
cd dst && git reflog
cd dst && git reflog show origin/HEAD
cd src && echo b >> a && git commit -qam two && git checkout -qb side && git commit -q --allow-empty -m s && git checkout -q main
cd dst && git fetch -q
cd dst && git reflog show origin/main
cd dst && git reflog show origin/side
cd src && git commit -q --allow-empty -m three
cd dst && git pull -q
cd dst && git reflog -2
cd dst && git commit -q --allow-empty -m mine && git push -q origin main:pushed
cd dst && git reflog show origin/pushed
cd src && git commit -q --amend --allow-empty -m three2
cd dst && git fetch
cd dst && git reflog show origin/main -1
`},
	{"stash", `
git init -q -b main
printf '1\n2\n3\n' > a
echo b > b
git add .
git commit -q -m one
git stash
echo x >> a
echo new > n
git add n
echo u > u
git stash
git status -s
git stash list
git stash show
git stash show -p
git cat-file -p stash@{0}
git log --format="%h %p %s" -3 stash
git stash apply
git status -s
git reset -q --hard
git stash pop
git stash list
git stash push -m "my message" a
git stash list
git status -s
git stash -u
git status -s
git stash list
git log --format="%h %p %s" -1 stash@{0}^3
git stash show --include-untracked
git stash pop stash@{1}
git stash drop
git stash list
git stash pop
git stash branch newbranch
echo y >> a
git stash -q
echo conflict > a
git commit -qam "change a"
git stash pop
git status -s
git stash list
git checkout -q -- a
git reset -q --hard
git stash branch fromstash
git status -s
git stash list
echo k >> b
git add b
echo w >> a
git stash --keep-index
git status -s
git stash show -p
git stash clear
git stash list
`},
	{"rebase", `
git init -q -b main
printf '1\n2\n3\n' > f
git add f
git commit -qm base
git checkout -qb feature
echo a > a
git add a
git commit -qm "add a"
echo b > b
git add b
git commit -qm "add b"
git checkout -q main
echo m > m
git add m
git commit -qm "add m"
git rebase main
git rebase main feature
git checkout -q feature
git rebase main
git log --oneline --graph --all
git reflog -6
git checkout -q main
printf '1\nMAIN\n3\n' > f
git commit -qam "main f"
git checkout -q feature
printf '1\nFEAT\n3\n' > f
git commit -qam "feat f"
echo c > c
git add c
git commit -qm "add c"
git rebase main
git status
git status -s
git branch
cat .git/rebase-merge/done .git/rebase-merge/git-rebase-todo
git rebase main
GIT_EDITOR=true git rebase --continue
echo resolved > f
git add f
git status
GIT_EDITOR=true git rebase --continue
git log --oneline -6
git reflog -8
git status
`},
	{"rebase abort, skip, onto", `
git init -q -b main
printf '1\n2\n3\n' > f
git add f
git commit -qm base
git checkout -qb topic
printf '1\nTOPIC\n3\n' > f
git commit -qam "topic f"
echo t > t
git add t
git commit -qm "add t"
git checkout -q main
printf '1\nMAIN\n3\n' > f
git commit -qam "main f"
git checkout -q topic
git rebase main
git rebase --abort
git status -sb
git log --oneline -3
git rebase main
git rebase --skip
git log --oneline -3
git checkout -q -b onto-test main
echo o > o
git add o
git commit -qm "add o"
git rebase --onto HEAD~1 HEAD~1 topic
git log --oneline -3
git rebase --continue
git checkout -q main
git cherry-pick topic
git checkout -q -b dup main~1
git cherry-pick main
echo d > d
git add d
git commit -qm "add d"
git rebase main
git log --oneline -4
`},
	{"pull --rebase", `
git init -q --bare -b main server.git
git clone -q server.git one
cd one && echo a > a && git add a && git commit -qm a && git push -q -u origin main
git clone -q server.git two
cd one && echo b > b && git add b && git commit -qm b && git push -q
cd two && echo c > c && git add c && git commit -qm c
cd two && git pull --rebase
cd two && git log --oneline
cd two && git pull --rebase
cd two && git config pull.rebase true && git push -q
cd one && echo d > d && git add d && git commit -qm d && git pull
cd one && git log --oneline
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
				// GOGIT_DUMP=<dir> keeps both transcripts for diffing.
				if dir := os.Getenv("GOGIT_DUMP"); dir != "" {
					name := strings.ReplaceAll(sc.name, " ", "_")
					os.WriteFile(filepath.Join(dir, name+".want"), []byte(want), 0644)
					os.WriteFile(filepath.Join(dir, name+".got"), []byte(got), 0644)
				}
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
	for _, l := range w[start:min(i+25, len(w))] {
		b.WriteString("  " + l + "$\n") // "$" marks the line end
	}
	b.WriteString("--- go-git\n")
	for _, l := range g[start:min(i+25, len(g))] {
		b.WriteString("  " + l + "$\n")
	}
	return b.String()
}
