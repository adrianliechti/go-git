package git

import (
	"fmt"
	"sort"
	"strings"
)

const mainUsage = `usage: git [-v | --version] [-h | --help] [-C <path>] [-c <name>=<value>]
           [-P | --no-pager] <command> [<args>]
`

// helpGroups mirrors "git help" but lists only the implemented commands.
var helpGroups = []struct {
	title    string
	commands []string
}{
	{"start a working area", []string{"clone", "init"}},
	{"work on the current change", []string{"add", "mv", "restore", "rm", "stash"}},
	{"examine the history and state", []string{"bisect", "diff", "grep", "log", "show", "status"}},
	{"grow, mark and tweak your common history", []string{"branch", "cherry-pick", "commit", "merge", "rebase", "reset", "revert", "switch", "tag"}},
	{"collaborate", []string{"fetch", "pull", "push"}},
}

type commandHelp struct {
	summary string
	usage   []string
	options [][2]string
}

var help = map[string]commandHelp{
	"add": {"Add file contents to the index",
		[]string{"git add [<options>] [--] <pathspec>..."},
		[][2]string{{"-f, --force", "allow adding otherwise ignored files"}, {"-u, --update", "update tracked files"}, {"-A, --all", "add changes from all tracked and untracked files"}}},
	"branch": {"List, create, or delete branches",
		[]string{"git branch [<options>] [-l] [<pattern>...]", "git branch [<options>] <branch-name> [<start-point>]", "git branch [<options>] (-d | -D) <branch-name>...", "git branch --show-current"},
		[][2]string{{"-d, --delete", "delete fully merged branch"}, {"-D", "delete branch (even if not merged)"}, {"--show-current", "show current branch name"}}},
	"cat-file": {"Provide contents or details of repository objects",
		[]string{"git cat-file (-t | -s | -e | -p) <object>", "git cat-file <type> <object>"}, nil},
	"checkout": {"Switch branches or restore working tree files",
		[]string{"git checkout [<options>] <branch>", "git checkout [<options>] [<branch>] -- <file>..."},
		[][2]string{{"-b <branch>", "create and checkout a new branch"}, {"-q, --quiet", "suppress progress reporting"}, {"--detach", "detach HEAD at named commit"}, {"-f, --force", "force checkout (throw away local modifications)"}}},
	"clone": {"Clone a repository into a new directory",
		[]string{"git clone [<options>] [--] <repo> [<dir>]"},
		[][2]string{{"-q, --quiet", "be more quiet"}, {"--bare", "create a bare repository"}, {"-b, --branch <branch>", "checkout <branch> instead of the remote's HEAD"}, {"-o, --origin <name>", "use <name> instead of 'origin' to track upstream"}}},
	"commit": {"Record changes to the repository",
		[]string{"git commit [-a | --all] [-q] [--amend] [--allow-empty] (-m <msg> | -F <file>)"},
		[][2]string{{"-m, --message <message>", "commit message"}, {"-F, --file <file>", "read message from file"}, {"-a, --all", "commit all changed files"}, {"--amend", "amend previous commit"}, {"--no-edit", "use the previous message when amending"}, {"--allow-empty", "allow an empty commit"}}},
	"config": {"Get and set repository or global options",
		[]string{"git config [--global] <name> [<value>]", "git config [--global] --unset <name>", "git config [--global] --list"}, nil},
	"diff": {"Show changes between commits, commit and working tree, etc",
		[]string{"git diff [<options>] [<commit>] [--] [<path>...]", "git diff [<options>] --cached [<commit>] [--] [<path>...]", "git diff [<options>] <commit> <commit> [--] [<path>...]"},
		[][2]string{{"--cached, --staged", "compare the index with a commit"}, {"--stat", "show a diffstat"}, {"--name-only", "show only names of changed files"}, {"--name-status", "show names and status of changed files"}, {"--numstat", "show numbers of added and deleted lines"}, {"--quiet", "disable all output; exit 1 if there were differences"}, {"--exit-code", "exit with 1 if there were differences"}}},
	"fetch": {"Download objects and refs from another repository",
		[]string{"git fetch [<options>] [<repository> [<refspec>...]]"},
		[][2]string{{"-q, --quiet", "be more quiet"}, {"--all", "fetch from all remotes"}, {"-p, --prune", "prune remote-tracking branches no longer on remote"}}},
	"hash-object": {"Compute object ID and optionally create an object from a file",
		[]string{"git hash-object [-w] [--stdin] [--] <file>..."}, nil},
	"init": {"Create an empty Git repository or reinitialize an existing one",
		[]string{"git init [-q | --quiet] [--bare] [-b <branch-name> | --initial-branch=<branch-name>] [<directory>]"}, nil},
	"log": {"Show commit logs",
		[]string{"git log [<options>] [<revision-range>] [[--] <path>...]"},
		[][2]string{{"-n <number>", "limit the number of commits"}, {"--oneline", "one line per commit"}, {"--format=<format>", "pretty-print with a format string"}, {"--stat", "show a diffstat"}, {"-p, --patch", "show patches"}, {"--reverse", "output in reverse order"}, {"--decorate", "show ref names"}}},
	"ls-files": {"Show information about files in the index",
		[]string{"git ls-files [-s | --stage] [--] [<file>...]"}, nil},
	"merge": {"Join two or more development histories together",
		[]string{"git merge [<options>] <commit>", "git merge --abort", "git merge --continue"},
		[][2]string{{"--ff-only", "abort if fast-forward is not possible"}, {"--no-ff", "always create a merge commit"}, {"--squash", "create a single commit instead of doing a merge"}, {"--no-commit", "perform the merge but do not commit"}, {"-m <message>", "merge commit message"}, {"-q, --quiet", "be more quiet"}}},
	"mv": {"Move or rename a file, a directory, or a symlink",
		[]string{"git mv [<options>] <source>... <destination>"},
		[][2]string{{"-f, --force", "force move/rename even if target exists"}, {"-k", "skip move/rename errors"}, {"-n, --dry-run", "dry run"}}},
	"pull": {"Fetch from and integrate with another repository or a local branch",
		[]string{"git pull [<options>] [<repository> [<refspec>...]]"},
		[][2]string{{"--ff-only", "abort if fast-forward is not possible"}, {"-q, --quiet", "be more quiet"}}},
	"push": {"Update remote refs along with associated objects",
		[]string{"git push [<options>] [<repository> [<refspec>...]]"},
		[][2]string{{"-u, --set-upstream", "set upstream for git pull/status"}, {"-f, --force", "force updates"}, {"--tags", "push tags"}, {"-d, --delete", "delete refs"}, {"-q, --quiet", "be more quiet"}}},
	"remote": {"Manage set of tracked repositories",
		[]string{"git remote [-v | --verbose]", "git remote add <name> <url>", "git remote remove <name>", "git remote rename <old> <new>", "git remote get-url <name>", "git remote set-url <name> <newurl>"}, nil},
	"reset": {"Reset current HEAD to the specified state",
		[]string{"git reset [-q] [<tree-ish>] [--] <pathspec>...", "git reset [--mixed | --soft | --hard] [-q] [<commit>]"}, nil},
	"restore": {"Restore working tree files",
		[]string{"git restore [<options>] [--source=<branch>] <file>..."},
		[][2]string{{"-S, --staged", "restore the index"}, {"-W, --worktree", "restore the working tree (default)"}, {"--source=<tree-ish>", "which tree-ish to checkout from"}}},
	"rev-parse": {"Pick out and massage parameters",
		[]string{"git rev-parse [<options>] <args>..."}, nil},
	"rm": {"Remove files from the working tree and from the index",
		[]string{"git rm [-f | --force] [-r] [--cached] [-q] [--] <pathspec>..."}, nil},
	"show": {"Show various types of objects",
		[]string{"git show [<options>] <object>..."}, nil},
	"status": {"Show the working tree status",
		[]string{"git status [<options>] [--] [<pathspec>...]"},
		[][2]string{{"-s, --short", "show status concisely"}, {"-b, --branch", "show branch information"}, {"--porcelain", "machine-readable output"}, {"-u[<mode>]", "show untracked files (no, normal, all)"}}},
	"switch": {"Switch branches",
		[]string{"git switch [<options>] <branch>", "git switch -c <new-branch> [<start-point>]"},
		[][2]string{{"-c, --create <branch>", "create and switch to a new branch"}, {"-d, --detach", "detach HEAD at named commit"}}},
	"tag": {"Create, list, delete or verify tags",
		[]string{"git tag [-a] [-m <msg>] <tagname> [<commit>]", "git tag -d <tagname>...", "git tag [-l] [<pattern>...]"}, nil},
	"clean": {"Remove untracked files from the working tree",
		[]string{"git clean [-d] [-f] [-n] [-x | -X] [--] [<pathspec>...]"},
		[][2]string{{"-n, --dry-run", "dry run"}, {"-f, --force", "force"}, {"-d", "remove whole directories"}, {"-x", "remove ignored files, too"}, {"-X", "remove only ignored files"}}},
	"version":       {"Display version information about Git", []string{"git version"}, nil},
	"ls-tree":       {"List the contents of a tree object", []string{"git ls-tree [-d] [-r] [-t] [-l] [--name-only] <tree-ish> [<path>...]"}, nil},
	"show-ref":      {"List references in a local repository", []string{"git show-ref [--head] [-d] [-s] [--heads] [--tags] [<pattern>...]", "git show-ref --verify [-q] [-s] <ref>..."}, nil},
	"for-each-ref":  {"Output information on each ref", []string{"git for-each-ref [--count=<count>] [--sort=<key>]... [--format=<format>] [<pattern>...]"}, nil},
	"rev-list":      {"Lists commit objects in reverse chronological order", []string{"git rev-list [<options>] <commit>... [--] [<path>...]"}, nil},
	"update-ref":    {"Update the object name stored in a ref safely", []string{"git update-ref [-m <reason>] [--no-deref] (-d <ref> [<old-oid>] | <ref> <new-oid> [<old-oid>])"}, nil},
	"symbolic-ref":  {"Read, modify and delete symbolic refs", []string{"git symbolic-ref [-m <reason>] <name> <ref>", "git symbolic-ref [-q] [--short] <name>", "git symbolic-ref --delete [-q] <name>"}, nil},
	"write-tree":    {"Create a tree object from the current index", []string{"git write-tree"}, nil},
	"commit-tree":   {"Create a new commit object", []string{"git commit-tree <tree> [(-p <parent>)...] [(-m <message>)...] [(-F <file>)...]"}, nil},
	"read-tree":     {"Reads tree information into the index", []string{"git read-tree [--prefix=<prefix>] [-u] (--empty | <tree-ish>)"}, nil},
	"update-index":  {"Register file contents in the working tree to the index", []string{"git update-index [--add] [--remove | --force-remove] [--chmod=(+|-)x] [--cacheinfo <mode>,<object>,<path>]... [--] [<file>...]"}, nil},
	"diff-tree":     {"Compares the content and mode of blobs found via two tree objects", []string{"git diff-tree [--no-commit-id] [-r] [-t] [-p] [--root] [--name-only | --name-status] <tree-ish> [<tree-ish>]"}, nil},
	"count-objects": {"Count unpacked number of objects and their disk consumption", []string{"git count-objects [-v]"}, nil},
	"ls-remote":     {"List references in a remote repository", []string{"git ls-remote [--heads] [--tags] [--refs] [-q] [<repository> [<patterns>...]]"}, nil},
	"merge-file":    {"Run a three-way file merge", []string{"git merge-file [-p] [-q] [-L <name1> [-L <orig> [-L <name2>]]] <file1> <orig-file> <file2>"}, nil},
	"describe":      {"Give an object a human readable name based on an available ref", []string{"git describe [--all] [--tags] [--long] [--always] [--abbrev=<n>] [--dirty[=<mark>]] [--match <pattern>] [<commit-ish>...]"}, nil},
	"shortlog":      {"Summarize 'git log' output", []string{"git shortlog [-s] [-n] [-e] [-c] [<revision-range>] [[--] <path>...]"}, nil},
	"grep": {"Print lines matching a pattern",
		[]string{"git grep [-n] [-i] [-c] [-l] [-L] [-w] [-v] [-h] [-q] [-E | -F] [--cached] [-e] <pattern> [<rev>...] [[--] <path>...]"}, nil},
	"format-patch": {"Prepare patches for e-mail submission",
		[]string{"git format-patch [-o <dir>] [--stdout] [-n | -N] [--no-signature] [--subject-prefix=<prefix>] [-<n>] [<since> | <revision-range>]"}, nil},
	"apply": {"Apply a patch to files and/or to the index",
		[]string{"git apply [--stat] [--numstat] [--summary] [--check] [--index | --cached] [-R] [-p<n>] [<patch>...]"}, nil},
	"am": {"Apply a series of patches from a mailbox",
		[]string{"git am [-q] [<mbox>...]", "git am (--continue | --skip | --abort | --show-current-patch)"}, nil},
	"worktree": {"Manage multiple working trees",
		[]string{"git worktree add [-f] [--detach] [--lock] [-b <new-branch>] <path> [<commit-ish>]",
			"git worktree list [--porcelain]", "git worktree lock [--reason <string>] <worktree>",
			"git worktree move <worktree> <new-path>", "git worktree prune",
			"git worktree remove [-f] <worktree>", "git worktree unlock <worktree>"}, nil},
	"bisect": {"Use binary search to find the commit that introduced a bug",
		[]string{"git bisect start [--term-(bad|new)=<term>] [--term-(good|old)=<term>] [<bad> [<good>...]]",
			"git bisect (bad|new|<term-new>) [<rev>]", "git bisect (good|old|<term-old>) [<rev>...]",
			"git bisect skip [<rev>...]", "git bisect reset [<commit>]", "git bisect (log|terms|visualize)"}, nil},
	"blame": {"Show what revision and author last modified each line of a file",
		[]string{"git blame [-s] [-e] [-l] [-t] [-L <start>,<end>] [<rev>] [--] <file>"}, nil},

	"stash": {"Stash the changes in a dirty working directory away",
		[]string{"git stash [push [-k] [-u | -a] [-q] [-m <message>] [--] [<pathspec>...]]", "git stash list",
			"git stash show [-p] [-u] [<stash>]", "git stash (pop | apply) [--index] [-q] [<stash>]",
			"git stash drop [-q] [<stash>]", "git stash branch <branchname> [<stash>]", "git stash clear"}, nil},
	"rebase": {"Reapply commits on top of another base tip",
		[]string{"git rebase [-q] [--onto <newbase>] [<upstream> [<branch>]]", "git rebase (--continue | --skip | --abort)"}, nil},
	"reflog": {"Manage reflog information",
		[]string{"git reflog [show] [<log-options>] [<ref>]", "git reflog exists <ref>"}, nil},
	"merge-base": {"Find as good common ancestors as possible for a merge",
		[]string{"git merge-base <commit> <commit>", "git merge-base --is-ancestor <commit> <commit>"}, nil},
	"cherry-pick": {"Apply the changes introduced by some existing commits",
		[]string{"git cherry-pick [-n] [-x] <commit>...", "git cherry-pick (--continue | --skip | --abort)"},
		[][2]string{{"-n, --no-commit", "don't automatically commit"}, {"-x", "append commit name"}}},
	"revert": {"Revert some existing commits",
		[]string{"git revert [-n] [--no-edit] <commit>...", "git revert (--continue | --skip | --abort)"},
		[][2]string{{"-n, --no-commit", "don't automatically commit"}}},
}

