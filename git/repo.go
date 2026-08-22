// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// Package git implements support for querying and patching
// git repositories. Operations in this package are intended
// to be used in command line tooling and are therefore
// generally fatal on error.
package git

import (
	"bytes"
	"context"
	"crypto"
	_ "crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/grailbio/base/digest"
	"github.com/grailbio/base/flock"
	"github.com/grailbio/base/log"
)

func init() {
	// If we are testing in a sandboxed environment with no writable /var/tmp,
	// we can use the TEST_TMPDIR environment variable to override the default
	// location.
	testTmp := os.Getenv("TEST_TMPDIR")
	if testTmp != "" {
		Dir = filepath.Join(testTmp, "grit")
	}
}

// Dir is the directory in which git checkouts are made.
var Dir = "/var/tmp/grit"

// SHA1 is the digester used to represent Git hashes.
var SHA1 = digest.Digester(crypto.SHA1)

const gitTimeLayout = "Mon, 2 Jan 2006 15:04:05 -0700"

// A Repo is a cached git repository against which
// supported git operations are issued.
type Repo struct {
	url        string
	branch     string
	root       string
	prefix     string
	lock       *flock.T
	config     map[string]string
	originHead string
}

// Open returns a repo representing the provided git remote url, branch, and
// prefix within the repository. The prefix is interpreted to provide
// a "view" into the git repository: all operations apply only to
// this prefix. Repositories are safe for concurrent operations
// across multiple uses on the same machine.
func Open(url, prefix, branch string) (*Repo, error) {
	base := filepath.Base(url)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	h := sha256.New()
	h.Write([]byte(url))
	b := h.Sum(nil)
	os.MkdirAll(Dir, 0700)
	path := filepath.Join(Dir, fmt.Sprintf("%s%02x%02x%02x%02x", base, b[0], b[1], b[2], b[3]))
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	r := &Repo{url: url, root: path, prefix: prefix, branch: branch}
	r.lock = flock.New(path + ".lock")
	if err := r.lock.Lock(context.Background()); err != nil {
		return nil, fmt.Errorf("lock %s: %v", path, err)
	}
	if err != nil {
		os.MkdirAll(path, 0777)
		if _, err := r.git(nil, "clone", "--single-branch", r.url, r.root); err != nil {
			return nil, err
		}
	}
	if _, err := r.git(nil, "fetch", "origin", branch); err != nil {
		return nil, err
	}
	// Capture the remote tip now: any later fetch (e.g. object seeding)
	// overwrites FETCH_HEAD, so this is the only reliable moment.
	head, err := r.Head()
	if err != nil {
		return nil, err
	}
	r.originHead, err = r.RevParse("FETCH_HEAD")
	if err != nil {
		return nil, err
	}
	// Preserve work that must survive across grit invocations: a paused
	// git am session awaiting conflict resolution, and commits that a
	// continued session has already created but that have not been
	// pushed yet. Both are only ever written by grit itself, and
	// discarding them would destroy manually resolved work. HEAD that is
	// merely behind the remote tip is the normal case and gets reset.
	inProgress, err := r.InProgressAM()
	if err != nil {
		return nil, err
	}
	if inProgress {
		log.Printf("resuming interrupted git am session in %s", r.root)
		return r, nil
	}
	ahead, err := r.IsAncestor(head, r.originHead)
	if err != nil {
		return nil, err
	}
	if !ahead {
		// Only grit-authored state (a shipit id at HEAD, as written for
		// every commit grit creates) may be preserved for a later push.
		authored, err := r.HeadIsGritAuthored()
		if err != nil {
			return nil, err
		}
		if !authored {
			return nil, fmt.Errorf("%s holds unpushed commits without a grit shipit id at HEAD; this is not grit-authored state -- inspect the repository and remove or explicitly adopt them before synchronizing", r.root)
		}
		log.Printf("preserving %s: local commits from a resolved session have not been pushed yet", r.root)
		return r, nil
	}
	// Clear potentially interrupted run.
	_, _ = r.git(nil, "am", "--abort")
	if _, err := r.git(nil, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return nil, err
	}
	return r, nil
}

