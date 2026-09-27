package git

import (
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// writeTree stores the files of s as tree objects and returns the root.
func (r *repo) writeTree(s side) (plumbing.Hash, error) {
	paths := make([]string, 0, len(s))
	for p := range s {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return r.writeSubtree(s, "", paths)
}

// writeSubtree writes the tree for prefix; paths are all below it.
func (r *repo) writeSubtree(s side, prefix string, paths []string) (plumbing.Hash, error) {
	var entries []object.TreeEntry
	for i := 0; i < len(paths); {
		rel := strings.TrimPrefix(paths[i], prefix)
		name, _, isDir := strings.Cut(rel, "/")
		if !isDir {
			e := s[paths[i]]
			entries = append(entries, object.TreeEntry{Name: name, Mode: e.mode, Hash: e.hash})
			i++
			continue
		}
		sub := prefix + name + "/"
		j := i
		for j < len(paths) && strings.HasPrefix(paths[j], sub) {
			j++
		}
		h, err := r.writeSubtree(s, sub, paths[i:j])
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
		i = j
	}
	// Git orders tree entries as if directory names ended in '/'.
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
	return r.storeObject(&object.Tree{Entries: entries})
}

type encoder interface {
	Encode(plumbing.EncodedObject) error
}

func (r *repo) storeObject(o encoder) (plumbing.Hash, error) {
	obj := r.Storer.NewEncodedObject()
	if err := o.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.Storer.SetEncodedObject(obj)
}

// commitIndex stores a commit of the current index. Signatures are always
// explicit, so no configuration outside FS is consulted.
func (r *repo) commitIndex(msg string, author, committer *object.Signature, parents []plumbing.Hash) (plumbing.Hash, error) {
	idx, err := r.readIndex()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	tree, err := r.writeTree(r.indexSide(idx))
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return r.storeObject(&object.Commit{
		Author: *author, Committer: *committer, Message: msg,
		TreeHash: tree, ParentHashes: parents,
	})
}
