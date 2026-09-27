// Command git runs the go-git based CLI against the host filesystem. It is a
// drop-in for scripts that use the supported subset, which makes it easy to
// compare its output with real git.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	git "github.com/adrianliechti/go-git"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx))
}

func run(ctx context.Context) int {
	cwd, err := os.Getwd()
	if err == nil {
		cwd, err = filepath.EvalSymlinks(cwd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		return 128
	}
	// Root the FS at the volume root so -C and paths outside cwd work.
	root := filepath.VolumeName(cwd) + string(filepath.Separator)
	dir, err := git.OpenDir(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		return 128
	}
	defer dir.Close()
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	code, err := git.Run(ctx, git.Options{
		Args:   os.Args[1:],
		Dir:    filepath.ToSlash(strings.TrimPrefix(cwd, filepath.VolumeName(cwd))),
		Env:    env,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		FS:     dir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		return 128
	}
	return code
}
