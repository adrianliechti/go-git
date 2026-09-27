package git

import (
	"io/fs"
	"os"
)

// Dir is an FS backed by a host directory. os.Root confines every operation,
// including symlink resolution, to that directory.
type Dir struct {
	root *os.Root
	fs.FS
}

var _ FS = (*Dir)(nil)

// OpenDir opens a host directory as an FS. Close it when done.
func OpenDir(dir string) (*Dir, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Dir{root: root, FS: root.FS()}, nil
}

func (d *Dir) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	return d.root.OpenFile(name, flag, perm)
}

func (d *Dir) Mkdir(name string, perm fs.FileMode) error { return d.root.Mkdir(name, perm) }
func (d *Dir) Remove(name string) error                  { return d.root.Remove(name) }
func (d *Dir) Rename(oldName, newName string) error      { return d.root.Rename(oldName, newName) }
func (d *Dir) Close() error                              { return d.root.Close() }