// InProgressAM reports whether a git am session is paused in the
// repository, awaiting conflict resolution.
func (r *Repo) InProgressAM() (bool, error) {
	_, err := os.Stat(filepath.Join(r.root, ".git", "rebase-apply"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// IsAncestor reports whether ancestor is equal to or an ancestor of
// descendant. Both digests must resolve; with valid revisions, a
// merge-base failure can only mean that the two commits are unrelated,
// which is not ancestry. (A transient git failure would also map to
// "not ancestor"; the consequence is a preserved tree and a later push,
// never data loss.)
func (r *Repo) IsAncestor(ancestor, descendant string) (bool, error) {
	out, err := r.git(nil, "merge-base", ancestor, descendant)
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) == ancestor, nil
}

// OriginHead returns the digest of the repository's remote tip for the
// configured branch, as of the most recent Open. The value is a
// snapshot: a concurrent third-party push makes comparisons against it
// stale, which is benign here — the next run re-syncs, and a stale push
// fails loudly rather than silently.
func (r *Repo) OriginHead() string {
	return r.originHead
}

var ownLineShipitIDRe = regexp.MustCompile(`(?m)^\s*(?:fb)?shipit-source-id: ([a-z0-9]+)$`)

// OwnLineShipitIDs returns the shipit source ids recorded on their own
// lines in the commit message — the form grit writes. Matching anywhere
// in the body would let prose that quotes an id be mistaken for one;
// note that Log dedents four-space-indented lines, so indented
// quotations of ids remain an accepted residual ambiguity shared with
// ShipitID.
func (c *Commit) OwnLineShipitIDs() []string {
	var ids []string
	for _, match := range ownLineShipitIDRe.FindAllStringSubmatch(c.Body, -1) {
		ids = append(ids, match[1])
	}
	return ids
}

// HeadIsGritAuthored reports whether HEAD carries a shipit source id,
// marking it as state grit itself created.
func (r *Repo) HeadIsGritAuthored() (bool, error) {
	commits, err := r.Log("-1", "HEAD")
	if err != nil {
		return false, err
	}
	return len(commits) == 1 && len(commits[0].OwnLineShipitIDs()) > 0, nil
}

// Head returns the digest of the commit HEAD refers to.
func (r *Repo) Head() (string, error) {
	return r.RevParse("HEAD")
}

// RevParse returns the digest that the provided revision resolves to.
func (r *Repo) RevParse(rev string) (string, error) {
	out, err := r.git(nil, "rev-parse", rev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// BlobHash returns the git blob digest of the file at the provided
// repository-relative working tree path. Deleted files hash to the zero
// digest, mirroring the index line of git diffs. Symbolic links hash
// their target path text via git's own machinery, matching how git
// stores links. The zero digest is also the sentinel used for
// sha256-object-format repositories' deletion lines under the SHA1
// digester; such repositories fail loudly elsewhere.
func (r *Repo) BlobHash(path string) (string, error) {
	if _, err := os.Stat(filepath.Join(r.root, filepath.FromSlash(path))); err != nil {
		if os.IsNotExist(err) {
			return zeroBlob, nil
		}
		return "", err
	}
	out, err := r.git(nil, "hash-object", "--", path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// FetchObjects fetches the provided branch from the local repository at
// the provided path into a private ref, making the source repository's
// objects (in particular the pre-image blobs recorded by patches)
// available for three-way merges in this repository. The ref is local
// plumbing and is never pushed.
func (r *Repo) FetchObjects(path, branch string) error {
	_, err := r.git(nil, "fetch", "--no-tags", path,
		fmt.Sprintf("+refs/heads/%s:refs/grit/src", branch))
	return err
}

// Prefix returns the prefix within the repository, as specified in Open.
func (r *Repo) Prefix() string {
	return r.prefix
}

// RepoRoot returns the repository's working directory on the local
// filesystem.
func (r *Repo) RepoRoot() string {
	return r.root
}

func (r *Repo) String() string {
	return fmt.Sprintf("%s,%s,%s", r.url, r.prefix, r.branch)
}

// Close relinquishes the repo's lock. Repo operations may not
// be safely performed after the repository has been closed.
func (r *Repo) Close() error {
	return r.lock.Unlock()
}

// Linearize linearizes the repository's history.
func (r *Repo) Linearize() error {
	_, err := r.git(nil, "filter-branch", "-f", "--parent-filter", `cut -f 2,3 -d " "`)
	return err
}

// Configure sets the configuration parameter named by key to
// the value value. Properties configured this way overrides the
// Git's defaults (e.g., sourced through a user's .gitconfig) for
// repo Git invocations.
func (r *Repo) Configure(key, value string) {
	if r.config == nil {
		r.config = make(map[string]string)
	}
	r.config[key] = value
}

// Log returns a set of commit objects representing the "git log" operation
// with the provided arguments.
func (r *Repo) Log(args ...string) (commits []*Commit, err error) {
	args = append([]string{"log"}, args...)
	if r.prefix != "" {
		args = append(args, r.prefix)
	}
	out, err := r.git(nil, args...)
	if err != nil {
		if strings.Contains(err.Error(), "path not in the working tree") {
			// Allow missing destination directory.
			return nil, nil
		}
		return nil, err
	}
	err = foreach(out, "commit", func(commit []byte) error {
		c := &Commit{repo: r}
		headers := scan(&commit, "\n")
		digest := scanLine(&headers)
		digest = bytes.TrimPrefix(digest, []byte("commit "))
		var err error
		c.Digest, err = SHA1.Parse(string(digest))
		if err != nil {
			return fmt.Errorf("invalid commit digest %v: %v", digest, err)
		}
		for headers != nil {
			line := scanLine(&headers)
			keyval := strings.SplitN(string(line), ":", 2)
			key, val := keyval[0], keyval[1]
			val = strings.TrimLeftFunc(val, unicode.IsSpace)
			c.Headers = append(c.Headers, Header{key, val})
		}
		commit = bytes.TrimPrefix(commit, []byte("    "))
		c.Body = string(bytes.Replace(commit, []byte("\n    "), []byte("\n"), -1))
		commits = append(commits, c)
		return nil
	})
	return
}

var (
	prefixA = []byte("--- a/")
	prefixB = []byte("+++ b/")
)

// Patch returns a patch representing the commit named by the provided ID.  Arg
// dstPrefix is the prefix of the destination repository. If dstPrefix!="", it
// it is prepended to the pathnames in the patch.
func (r *Repo) Patch(id digest.Digest, dstPrefix string) (Patch, error) {
	// To minimize the amount of parsing we have to do here, first get the
	// diffs only, and then extract the rest of the message which can be
	// passed directly as a regular email.

	rawdiffs, err := r.git(nil, "format-patch",
		"--always", // to support empty commits
		"--no-renames", "--no-stat", "--stdout",
		"--format=", // diff content only
		// Serialize binary deltas as applicable binary patches, and
		// record full (unabbreviated) blob digests in index lines, so
		// that three-way merges reconstruct exact pre-images and
		// post-images are comparable byte-for-byte.
		"--binary", "--full-index",
		"-1", id.Hex(),
	)
	if err != nil {
		return Patch{}, err
	}
	raw, err := r.git(nil, "format-patch",
		"--always", "--no-renames", "--no-stat", "--binary", "--full-index", "-1", id.Hex(), "--stdout")
	if err != nil {
		return Patch{}, err
	}
	raw = bytes.TrimSuffix(raw, rawdiffs)
	patch, err := parsePatchHeader(raw)
	if err != nil {
		return Patch{}, fmt.Errorf("parse patch %v: %v", id, err)
	}

	err = foreach(rawdiffs, "diff", func(diff []byte) error {
		header := scanLine(&diff)
		path := parseDiffHeader(header)
		if path == nil {
			return errors.New("diff is missing header")
		}
		meta := next(&diff, "@@")
		patch.Diffs = append(patch.Diffs, Diff{Path: string(path), Meta: meta, Body: diff})
		return nil
	})
	if err != nil {
		return Patch{}, err
	}
	fixPath := func(path string) string {
		return dstPrefix + strings.TrimPrefix(path, r.prefix)
	}

	var diffs []Diff
	for _, diff := range patch.Diffs {
		if strings.HasPrefix(diff.Path, r.prefix) {
			diff.Path = fixPath(diff.Path)
			// Also rewrite any --- or +++ meta lines that begin with a/ or b/,
			// since they are also paths. The rest of meta is opaque to us.
			meta := diff.Meta
			diff.Meta = nil
			for meta != nil {
				line := scanLine(&meta)
				switch {
				case bytes.HasPrefix(line, prefixA) || bytes.HasPrefix(line, prefixB):
					path := []byte(fixPath(string(line[len(prefixA):])))
					diff.Meta = append(diff.Meta, line[:len(prefixA)]...)
					diff.Meta = append(diff.Meta, path...)
					diff.Meta = append(diff.Meta, '\n')
				default:
					diff.Meta = append(diff.Meta, line...)
					diff.Meta = append(diff.Meta, '\n')
				}
			}
			diff.Meta = bytes.TrimSuffix(diff.Meta, []byte{'\n'})
			diffs = append(diffs, diff)
		} else {
			log.Debug.Printf("dropping diff with path %s not in prefix %s", diff.Path, r.prefix)
		}
	}
	patch.Diffs = diffs
	return patch, nil
}

// Apply applies a patch to the repository.
func (r *Repo) Apply(patch Patch) error {
	if len(patch.Diffs) == 0 {
		return nil
	}
	var b bytes.Buffer
	if err := patch.Write(&b); err != nil {
		return fmt.Errorf("patch write: %v", err)
	}
	log.Debug.Printf("applying patch %s", patch.ID.Hex()[:7])
	// --3way falls back to a three-way merge (base = the pre-image blob
	// recorded in the patch's index line, ours = the local state,
	// theirs = the patched state) whenever the textual patch does not
	// apply. This converges content that arrived through other routes
	// instead of failing, while leaving genuine conflicts paused for
	// resolution.
	_, err := r.git(b.Bytes(), "am", "--3way", "--keep-non-patch", "--keep-cr")
	return err
}

// Push pushes the current state of the repository to the provided
// branch on the provided remote.
func (r *Repo) Push(remote, remoteBranch string) error {
	_, err := r.git(nil, "lfs", "push", "origin", remoteBranch)
	if err != nil {
		return err
	}
	_, err = r.git(nil, "push", remote, "HEAD:"+remoteBranch)
	return err
}

// ListLFSPointers returns paths to in the repository which are LFS
// pointers. The paths are relative to the repository's root.
func (r *Repo) ListLFSPointers() (pointers []string, err error) {
	lines, err := r.git(nil, "lfs", "ls-files")
	if err != nil {
		return nil, err
	}
	prefix := []byte(r.prefix)
	for lines != nil {
		line := scanLine(&lines)
		if len(line) == 0 {
			continue
		}
		parts := bytes.Fields(line)
		if len(parts) != 3 {
			return nil, fmt.Errorf("malformed git lfs ls-files output %q", line)
		}
		if !bytes.HasPrefix(parts[2], prefix) {
			log.Debug.Printf("skipping LFS file %s: not in repo's prefix %s", parts[2], prefix)
			continue
		}
		path := bytes.TrimPrefix(parts[2], prefix)
		pointers = append(pointers, string(path))
	}
	return
}

// CopyLFSObject copies the object referred to by the provided pointer
// from the given source repository.
func (r *Repo) CopyLFSObject(src *Repo, pointer string) error {
	p, err := ioutil.ReadFile(r.path(r.prefix, pointer))
	if err != nil {
		return err
	}
	var (
		q    = p
		line []byte
		oid  string
	)
	for q != nil {
		line = scanLine(&q)
		if !bytes.HasPrefix(line, []byte("oid ")) {
			continue
		}
		id, err := digest.Parse(string(line[4:]))
		if err != nil {
			return err
		}
		oid = id.Hex()
		break
	}
	if oid == "" {
		return errors.New("pointer file is missing oid")
	}
	opath := r.path(".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	// Do we already have the object?
	if _, err := os.Stat(opath); err == nil {
		log.Debug.Printf("object %s for pointer %s already exists", oid[:7], pointer)
		return nil
	}
	log.Debug.Printf("copying object %s for pointer %s", oid[:7], pointer)
	os.MkdirAll(filepath.Dir(opath), 0700)
	tmp, err := os.Create(opath + ".grit")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := src.gitIO(bytes.NewReader(p), tmp, "lfs", "smudge"); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), opath)
}

func (r *Repo) path(elems ...string) string {
	return filepath.Join(append([]string{r.root}, elems...)...)
}

func (r *Repo) git(stdin []byte, arg ...string) ([]byte, error) {
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	err := r.gitIO(in, &out, arg...)
	return out.Bytes(), err
}

// GitIO invokes a git command on the repository r. The provided
// arguments are passed to "git"; reader stdin is plumbed to the
// process input and its output is written to writer stdout. If an
// error occurs during the invocation of the "git" command, its
// standard error is included in the returned error.
func (r *Repo) gitIO(stdin io.Reader, stdout io.Writer, arg ...string) error {
	args := []string{"-C", r.root}
	for k, v := range r.config {
		args = append(args, "-c")
		args = append(args, k+"="+v)
	}
	args = append(args, arg...)
	cmd := exec.Command("git", args...)
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdin = stdin
	if len(arg) > 0 && arg[0] != "lfs" {
		cmd.Env = append(os.Environ(), "GIT_LFS_SKIP_SMUDGE=1")
	}
	log.Debug.Printf("%s: git %s", r.root, strings.Join(arg, " "))
	if err := cmd.Run(); err != nil {
		outerr := string(stderr.Bytes())
		if len(outerr) > 0 {
			outerr = "\n" + outerr
		}
		return fmt.Errorf("%s: git %s: error: %v%s", r.root, strings.Join(arg, " "), err, outerr)
	}
	outerr := string(stderr.Bytes())
	if len(outerr) > 0 {
		outerr = "\n" + outerr
	}
	log.Debug.Printf("%s: git %s: ok%s", r.root, strings.Join(arg, " "), outerr)
	return nil
}

// Header is a commit header.
type Header struct{ K, V string }

// Commit represents a single commit.
type Commit struct {
	// Digest is the git hash for the commit.
	Digest digest.Digest
	// Headers is the set of headers present in the commit.
	Headers []Header
	// Body is the commit message.
	Body string

	repo *Repo
}

var shipitRe = regexp.MustCompile(`(?:fb)?shipit-source-id: ([a-z0-9]+)`)

// ShipitID returns the shipit ID, if any.
func (c *Commit) ShipitID() (ids []string) {
	for _, g := range shipitRe.FindAllStringSubmatch(c.Body, -1) {
		if len(g) != 2 {
			log.Fatalf("invalid commit %s (%+v)", c, g)
			panic("not reached")
		}
		ids = append(ids, g[1])
	}
	return
}

// String returns a "one-line" commit message.
func (c *Commit) String() string {
	return fmt.Sprintf("%s: %s", c.Digest.Short(), c.Title())
}

// Title returns the commit's title -- the first line of its body.
func (c *Commit) Title() string {
	return strings.SplitN(c.Body, "\n", 2)[0]
}
