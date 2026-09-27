package git

import (
	"context"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for _, tc := range []struct {
		in, want, wantSpace string
	}{
		{"plain.txt", "plain.txt", "plain.txt"},
		{"with space", "with space", `"with space"`},
		{"tab\tx", `"tab\tx"`, `"tab\tx"`},
		{`q"uote`, `"q\"uote"`, `"q\"uote"`},
		{`back\slash`, `"back\\slash"`, `"back\\slash"`},
		{"ümlaut", `"\303\274mlaut"`, `"\303\274mlaut"`},
		{"bell\a", `"bell\a"`, `"bell\a"`},
		{"del\x7f", `"del\177"`, `"del\177"`},
	} {
		if got := q(tc.in); got != tc.want {
			t.Errorf("q(%q) = %s, want %s", tc.in, got, tc.want)
		}
		if got := qs(tc.in); got != tc.wantSpace {
			t.Errorf("qs(%q) = %s, want %s", tc.in, got, tc.wantSpace)
		}
	}
}

func TestRenameName(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"numbers", "counted", "numbers => counted"},
		{"d/a", "e/a", "{d => e}/a"},
		{"src/lib/code.go", "src/pkg/code.go", "src/{lib => pkg}/code.go"},
		{"a/b", "a/c/b", "a/{ => c}/b"},
		{"a/c/b", "a/b", "a/{c => }/b"},
		{"dir/file", "dir/other", "dir/{file => other}"},
		{"x y", "ümlaut", `x y => "\303\274mlaut"`},
	} {
		if got := renameName(tc.a, tc.b); got != tc.want {
			t.Errorf("renameName(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	a := []byte(strings.Repeat("line\n", 10))
	if s := similarity(hashChars(a, true), hashChars(a, true), len(a), len(a)); s != maxScore {
		t.Errorf("identical: %d", s)
	}
	b := append(append([]byte{}, a...), []byte(strings.Repeat("other\n", 3))...)
	if s := similarity(hashChars(a, true), hashChars(b, true), len(a), len(b)); similarityIndex(s) != 73 {
		t.Errorf("partial: %d%%", similarityIndex(s))
	}
	// Sizes too far apart are rejected before hashing, as in git.
	c := append(append([]byte{}, a...), []byte(strings.Repeat("other\n", 10))...)
	if s := similarity(hashChars(a, true), hashChars(c, true), len(a), len(c)); s != 0 {
		t.Errorf("size cutoff: %d", s)
	}
	crlf := []byte(strings.ReplaceAll(string(a), "\n", "\r\n"))
	if s := similarity(hashChars(a, true), hashChars(crlf, true), len(a), len(crlf)); s == 0 {
		t.Error("CRLF should hash like LF in text files")
	}
}

func TestDiffLines(t *testing.T) {
	render := func(ops []lineOp) string {
		var b strings.Builder
		for _, op := range ops {
			b.WriteByte(op.kind)
			b.WriteString(op.line)
		}
		return b.String()
	}
	split := func(s string) []string { return splitLines([]byte(s)) }
	for _, tc := range []struct{ a, b, want string }{
		{"a\nb\nc\n", "a\nb\nc\n", " a\n b\n c\n"},
		{"a\nb\nc\n", "a\nc\n", " a\n-b\n c\n"},
		{"a\n", "a\nb\n", " a\n+b\n"},
		// Ambiguous insertions slide down, as in git.
		{"x\na\n", "x\na\na\n", " x\n a\n+a\n"},
		{"a\nb\n", "", "-a\n-b\n"},
		{"", "a\n", "+a\n"},
		{"a", "a\n", "-a+a\n"},
	} {
		if got := render(diffLines(split(tc.a), split(tc.b))); got != tc.want {
			t.Errorf("diffLines(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestHunks(t *testing.T) {
	ops := make([]lineOp, 20)
	for i := range ops {
		ops[i] = lineOp{kind: ' '}
	}
	ops[2].kind, ops[15].kind = '-', '+'
	if got := hunks(ops, 3); len(got) != 2 || got[0] != [2]int{0, 6} || got[1] != [2]int{12, 19} {
		t.Errorf("separate hunks: %v", got)
	}
	ops[8].kind = '-'
	if got := hunks(ops, 3); len(got) != 1 || got[0] != [2]int{0, 19} {
		t.Errorf("merged hunks: %v", got)
	}
}

func TestShellAliasRefused(t *testing.T) {
	var out strings.Builder
	code, err := Run(context.Background(), Options{
		Args: []string{"-c", "alias.hi=!echo hi", "hi"}, Stdout: &out, Stderr: &out,
	})
	if err != nil || code != 128 || !strings.Contains(out.String(), "shell alias 'hi' cannot run") {
		t.Fatalf("shell alias: %d %v %q", code, err, out.String())
	}
}

func TestHelp(t *testing.T) {
	run := func(args ...string) (int, string) {
		var out strings.Builder
		code, err := Run(context.Background(), Options{Args: args, FS: nil, Stdout: &out, Stderr: &out})
		if err != nil {
			t.Fatal(err)
		}
		return code, out.String()
	}
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		code, out := run(args...)
		if code != 0 || !strings.HasPrefix(out, "usage: git") || !strings.Contains(out, "   commit     Record changes") {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
	if code, out := run(); code != 1 || !strings.HasPrefix(out, "usage: git") {
		t.Errorf("no args: %d %q", code, out)
	}
	if code, out := run("commit", "-h"); code != 129 || !strings.HasPrefix(out, "usage: git commit") {
		t.Errorf("commit -h: %d %q", code, out)
	}
	if code, out := run("status", "--help"); code != 0 || !strings.HasPrefix(out, "usage: git status") {
		t.Errorf("status --help: %d %q", code, out)
	}
	if code, out := run("help", "status"); code != 0 || !strings.Contains(out, "--porcelain") {
		t.Errorf("help status: %d %q", code, out)
	}
	if code, out := run("help", "frobnicate"); code != 1 || !strings.Contains(out, "not a git command") {
		t.Errorf("help unknown: %d %q", code, out)
	}
	if code, out := run("help", "-a"); code != 0 || !strings.Contains(out, "hash-object") {
		t.Errorf("help -a: %d %q", code, out)
	}
	// Every command has help text.
	for name := range commands {
		if help[name].summary == "" {
			t.Errorf("no help for %s", name)
		}
	}
}
