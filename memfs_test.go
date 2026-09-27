package git_test

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	git "github.com/adrianliechti/go-git"
)

// memFS is a minimal in-memory git.FS for tests that must not touch the host.
type memFS struct {
	mu    sync.Mutex
	files map[string]*memNode
}

type memNode struct {
	dir   bool
	data  []byte
	mtime time.Time
}

func newMemFS() *memFS {
	return &memFS{files: map[string]*memNode{".": {dir: true}}}
}

func (m *memFS) Open(name string) (fs.File, error) { return m.OpenFile(name, os.O_RDONLY, 0) }

func (m *memFS) OpenFile(name string, flag int, perm fs.FileMode) (git.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	n := m.files[name]
	switch {
	case n == nil && flag&os.O_CREATE == 0:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	case n != nil && flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrExist}
	case n == nil:
		if p := m.files[path.Dir(name)]; p == nil || !p.dir {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		n = &memNode{mtime: time.Now()}
		m.files[name] = n
	}
	if n.dir {
		return &memFile{fs: m, name: name, node: n}, nil
	}
	if flag&os.O_TRUNC != 0 {
		n.data = nil
	}
	f := &memFile{fs: m, name: name, node: n, append: flag&os.O_APPEND != 0}
	return f, nil
}

func (m *memFS) Mkdir(name string, perm fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files[name] != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	if p := m.files[path.Dir(name)]; p == nil || !p.dir {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrNotExist}
	}
	m.files[name] = &memNode{dir: true, mtime: time.Now()}
	return nil
}

func (m *memFS) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.files[name]
	if n == nil {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	for p := range m.files {
		if strings.HasPrefix(p, name+"/") {
			return &fs.PathError{Op: "remove", Path: name, Err: syscall.ENOTEMPTY}
		}
	}
	delete(m.files, name)
	return nil
}

func (m *memFS) Rename(oldName, newName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.files[oldName]
	if n == nil {
		return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrNotExist}
	}
	moved := map[string]*memNode{}
	for p, c := range m.files {
		if p == oldName || strings.HasPrefix(p, oldName+"/") {
			moved[newName+strings.TrimPrefix(p, oldName)] = c
			delete(m.files, p)
		}
	}
	for p, c := range moved {
		m.files[p] = c
	}
	return nil
}

type memFile struct {
	fs     *memFS
	name   string
	node   *memNode
	off    int64
	append bool
	dirPos int
}

func (f *memFile) Stat() (fs.FileInfo, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	return memInfo{name: path.Base(f.name), node: f.node, size: int64(len(f.node.data))}, nil
}

func (f *memFile) Read(p []byte) (int, error) {
	n, err := f.ReadAt(p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if off >= int64(len(f.node.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.node.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *memFile) Write(p []byte) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.append {
		f.off = int64(len(f.node.data))
	}
	if end := f.off + int64(len(p)); end > int64(len(f.node.data)) {
		f.node.data = append(f.node.data, make([]byte, end-int64(len(f.node.data)))...)
	}
	copy(f.node.data[f.off:], p)
	f.off += int64(len(p))
	f.node.mtime = time.Now()
	return len(p), nil
}

func (f *memFile) Seek(off int64, whence int) (int64, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	switch whence {
	case io.SeekCurrent:
		off += f.off
	case io.SeekEnd:
		off += int64(len(f.node.data))
	}
	f.off = off
	return off, nil
}

func (f *memFile) Truncate(size int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	f.node.data = bytes.Clone(f.node.data[:min(size, int64(len(f.node.data)))])
	return nil
}

func (f *memFile) ReadDir(n int) ([]fs.DirEntry, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	var out []fs.DirEntry
	for p, c := range f.fs.files {
		if p != "." && path.Dir(p) == f.name {
			out = append(out, fs.FileInfoToDirEntry(memInfo{name: path.Base(p), node: c, size: int64(len(c.data))}))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	out = out[f.dirPos:]
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	f.dirPos += len(out)
	if n > 0 && len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}

func (f *memFile) Close() error { return nil }

type memInfo struct {
	name string
	node *memNode
	size int64
}

func (i memInfo) Name() string { return i.name }
func (i memInfo) Size() int64  { return i.size }
func (i memInfo) Mode() fs.FileMode {
	if i.node.dir {
		return fs.ModeDir | 0777
	}
	return 0666
}
func (i memInfo) ModTime() time.Time { return i.node.mtime }
func (i memInfo) IsDir() bool        { return i.node.dir }
func (i memInfo) Sys() any           { return nil }
