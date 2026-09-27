package git

import (
	"errors"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Remotes are either other repositories inside FS (local paths and file://
// URLs), whose objects are copied between storages, or HTTP(S) servers,
// reached with go-git's smart HTTP client and the host's http.Client unless
// Options.DisableNetwork is set. go-git's Remote type is not used: it
// resolves transports through a global registry, and its file transport
// reads the host filesystem.

// endpoint is a resolved remote URL.
type endpoint struct {
	url   string // as configured, for messages
	local string // absolute FS path of a local repository
	http  *transport.Endpoint
}

// resolveURL resolves a remote URL; relative paths are relative to base.
func (g *gitRun) resolveURL(url, base string) (endpoint, error) {
	ep := endpoint{url: url}
	switch {
	case strings.HasPrefix(url, "file://"):
		ep.local = path.Clean("/" + strings.TrimPrefix(url, "file://"))
	case strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://"):
		if g.disableNetwork {
			return ep, fatalf("network access is disabled; cannot reach '%s'", url)
		}
		e, err := transport.NewEndpoint(url)
		if err != nil {
			return ep, fatalf("invalid URL '%s': %v", url, err)
		}
		ep.http = e
	case strings.Contains(url, "://") || isSCPLike(url):
		return ep, fatalf("unsupported remote '%s': only local paths, file:// and http(s):// URLs are supported", url)
	case path.IsAbs(url):
		ep.local = path.Clean(url)
	default:
		ep.local = path.Join(base, url)
	}
	return ep, nil
}

// isSCPLike matches ssh shorthand such as "git@host:repo.git".
func isSCPLike(url string) bool {
	colon := strings.Index(url, ":")
	slash := strings.Index(url, "/")
	return colon > 0 && (slash < 0 || colon < slash)
}

// displayURL is the URL in "From" lines: git drops a trailing ".git".
func displayURL(url string) string {
	return strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
}

// remoteRefs is a remote's ref advertisement.
type remoteRefs struct {
	refs   map[plumbing.ReferenceName]plumbing.Hash
	peeled map[plumbing.ReferenceName]plumbing.Hash // annotated tags to their targets
	head   plumbing.ReferenceName                   // HEAD's branch, "" if unknown or detached
}

type refUpdate struct {
	name     plumbing.ReferenceName
	old, new plumbing.Hash // a zero new hash deletes the ref
}

type remoteConn interface {
	advertised() (*remoteRefs, error)
	// fetch stores the objects reachable from wants but not from haves.
	fetch(dst *repo, wants, haves []plumbing.Hash) error
	// push sends objects and applies updates. It returns the remote's
	// rejection reasons by ref name.
	push(src *repo, updates []refUpdate, haves []plumbing.Hash) (map[plumbing.ReferenceName]string, error)
}

func errNoRemoteRepo(url string) error {
	return failf(128, "fatal: '%s' does not appear to be a git repository\n"+
		"fatal: Could not read from remote repository.\n\n"+
		"Please make sure you have the correct access rights\nand the repository exists.\n", url)
}

func (g *gitRun) connect(ep endpoint) (remoteConn, error) {
	if ep.http != nil {
		client := g.httpClient
		if client == nil {
			client = http.DefaultClient
		}
		return &httpConn{g: g, tr: githttp.NewClient(client), ep: ep.http}, nil
	}
	r, err := g.openPath(ep.local)
	if err != nil {
		return nil, errNoRemoteRepo(ep.url)
	}
	return &localConn{r: r}, nil
}

// localConn is a repository inside FS.
type localConn struct{ r *repo }

func (c *localConn) advertised() (*remoteRefs, error) {
	adv := &remoteRefs{refs: map[plumbing.ReferenceName]plumbing.Hash{}, peeled: map[plumbing.ReferenceName]plumbing.Hash{}}
	iter, err := c.r.Storer.IterReferences()
	if err != nil {
		return nil, err
	}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name()
		if name == plumbing.HEAD {
			return nil
		}
		resolved, err := c.r.Reference(name, true)
		if err != nil {
			return nil // dangling symbolic ref
		}
		adv.refs[name] = resolved.Hash()
		if name.IsTag() {
			if t, err := c.r.TagObject(resolved.Hash()); err == nil {
				adv.peeled[name] = t.Target
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if head, err := c.r.Storer.Reference(plumbing.HEAD); err == nil && head.Type() == plumbing.SymbolicReference {
		adv.head = head.Target()
	}
	return adv, nil
}

// copyObjects copies the objects reachable from wants and not from haves.
func copyObjects(src, dst *repo, wants, haves []plumbing.Hash) error {
	var known []plumbing.Hash
	for _, h := range haves {
		if src.Storer.HasEncodedObject(h) == nil {
			known = append(known, h)
		}
	}
	hashes, err := revlist.Objects(src.Storer, wants, known)
	if err != nil {
		return err
	}
	for _, h := range hashes {
		if dst.Storer.HasEncodedObject(h) == nil {
			continue
		}
		obj, err := src.Storer.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return err
		}
		if _, err := dst.Storer.SetEncodedObject(obj); err != nil {
			return err
		}
	}
	return nil
}

func (c *localConn) fetch(dst *repo, wants, haves []plumbing.Hash) error {
	return copyObjects(c.r, dst, wants, haves)
}

func (c *localConn) push(src *repo, updates []refUpdate, haves []plumbing.Hash) (map[plumbing.ReferenceName]string, error) {
	rejected := map[plumbing.ReferenceName]string{}
	var wants []plumbing.Hash
	current := ""
	if !c.r.bare() {
		current, _ = c.r.branchName()
	}
	for _, u := range updates {
		if current != "" && u.name == plumbing.NewBranchReferenceName(current) {
			rejected[u.name] = "branch is currently checked out"
			continue
		}
		if !u.new.IsZero() {
			wants = append(wants, u.new)
		}
	}
	if len(wants) > 0 {
		if err := copyObjects(src, c.r, wants, haves); err != nil {
			return nil, err
		}
	}
	for _, u := range updates {
		if _, ok := rejected[u.name]; ok {
			continue
		}
		var err error
		if u.new.IsZero() {
			err = c.r.Storer.RemoveReference(u.name)
		} else {
			err = c.r.Storer.SetReference(plumbing.NewHashReference(u.name, u.new))
		}
		if err != nil {
			return nil, err
		}
	}
	return rejected, nil
}

// httpConn is a smart HTTP server.
type httpConn struct {
	g   *gitRun
	tr  transport.Transport
	ep  *transport.Endpoint
	up  transport.UploadPackSession
	adv *packp.AdvRefs
}

func (c *httpConn) advertised() (*remoteRefs, error) {
	sess, err := c.tr.NewUploadPackSession(c.ep, nil)
	if err != nil {
		return nil, c.wrap(err)
	}
	ar, err := sess.AdvertisedReferencesContext(c.g.ctx)
	if err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, c.wrap(err)
	}
	c.up, c.adv = sess, ar
	adv := &remoteRefs{refs: map[plumbing.ReferenceName]plumbing.Hash{}, peeled: map[plumbing.ReferenceName]plumbing.Hash{}}
	if ar == nil {
		return adv, nil
	}
	for name, h := range ar.References {
		adv.refs[plumbing.ReferenceName(name)] = h
	}
	for name, h := range ar.Peeled {
		adv.peeled[plumbing.ReferenceName(name)] = h
	}
	for _, v := range ar.Capabilities.Get(capability.SymRef) {
		if src, dst, ok := strings.Cut(v, ":"); ok && src == "HEAD" {
			adv.head = plumbing.ReferenceName(dst)
		}
	}
	return adv, nil
}

func (c *httpConn) wrap(err error) error {
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fatalf("repository '%s' not found", c.ep.String())
	case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
		return fatalf("Authentication failed for '%s'", c.ep.String())
	}
	return fatalf("unable to access '%s': %v", c.ep.String(), err)
}

func (c *httpConn) fetch(dst *repo, wants, haves []plumbing.Hash) error {
	if c.up == nil {
		if _, err := c.advertised(); err != nil {
			return err
		}
	}
	req := packp.NewUploadPackRequestFromCapabilities(c.adv.Capabilities)
	if c.adv.Capabilities.Supports(capability.NoProgress) {
		req.Capabilities.Set(capability.NoProgress)
	}
	req.Wants = wants
	for _, h := range haves {
		if dst.Storer.HasEncodedObject(h) == nil {
			req.Haves = append(req.Haves, h)
		}
	}
	reader, err := c.up.UploadPack(c.g.ctx, req)
	if errors.Is(err, transport.ErrEmptyUploadPackRequest) {
		return nil
	}
	if err != nil {
		return c.wrap(err)
	}
	defer reader.Close()
	var rd io.Reader = reader
	switch {
	case req.Capabilities.Supports(capability.Sideband64k):
		rd = sideband.NewDemuxer(sideband.Sideband64k, reader)
	case req.Capabilities.Supports(capability.Sideband):
		rd = sideband.NewDemuxer(sideband.Sideband, reader)
	}
	return packfile.UpdateObjectStorage(dst.Storer, rd)
}

func (c *httpConn) push(src *repo, updates []refUpdate, haves []plumbing.Hash) (map[plumbing.ReferenceName]string, error) {
	sess, err := c.tr.NewReceivePackSession(c.ep, nil)
	if err != nil {
		return nil, c.wrap(err)
	}
	defer sess.Close()
	ar, err := sess.AdvertisedReferencesContext(c.g.ctx)
	if err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, c.wrap(err)
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	var wants []plumbing.Hash
	for _, u := range updates {
		req.Commands = append(req.Commands, &packp.Command{Name: u.name, Old: u.old, New: u.new})
		if !u.new.IsZero() {
			wants = append(wants, u.new)
		}
	}
	var known []plumbing.Hash
	for _, h := range haves {
		if src.Storer.HasEncodedObject(h) == nil {
			known = append(known, h)
		}
	}
	done := make(chan error, 1)
	if len(wants) > 0 {
		hashes, err := revlist.Objects(src.Storer, wants, known)
		if err != nil {
			return nil, err
		}
		rd, wr := io.Pipe()
		req.Packfile = rd
		go func() {
			_, err := packfile.NewEncoder(wr, src.Storer, false).Encode(hashes, 10)
			done <- wr.CloseWithError(err)
		}()
	} else {
		close(done)
	}
	report, err := sess.ReceivePack(c.g.ctx, req)
	if err != nil {
		if req.Packfile != nil {
			req.Packfile.Close()
		}
		return nil, c.wrap(err)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	rejected := map[plumbing.ReferenceName]string{}
	if report != nil {
		for _, s := range report.CommandStatuses {
			if s.Status != "ok" {
				rejected[s.ReferenceName] = s.Status
			}
		}
	}
	return rejected, nil
}
