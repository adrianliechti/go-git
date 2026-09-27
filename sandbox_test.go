package git_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/adrianliechti/go-git"
)

// TestSandbox runs git against an in-memory FS while the host environment
// points at a directory with its own git config. Nothing may be read from or
// written to the host: identity comes only from opts.Env and FS.
func TestSandbox(t *testing.T) {
	host := t.TempDir()
	hostConfig := "[user]\n\tname = Host User\n\temail = host@example.com\n[init]\n\tdefaultBranch = host\n"
	if err := os.WriteFile(filepath.Join(host, ".gitconfig"), []byte(hostConfig), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", host)
	t.Setenv("XDG_CONFIG_HOME", host)
	t.Setenv("GIT_AUTHOR_NAME", "Host Env")
	t.Setenv("GIT_AUTHOR_EMAIL", "host-env@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Host Env")
	t.Setenv("GIT_COMMITTER_EMAIL", "host-env@example.com")
	cwd, _ := os.Getwd()
	t.Chdir(host)

	mem := newMemFS()
	run := func(env map[string]string, args ...string) (int, string) {
		var out strings.Builder
		code, err := git.Run(context.Background(), git.Options{
			Args: args, Dir: "/", Env: env, Stdout: &out, Stderr: &out, FS: mem,
		})
		if err != nil {
			t.Fatal(err)
		}
		return code, out.String()
	}
	if err := mem.Mkdir("repo", 0777); err != nil {
		t.Fatal(err)
	}
	if code, out := run(nil, "init", "repo"); code != 0 || out != "Initialized empty Git repository in /repo/.git/\n" {
		t.Fatalf("init: %d %q", code, out)
	}
	f, _ := mem.OpenFile("repo/a", os.O_WRONLY|os.O_CREATE, 0666)
	f.Write([]byte("a\n"))
	f.Close()
	if code, out := run(nil, "-C", "repo", "add", "a"); code != 0 {
		t.Fatalf("add: %d %q", code, out)
	}

	// The host config and environment must be invisible.
	code, out := run(map[string]string{"HOME": "/"}, "-C", "repo", "commit", "-q", "-m", "x")
	if code != 128 || !strings.Contains(out, "Author identity unknown") {
		t.Fatalf("commit without identity: %d %q", code, out)
	}
	if code, out := run(nil, "-C", "repo", "branch", "--show-current"); out != "master\n" {
		t.Fatalf("default branch leaked from host config: %d %q", code, out)
	}

	// A global config inside FS is honored.
	f, _ = mem.OpenFile(".gitconfig", os.O_WRONLY|os.O_CREATE, 0666)
	f.Write([]byte("[user]\n\tname = Sandbox\n\temail = sandbox@example.com\n"))
	f.Close()
	if code, out := run(map[string]string{"HOME": "/"}, "-C", "repo", "commit", "-q", "-m", "x"); code != 0 {
		t.Fatalf("commit: %d %q", code, out)
	}
	if _, out := run(nil, "-C", "repo", "log", "--format=%an <%ae>"); out != "Sandbox <sandbox@example.com>\n" {
		t.Fatalf("log: %q", out)
	}

	// An alternates file cannot reach host objects: paths resolve inside FS.
	f, _ = mem.OpenFile("repo/.git/objects/info/alternates", os.O_WRONLY|os.O_CREATE, 0666)
	f.Write([]byte(filepath.Join(cwd, ".git/objects") + "\n"))
	f.Close()
	if code, out := run(nil, "-C", "repo", "log", "--oneline"); code != 0 || strings.Count(out, "\n") != 1 {
		t.Fatalf("log with alternates: %d %q", code, out)
	}

	// Nothing was written to the host.
	var hostFiles []string
	filepath.WalkDir(host, func(p string, d fs.DirEntry, err error) error {
		if p != host {
			hostFiles = append(hostFiles, strings.TrimPrefix(p, host))
		}
		return nil
	})
	if len(hostFiles) != 1 || hostFiles[0] != "/.gitconfig" {
		t.Fatalf("host directory changed: %v", hostFiles)
	}
}