func (g *gitRun) writeMainHelp() {
	fmt.Fprint(g.out, mainUsage)
	fmt.Fprintln(g.out, "\nThese are the Git commands implemented by this sandboxed git:")
	for _, grp := range helpGroups {
		var lines []string
		for _, c := range grp.commands {
			if _, ok := commands[c]; ok {
				lines = append(lines, fmt.Sprintf("   %-10s %s\n", c, help[c].summary))
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(g.out, "\n%s\n%s", grp.title, strings.Join(lines, ""))
		}
	}
	fmt.Fprint(g.out, "\n'git help -a' lists all available subcommands.\n"+
		"See 'git help <command>' or 'git <command> -h' for a specific subcommand.\n")
}

// commandUsage returns "git <cmd> -h" output, which git prints with exit 129.
func commandUsage(name string) string {
	h, ok := help[name]
	if !ok {
		return fmt.Sprintf("usage: git %s\n", name)
	}
	var b strings.Builder
	for i, u := range h.usage {
		prefix := "   or: "
		if i == 0 {
			prefix = "usage: "
		}
		b.WriteString(prefix + u + "\n")
	}
	if len(h.options) > 0 {
		b.WriteString("\n")
		for _, o := range h.options {
			fmt.Fprintf(&b, "    %-26s%s\n", o[0], o[1])
		}
	}
	b.WriteString("\n")
	return b.String()
}

func (g *gitRun) help(args []string) error {
	switch {
	case len(args) == 0 || args[0] == "-g" || args[0] == "--guides":
		g.writeMainHelp()
	case args[0] == "-a" || args[0] == "--all":
		fmt.Fprintln(g.out, "available git commands")
		fmt.Fprintln(g.out)
		names := make([]string, 0, len(commands))
		for name := range commands {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(g.out, "   %-15s %s\n", name, help[name].summary)
		}
	default:
		if _, ok := commands[args[0]]; !ok {
			return failf(1, "git: '%s' is not a git command. See 'git --help'.\n", args[0])
		}
		// There are no man pages in the sandbox; show the short usage.
		fmt.Fprint(g.out, commandUsage(args[0]))
	}
	return nil
}
