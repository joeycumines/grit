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
	"path"
	"path/filepath"
	"regexp"
	"strconv"
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

// ErrStaleCloneCache reports that a clone cache held state that could
// not be preserved and was removed; Open recovers by recloning.
var ErrStaleCloneCache = errors.New("stale clone cache discarded")

// Dir is the directory in which git checkouts are made.
var Dir = "/var/tmp/grit"

// CachePath returns the clone directory grit derives for the provided
// endpoint. The key covers everything that changes the clone's
// semantics: two configurations sharing a URL but differing in branch
// or prefix must not share a working clone, or preservation logic could
// carry one configuration's unpushed state into the other's push.
func CachePath(url, prefix, branch string) string {
	base := filepath.Base(url)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	h := sha256.New()
	h.Write([]byte(url))
	h.Write([]byte{0})
	h.Write([]byte(prefix))
	h.Write([]byte{0})
	h.Write([]byte(branch))
	b := h.Sum(nil)
	// 128 bits of digest make cross-configuration collisions negligible
	// while keeping directory names reasonably short.
	return filepath.Join(Dir, fmt.Sprintf("%s%x", base, b[:16]))
}

// LegacyCachePath returns the clone directory that versions prior to the
// per-endpoint keying derived for a URL, for announcement purposes.
func LegacyCachePath(url string) string {
	base := filepath.Base(url)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(Dir, fmt.Sprintf("%s%02x%02x%02x%02x",
		base, sum[0], sum[1], sum[2], sum[3]))
}

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
	preserve   bool
}

// Open returns a repo representing the provided git remote url, branch, and
// prefix within the repository. The prefix is interpreted to provide
// a "view" into the git repository: all operations apply only to
// this prefix. The clone is a read-through cache of the remote: nothing
// is ever pushed from it, so its local state is always discarded in
// favor of the remote tip. Use OpenDestination for repositories whose
// local commits grit itself creates and must preserve across runs.
// Repositories are safe for concurrent operations across multiple uses
// on the same machine.
func Open(url, prefix, branch string) (*Repo, error) {
	return open(url, prefix, branch, false)
}

// OpenDestination behaves like Open but marks the clone as a
// synchronization destination: unpushed commits that grit authored (a
// paused or manually continued am session) survive across runs, while
// foreign unpushed state aborts loudly.
func OpenDestination(url, prefix, branch string) (*Repo, error) {
	return open(url, prefix, branch, true)
}

func open(url, prefix, branch string, preserve bool) (*Repo, error) {
	os.MkdirAll(Dir, 0700)
	path := CachePath(url, prefix, branch)
	r := &Repo{url: url, root: path, prefix: prefix, branch: branch, preserve: preserve}
	lock := flock.New(path + ".lock")
	if err := lock.Lock(context.Background()); err != nil {
		return nil, fmt.Errorf("lock %s: %v", path, err)
	}
	repo, err := r.openLocked()
	if errors.Is(err, ErrStaleCloneCache) {
		// The stale directory was removed; rebuild the clone under the
		// still-held lock and retry the open exactly once.
		os.MkdirAll(r.root, 0777)
		if _, cerr := r.git(nil, "clone", "--single-branch", "--branch", r.branch, r.url, r.root); cerr != nil {
			lock.Unlock()
			return nil, cerr
		}
		repo, err = r.openLocked()
	}
	if err != nil {
		// Open failures release the lock; only success hands its
		// lifetime to the caller (until Close).
		lock.Unlock()
		return nil, err
	}
	r.lock = lock
	return repo, nil
}

