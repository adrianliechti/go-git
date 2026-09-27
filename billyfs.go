package git

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"

	"github.com/go-git/go-billy/v5"
)

// billyFS adapts the shell's FS to go-billy, confined to root.
// Root and all names use io/fs conventions internally; billy callers may pass
// names with or without a leading slash.
type billyFS struct {
	fsys FS
	root string // io/fs name of the chroot directory, "." for the shell root
}

var (
	_ billy.Filesystem = (*billyFS)(nil)
	_ billy.Capable    = (*billyFS)(nil)
)

func newBillyFS(fsys FS, root string) *billyFS {
	return &billyFS{fsys: fsys, root: root}
}

// full maps a billy name to an io/fs name inside root. Cleaning the name as
// an absolute path clamps ".." at root, so no name can escape it.
func (b *billyFS) full(name string) (string, error) {
	rel := strings.TrimPrefix(path.Clean("/"+name), "/")
	switch {
	case rel == "":
		return b.root, nil
	case b.root == ".":
		return rel, nil
	default:
		return b.root + "/" + rel, nil
	}
}

func (b *billyFS) Create(name string) (billy.File, error) {
	return b.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
}

func (b *billyFS) Open(name string) (billy.File, error) {
	return b.OpenFile(name, os.O_RDONLY, 0)
}

func (b *billyFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	full, err := b.full(name)
	if err != nil {
		return nil, err
	}
	if flag&os.O_CREATE != 0 {
		// Like billy's osfs, creating a file also creates its parents.
		if err := b.mkdirAll(path.Dir(full)); err != nil {
			return nil, err
		}
	}
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) == 0 {
		f, err := b.fsys.Open(full)
		if err != nil {
			return nil, err
		}
		return newBillyFile(name, f)
	}
	f, err := b.fsys.OpenFile(full, flag, perm)
	if err != nil {
		return nil, err
	}
	return newBillyFile(name, f)
}

func (b *billyFS) Stat(name string) (os.FileInfo, error) {
	full, err := b.full(name)
	if err != nil {
		return nil, err
	}
	return fs.Stat(b.fsys, full)
}

// Lstat follows symlinks because FS has no link inspection; the shell's
// guests cannot create symlinks anyway.
func (b *billyFS) Lstat(name string) (os.FileInfo, error) { return b.Stat(name) }

func (b *billyFS) Rename(oldName, newName string) error {
	from, err := b.full(oldName)
	if err != nil {
		return err
	}
	to, err := b.full(newName)
	if err != nil {
		return err
	}
	if err := b.mkdirAll(path.Dir(to)); err != nil {
		return err
	}
	return b.fsys.Rename(from, to)
}

func (b *billyFS) Remove(name string) error {
	full, err := b.full(name)
	if err != nil {
		return err
	}
	return b.fsys.Remove(full)
}

func (b *billyFS) Join(elem ...string) string { return path.Join(elem...) }

func (b *billyFS) TempFile(dir, prefix string) (billy.File, error) {
	for range 100 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, err
		}
		name := path.Join(dir, prefix+hex.EncodeToString(suffix[:]))
		f, err := b.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0666)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, &fs.PathError{Op: "tempfile", Path: dir, Err: fs.ErrExist}
}

func (b *billyFS) ReadDir(name string) ([]os.FileInfo, error) {
	full, err := b.full(name)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(b.fsys, full)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed concurrently
		}
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
	return infos, nil
}

func (b *billyFS) MkdirAll(name string, perm os.FileMode) error {
	full, err := b.full(name)
	if err != nil {
		return err
	}
	return b.mkdirAll(full)
}

func (b *billyFS) mkdirAll(full string) error {
	if full == "." {
		return nil
	}
	if info, err := fs.Stat(b.fsys, full); err == nil {
		if !info.IsDir() {
			return &fs.PathError{Op: "mkdir", Path: full, Err: syscall.ENOTDIR}
		}
		return nil
	}
	if err := b.mkdirAll(path.Dir(full)); err != nil {
		return err
	}
	if err := b.fsys.Mkdir(full, 0777); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func (b *billyFS) Symlink(target, link string) error {
	return &fs.PathError{Op: "symlink", Path: link, Err: billy.ErrNotSupported}
}

func (b *billyFS) Readlink(link string) (string, error) {
	return "", &fs.PathError{Op: "readlink", Path: link, Err: billy.ErrNotSupported}
}

func (b *billyFS) Chroot(name string) (billy.Filesystem, error) {
	full, err := b.full(name)
	if err != nil {
		return nil, err
	}
	return &billyFS{fsys: b.fsys, root: full}, nil
}

func (b *billyFS) Root() string {
	if b.root == "." {
		return "/"
	}
	return "/" + b.root
}

func (b *billyFS) Capabilities() billy.Capability {
	return billy.WriteCapability | billy.ReadCapability | billy.ReadAndWriteCapability |
		billy.SeekCapability | billy.TruncateCapability
}

// billyFile adds the optional file capabilities go-git expects. Read-only files
// from plain fs.FS mounts may lack Seek and ReadAt; those are buffered.
type billyFile struct {
	name string
	fs.File
	rs interface {
		io.ReadSeeker
		io.ReaderAt
	}
}

func newBillyFile(name string, f fs.File) (billy.File, error) {
	bf := &billyFile{name: name, File: f}
	if rs, ok := f.(interface {
		io.ReadSeeker
		io.ReaderAt
	}); ok {
		bf.rs = rs
		return bf, nil
	}
	if _, writable := f.(io.Writer); writable {
		// Writable backends in this repository implement Seek and ReadAt.
		return bf, nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	bf.rs = bytes.NewReader(data)
	return bf, nil
}

func (f *billyFile) Name() string { return f.name }

func (f *billyFile) Read(p []byte) (int, error) {
	if f.rs != nil {
		return f.rs.Read(p)
	}
	return f.File.Read(p)
}

func (f *billyFile) ReadAt(p []byte, off int64) (int, error) {
	if f.rs == nil {
		return 0, &fs.PathError{Op: "readat", Path: f.name, Err: billy.ErrNotSupported}
	}
	return f.rs.ReadAt(p, off)
}

func (f *billyFile) Seek(off int64, whence int) (int64, error) {
	if f.rs != nil {
		return f.rs.Seek(off, whence)
	}
	if s, ok := f.File.(io.Seeker); ok {
		return s.Seek(off, whence)
	}
	return 0, &fs.PathError{Op: "seek", Path: f.name, Err: billy.ErrNotSupported}
}

func (f *billyFile) Write(p []byte) (int, error) {
	if w, ok := f.File.(io.Writer); ok {
		return w.Write(p)
	}
	return 0, &fs.PathError{Op: "write", Path: f.name, Err: fs.ErrPermission}
}

func (f *billyFile) Truncate(size int64) error {
	if t, ok := f.File.(interface{ Truncate(int64) error }); ok {
		return t.Truncate(size)
	}
	return &fs.PathError{Op: "truncate", Path: f.name, Err: billy.ErrNotSupported}
}

// Lock and Unlock are no-ops: vfs has no advisory locks. Concurrent git
// commands in one pipeline are therefore not protected from each other.
func (f *billyFile) Lock() error   { return nil }
func (f *billyFile) Unlock() error { return nil }
