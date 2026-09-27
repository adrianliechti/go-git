package git_test

import (
	"context"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/adrianliechti/go-git"
)

// hostGit runs the real git in dir with an isolated configuration.
func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(testEnv(dir), "PATH=/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func testEnv(home string) []string {
	return []string{
		"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Ada Author", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Cody Committer", "GIT_COMMITTER_EMAIL=cody@example.com",
		"GIT_AUTHOR_DATE=@1700000000 +0100", "GIT_COMMITTER_DATE=@1700000000 +0000",
	}
}

func envMap(home string) map[string]string {
	m := map[string]string{}
	for _, kv := range testEnv(home) {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// TestHTTPRemote clones, pulls, and pushes over smart HTTP against the real
// git http-backend, with the client side entirely in memory.
func TestHTTPRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("real git not found on PATH")
	}
	execPath := strings.TrimSpace(hostGit(t, t.TempDir(), "--exec-path"))
	backend := filepath.Join(execPath, "git-http-backend")
	root := t.TempDir()
	hostGit(t, root, "init", "-q", "--bare", "-b", "main", "srv.git")
	hostGit(t, filepath.Join(root, "srv.git"), "config", "http.receivepack", "true")
	hostGit(t, root, "clone", "-q", "srv.git", "seed")
	seed := filepath.Join(root, "seed")
	hostGit(t, seed, "commit", "-q", "--allow-empty", "-m", "seed")
	hostGit(t, seed, "push", "-q", "origin", "main")

	srv := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + root},
	})
	defer srv.Close()
	url := srv.URL + "/srv.git"

	mem := newMemFS()
	run := func(opts git.Options) (int, string) {
		var out strings.Builder
		opts.FS, opts.Stdout, opts.Stderr = mem, &out, &out
		if opts.Env == nil {
			opts.Env = envMap("/")
		}
		code, err := git.Run(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return code, out.String()
	}

	if code, out := run(git.Options{Args: []string{"clone", url, "work"}, Dir: "/", DisableNetwork: true}); code != 128 || !strings.Contains(out, "network access is disabled") {
		t.Fatalf("clone with network disabled: %d %q", code, out)
	}
	if code, out := run(git.Options{Args: []string{"clone", "-q", url, "work"}, Dir: "/", HTTPClient: srv.Client()}); code != 0 {
		t.Fatalf("clone: %d %q", code, out)
	}
	if _, out := run(git.Options{Args: []string{"log", "--format=%s"}, Dir: "/work"}); out != "seed\n" {
		t.Fatalf("log after clone: %q", out)
	}

	// Push a commit from memory to the server.
	f, _ := mem.OpenFile("work/a.txt", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	f.Write([]byte("hello\n"))
	f.Close()
	run(git.Options{Args: []string{"add", "a.txt"}, Dir: "/work"})
	run(git.Options{Args: []string{"commit", "-q", "-m", "from memory"}, Dir: "/work"})
	if code, out := run(git.Options{Args: []string{"push"}, Dir: "/work", HTTPClient: srv.Client()}); code != 0 || !strings.Contains(out, "main -> main") {
		t.Fatalf("push: %d %q", code, out)
	}
	if got := hostGit(t, filepath.Join(root, "srv.git"), "log", "--format=%s"); got != "from memory\nseed\n" {
		t.Fatalf("server log after push: %q", got)
	}

	// Pull a commit pushed by the real git.
	hostGit(t, seed, "pull", "-q")
	hostGit(t, seed, "commit", "-q", "--allow-empty", "-m", "from host")
	hostGit(t, seed, "push", "-q")
	if code, out := run(git.Options{Args: []string{"pull"}, Dir: "/work", HTTPClient: srv.Client()}); code != 0 || !strings.Contains(out, "Fast-forward") {
		t.Fatalf("pull: %d %q", code, out)
	}
	if _, out := run(git.Options{Args: []string{"log", "--format=%s"}, Dir: "/work"}); out != "from host\nfrom memory\nseed\n" {
		t.Fatalf("log after pull: %q", out)
	}
	if _, out := run(git.Options{Args: []string{"status", "-sb"}, Dir: "/work"}); out != "## main...origin/main\n" {
		t.Fatalf("status after pull: %q", out)
	}

	// Remotes that would need the host (ssh, other schemes) are refused.
	if code, out := run(git.Options{Args: []string{"clone", "git@example.com:repo.git"}, Dir: "/"}); code != 128 || !strings.Contains(out, "unsupported remote") {
		t.Fatalf("ssh clone: %d %q", code, out)
	}
}