// CheckPrefixCasing verifies that every component of the configured
// prefix matches a path in HEAD's tree exactly. A component differing
// only by letter case would silently select nothing on tree-level
// pathspec matching and drop every diff on case-insensitive hosts;
// misconfiguration must abort loudly. Components that do not exist at
// all are allowed: they can legitimately appear later in the history
// during initial synchronization. Unborn repositories are permitted and
// handled by their callers.
func (r *Repo) CheckPrefixCasing(prefix string) error {
	p := strings.Trim(prefix, "/")
	if p == "" {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(p, "/") {
		var args []string
		if cur == "" {
			args = []string{"-c", "core.quotePath=false", "ls-tree", "-z", "--name-only", "HEAD"}
		} else {
			args = []string{"-c", "core.quotePath=false", "ls-tree", "-z", "--name-only", "HEAD:" + strings.TrimSuffix(cur, "/")}
		}
		out, err := r.git(nil, args...)
		if err != nil {
			// An unborn or otherwise unreadable HEAD is not a casing
			// problem; leave it to the caller's normal flows.
			return nil
		}
		var foldMatch string
		found := false
		for _, entry := range strings.Split(string(out), "\x00") {
			if entry == "" {
				continue
			}
			name := path.Base(entry)
			if name == part {
				found = true
				break
			}
			if foldMatch == "" && strings.EqualFold(name, part) {
				foldMatch = name
			}
		}
		if !found {
			if foldMatch != "" {
				have := p[:len(cur)]
				return fmt.Errorf("configured prefix %q does not match the repository: at HEAD, %q differs from %q only by letter case; fix the prefix spelling", p, have+foldMatch, have+part)
			}
			return nil
		}
		cur = cur + part + "/"
	}
	return nil
}

// pathInPrefix reports whether p lies within prefix's scope: equal to
// it, or nested beneath it when the prefix is slash-terminated or the
// next path byte is a slash. A bare string-prefix match ("docs"
// matching "docs.txt") must not count as containment: TrimPrefix would
// mangle such a path into a phantom sibling of the prefix directory.
func pathInPrefix(p, prefix string) bool {
	if prefix == "" {
		return true
	}
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	return len(p) == len(prefix) || strings.HasSuffix(prefix, "/") || p[len(prefix)] == '/'
}

// isCaseMismatch reports whether diffPath falls within prefix's scope
// but differs from it by letter case alone — a state that tree-level
// matching silently drops and that must therefore abort loudly.
// Comparison is rune-wise simple case folding without any lowercasing
// precheck, so fold-equivalent scripts (Greek sigma variants) count as
// case differences. The span must respect the same directory boundary
// as pathInPrefix; byte inequality of the leading span then
// distinguishes genuine mismatches from exact matches, which report
// false.
func isCaseMismatch(diffPath, prefix string) bool {
	if prefix == "" {
		return false
	}
	prefixRunes := []rune(prefix)
	diffRunes := []rune(diffPath)
	if len(diffRunes) < len(prefixRunes) {
		return false
	}
	for i, r := range prefixRunes {
		if !strings.EqualFold(string(diffRunes[i]), string(r)) {
			return false
		}
	}
	if len(diffRunes) > len(prefixRunes) &&
		prefixRunes[len(prefixRunes)-1] != '/' && diffRunes[len(prefixRunes)] != '/' {
		return false
	}
	return string(diffRunes[:len(prefixRunes)]) != prefix
}

// openLocked performs the open while the repository's lock is held.
func (r *Repo) openLocked() (*Repo, error) {
	// Announce caches abandoned by the key derivation change (they were
	// keyed by URL alone): paused sessions or resolved-but-unpushed work
	// left there by older versions require manual attention before they
	// are lost to cleanup.
	legacy := LegacyCachePath(r.url)
	if legacy != r.root {
		if _, err := os.Stat(legacy); err == nil {
			log.Printf("note: legacy clone cache %s is no longer used; inspect it for paused sessions before deleting", legacy)
		}
	}

	// Stat under the lock: another process may have created the clone
	// while this one waited for it. A leftover directory that never
	// became a git repository (crash mid-clone) is removed so the entry
	// cannot wedge forever.
	fresh := false
	if _, statErr := os.Stat(r.root); statErr != nil {
		if !os.IsNotExist(statErr) {
			return nil, statErr
		}
		fresh = true
	} else if _, gitErr := os.Stat(filepath.Join(r.root, ".git")); gitErr != nil {
		if !os.IsNotExist(gitErr) {
			return nil, gitErr
		}
		if err := os.RemoveAll(r.root); err != nil {
			return nil, err
		}
		fresh = true
		log.Printf("removed incomplete clone cache %s left by an interrupted run", r.root)
	}
	if fresh {
		os.MkdirAll(r.root, 0777)
		// Clone the configured branch itself: a plain --single-branch
		// clone checks out the remote's default branch, which leaves
		// refs/heads/<branch> missing whenever the configuration names
		// a different branch and breaks every later local operation
		// against it.
		if _, err := r.git(nil, "clone", "--single-branch", "--branch", r.branch, r.url, r.root); err != nil {
			return nil, err
		}
	}
	if _, err := r.git(nil, "fetch", "origin", r.branch); err != nil {
		return nil, err
	}
	// Capture the remote tip now: any later fetch (e.g. object seeding)
	// overwrites FETCH_HEAD. Nothing re-reads FETCH_HEAD afterwards —
	// ResetToRemote uses this snapshot — so the overwrite is harmless,
	// which keeps grit compatible with git versions lacking
	// --no-write-fetch-head.
	originHead, err := r.RevParse("FETCH_HEAD")
	if err != nil {
		return nil, err
	}
	r.originHead = originHead
	// Destination clones preserve work that must survive across grit
	// invocations: a paused git am session awaiting conflict resolution,
	// and commits that a continued session has already created but that
	// have not been pushed yet. Every such commit is authored by grit —
	// carrying a shipit id or a convergence-pruned marker, verified per
	// commit — and discarding it would destroy manually resolved work.
	// HEAD that is merely behind the remote tip is the normal case and
	// gets reset. Source clones are read-through caches: their local
	// state — including anything left behind by -linearize or a stray
	// session — is always discarded in favor of the remote tip, as is
	// the state of a freshly created clone, which simply starts wherever
	// the remote's default branch points and may be unrelated to the
	// configured branch.
	if r.preserve && !fresh {
		inProgress, err := r.InProgressAM()
		if err != nil {
			return nil, err
		}
		if inProgress {
			log.Printf("resuming interrupted git am session in %s", r.root)
			return r, nil
		}
		head, err := r.Head()
		if err != nil {
			return nil, err
		}
		ahead, err := r.IsAncestor(head, r.originHead)
		if err != nil {
			return nil, err
		}
		if !ahead {
			authored, err := r.PreservedTailIsGritAuthored(r.originHead)
			if err != nil {
				return nil, err
			}
			if authored {
				log.Printf("preserving %s: local commits from a resolved session have not been pushed yet", r.root)
				return r, nil
			}
			// Not connected to the remote tip and not entirely
			// grit-authored: either a foreign history or a stale
			// grit-only lineage stranded by an out-of-band rewrite.
			// The former must never be published; the latter is fully
			// re-derivable from the authoritative source. Both resolve
			// by discarding the disposable cache and recloning, which
			// cannot lose anything not already on the remote or in the
			// source repository.
			log.Printf("%s holds unpushed commits without grit authorship markers and does not descend from the remote tip; discarding stale clone cache and recloning", r.root)
			if err := os.RemoveAll(r.root); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %s", ErrStaleCloneCache, r.root)
		}
	}
	_, _ = r.git(nil, "am", "--abort")
	if _, err := r.git(nil, "reset", "--hard", r.originHead); err != nil {
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

// HasCommonAncestor reports whether the two revisions share any common
// ancestor, i.e., whether their histories intersect at all. Git documents
// merge-base's exit status 1 as "no merge base found", which is exactly the
// disjoint case; every other failure is an error rather than a silent
// negative.
func (r *Repo) HasCommonAncestor(a, b string) (bool, error) {
	_, err := r.git(nil, "merge-base", a, b)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// RevListExcluding returns the digests of commits reachable from tip but not
// reachable from any of the excluded revisions.
func (r *Repo) RevListExcluding(tip string, exclude ...string) ([]string, error) {
	args := append([]string{"rev-list", tip, "--not"}, exclude...)
	out, err := r.git(nil, args...)
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

// OriginHead returns the digest of the repository's remote tip for the
// configured branch, as of the most recent Open. The value is a
// snapshot: a concurrent third-party push makes comparisons against it
// stale, which is benign here — the next run re-syncs, and a stale push
// fails loudly rather than silently.
func (r *Repo) OriginHead() string {
	return r.originHead
}

var ownLineShipitIDRe = regexp.MustCompile(`(?m)^\s*(?:fb)?shipit-source-id: ([a-z0-9]+)\r?$`)

var ownLineGritTagRe = regexp.MustCompile(`(?m)^fbshipit-source-id: ([a-z0-9]+)\r?$`)

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

// OwnLineGritTagIDs returns the subset of ids written flush-left with
// the fb prefix — the exact serialization grit emits. Safety decisions
// about grit-authored state use this stricter form: prose quotations
// are conventionally indented or unprefixed, and a false positive here
// would publish foreign state.
func (c *Commit) OwnLineGritTagIDs() []string {
	var ids []string
	for _, match := range ownLineGritTagRe.FindAllStringSubmatch(c.Body, -1) {
		ids = append(ids, match[1])
	}
	return ids
}

var ownLineConvergenceMarkerRe = regexp.MustCompile(`(?m)^grit-convergence-pruned: ([0-9]+)/([0-9]+)\r?$`)

// OwnLineConvergenceMarkers reports the convergence-pruned markers grit
// writes on commits whose converged diffs were deliberately left
// untagged — the second serialization the authorship gates accept.
// Each entry is the clean "old/new" id pair; line-ending bytes never
// leak into the returned values, matching OwnLineShipitIDs and
// OwnLineGritTagIDs.
func (c *Commit) OwnLineConvergenceMarkers() []string {
	var markers []string
	for _, match := range ownLineConvergenceMarkerRe.FindAllStringSubmatch(c.Body, -1) {
		markers = append(markers, match[1]+"/"+match[2])
	}
	return markers
}

// PreservedTailIsGritAuthored reports whether every commit between the
// provided origin tip and HEAD is state grit itself created: carrying
// either a shipit source id or a convergence-pruned marker. The origin
// tip must be an ancestor of HEAD; a tail sharing only a deeper merge
// base has diverged from the remote tip and can never be pushed (every
// push fails non-fast-forward), so it reports false like any other
// unpreservable lineage rather than deferring the loss to a doomed
// push cycle. Unrelated histories report false as well: nothing held
// locally descends from such a tip.
func (r *Repo) PreservedTailIsGritAuthored(originHead string) (bool, error) {
	mbOut, err := r.git(nil, "merge-base", originHead, "HEAD")
	if err != nil {
		// Between valid revisions a merge-base failure means unrelated
		// histories (the reading IsAncestor documents); nothing local
		// descends from the remote tip.
		return false, nil
	}
	if mb := strings.TrimSpace(string(mbOut)); mb != originHead {
		return false, nil
	}
	commits, err := r.LogIgnoringPrefix(originHead + "..HEAD")
	if err != nil {
		return false, err
	}
	for _, c := range commits {
		if len(c.OwnLineGritTagIDs()) == 0 && len(c.OwnLineConvergenceMarkers()) == 0 {
			return false, nil
		}
	}
	return true, nil
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

// HeadEntry returns the git blob digest and file mode that HEAD's tree
// records for the provided repository-relative path. Paths absent from
// the tree return the zero digest with an empty mode, mirroring the
// index line of git diffs. Tree reads are inherently case-sensitive and
// reflect committed state only, so convergence decisions are immune to
// worktree leftovers (untracked files, conflict markers) and to the
// filesystem's case sensitivity; an incoming addition colliding with an
// untracked leftover instead fails loudly at application time.
func (r *Repo) HeadEntry(path string) (digest, mode string, err error) {
	// :(literal) disables pathspec globbing so metacharacters in file
	// names cannot alias sibling entries; tree reads are case-exact and
	// reflect committed state only.
	out, err := r.git(nil, "ls-tree", "HEAD", "--", ":(literal)"+path)
	if err != nil {
		return "", "", err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return zeroBlob, "", nil
	}
	if len(lines) > 1 {
		return "", "", fmt.Errorf("path %s is ambiguous in HEAD's tree (%d entries)", path, len(lines))
	}
	fields := strings.Fields(strings.SplitN(lines[0], "\t", 2)[0])
	if len(fields) < 3 {
		return "", "", fmt.Errorf("malformed ls-tree output for %s: %q", path, lines[0])
	}
	return fields[2], fields[0], nil
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
	if r.lock == nil {
		return nil
	}
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
// with the provided arguments, restricted to the repository's path prefix.
func (r *Repo) Log(args ...string) (commits []*Commit, err error) {
	args = append([]string{"log"}, args...)
	if r.prefix != "" {
		args = append(args, r.prefix)
	}
	out, err := r.git(nil, args...)
	if err != nil {
		// Tolerate exactly upstream's tolerated failure: a pathspec
		// absent from the working tree. Matching on git's message keeps
		// every other failure loud; a localized git would turn this one
		// case loud as well, which is the safe direction.
		if strings.Contains(err.Error(), "path not in the working tree") {
			log.Printf("%s: prefix %q is absent; treating log as empty", r.root, r.prefix)
			return nil, nil
		}
		return nil, err
	}
	return parseCommits(r, out)
}

// LogIgnoringPrefix behaves like Log but is never restricted to the
// repository's path prefix. Message-level state (such as shipit source
// ids) must be read through this method: worktree-dependent pathspecs
// would silently empty it when the prefix subtree is absent.
func (r *Repo) LogIgnoringPrefix(args ...string) ([]*Commit, error) {
	out, err := r.git(nil, append([]string{"log"}, args...)...)
	if err != nil {
		return nil, err
	}
	return parseCommits(r, out)
}

func parseCommits(r *Repo, out []byte) (commits []*Commit, err error) {
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
	fixPath := func(p string) string {
		// Strip the prefix, then a directory separator exposed by an
		// unslash-terminated source prefix ("proj" against "proj/f.txt"
		// leaves "/f.txt"); destination paths must be relative, whatever
		// spelling the source prefix used.
		rest := strings.TrimPrefix(p, r.prefix)
		return dstPrefix + strings.TrimPrefix(rest, "/")
	}

	var diffs []Diff
	for _, diff := range patch.Diffs {
		if pathInPrefix(diff.Path, r.prefix) {
			diff.Path = fixPath(diff.Path)
			// Also rewrite any --- or +++ meta lines that begin with a/ or b/,
			// since they are also paths. The rest of meta is opaque to us.
			diff.Meta = rewriteDiffMeta(diff.Meta, fixPath)
			diffs = append(diffs, diff)
		} else if isCaseMismatch(diff.Path, r.prefix) {
			// A case-insensitive host or a mistyped configuration would
			// otherwise silently drop every diff of a renamed prefix.
			log.Fatalf("diff path %s matches prefix %q only by letter case; fix the configured prefix", diff.Path, r.prefix)
		} else {
			log.Debug.Printf("dropping diff with path %s not in prefix %s", diff.Path, r.prefix)
		}
	}
	patch.Diffs = diffs
	return patch, nil
}

// rewriteDiffMeta rewrites the a/ and b/ pathnames inside a diff's
// metadata section through fixPath, reproducing every other line
// byte-for-byte — including per-line trailing carriage returns, which
// must survive round trips even though they never participate in
// prefix inspection. The result carries no trailing newline, matching
// Diff.Meta's contract.
func rewriteDiffMeta(meta []byte, fixPath func(string) string) []byte {
	var out []byte
	for meta != nil {
		line := scanLine(&meta)
		hasCR := bytes.HasSuffix(line, []byte{'\r'})
		if hasCR {
			line = line[:len(line)-1]
		}
		switch {
		case bytes.HasPrefix(line, prefixA):
			out = append(out, prefixA...)
			out = append(out, fixPath(string(line[len(prefixA):]))...)
		case bytes.HasPrefix(line, prefixB):
			out = append(out, prefixB...)
			out = append(out, fixPath(string(line[len(prefixB):]))...)
		case bytes.HasPrefix(line, []byte(`--- "a/`)), bytes.HasPrefix(line, []byte(`+++ "b/`)):
			// C-quoted form: `--- "a/P"` / `+++ "b/P"`. The escaped
			// path is decoded, rewritten, and re-emitted in git's exact
			// quoting grammar.
			escaped := strings.TrimSuffix(string(line[7:]), `"`)
			unquoted, err := strconv.Unquote(`"` + escaped + `"`)
			if err != nil {
				out = append(out, line...)
				break
			}
			side := "a"
			if bytes.HasPrefix(line, []byte("+++")) {
				side = "b"
			}
			fixed := fixPath(unquoted)
			out = append(out, line[:4]...)
			out = append(out, []byte(quotePath(side+"/"+fixed))...)
		default:
			out = append(out, line...)
		}
		if hasCR {
			out = append(out, '\r')
		}
		out = append(out, '\n')
	}
	return bytes.TrimSuffix(out, []byte{'\n'})
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

// PatchIsEmpty reports whether the commit named by the provided digest
// contains no file changes (--always format-patch emits headers even
// for empty commits). Output is requested NUL-delimited and tested at
// raw length: pathnames may consist entirely of whitespace, so any
// trimming of line-oriented output would erase a real change.
func (r *Repo) PatchIsEmpty(id string) (bool, error) {
	out, err := r.git(nil, "diff-tree", "--no-commit-id", "--name-only", "-z", "-r", "--root", id)
	if err != nil {
		return false, err
	}
	return len(out) == 0, nil
}

// CommitEmptyWithMessageFile creates an empty commit whose message is
// read from the provided repository-relative file, preserving the
// record of an applied patch that produced no changes.
func (r *Repo) CommitEmptyWithMessageFile(relPath string) error {
	_, err := r.git(nil, "commit", "--allow-empty", "-F", relPath)
	return err
}

// ResetToRemote discards all local state in favor of the remote tip as
// of the most recent Open (the originHead snapshot; a third-party push
// after Open is picked up by the next run). Used after a failed push: a
// non-fast-forward rejection means the destination advanced
// independently, and any locally applied commits were built on a stale
// base.
func (r *Repo) ResetToRemote() error {
	_, _ = r.git(nil, "am", "--abort")
	_, err := r.git(nil, "reset", "--hard", r.originHead)
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

// gitEnv behaves like git but runs the command with the provided extra
// environment entries layered over the process environment.
func (r *Repo) gitEnv(env []string, stdin []byte, arg ...string) ([]byte, error) {
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	err := r.gitIOEnv(env, in, &out, arg...)
	return out.Bytes(), err
}

// GitIO invokes a git command on the repository r. The provided
// arguments are passed to "git"; reader stdin is plumbed to the
// process input and its output is written to writer stdout. If an
// error occurs during the invocation of the "git" command, its
// standard error is included in the returned error.
func (r *Repo) gitIO(stdin io.Reader, stdout io.Writer, arg ...string) error {
	return r.gitIOEnv(nil, stdin, stdout, arg...)
}

func (r *Repo) gitIOEnv(env []string, stdin io.Reader, stdout io.Writer, arg ...string) error {
	args := []string{"-C", r.root}
	for k, v := range r.config {
		args = append(args, "-c")
		args = append(args, k+"="+v)
	}
	args = append(args, arg...)
	cmd := exec.Command("git", args...)
	cmd.Stdout = stdout
	cmd.Env = append(os.Environ(), env...)
	if len(arg) > 0 && arg[0] != "lfs" {
		cmd.Env = append(cmd.Env, "GIT_LFS_SKIP_SMUDGE=1")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdin = stdin
	log.Debug.Printf("%s: git %s", r.root, strings.Join(arg, " "))
	if err := cmd.Run(); err != nil {
		outerr := string(stderr.Bytes())
		if len(outerr) > 0 {
			outerr = "\n" + outerr
		}
		return fmt.Errorf("%s: git %s: error: %w%s", r.root, strings.Join(arg, " "), err, outerr)
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
