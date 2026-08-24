package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A TreeEntry records the desired state of a single path within a tree:
// its repository-relative path, its mode, and its blob digest.
type TreeEntry struct {
	Path string
	Mode string
	Blob string
}

// Parents returns the digests of the provided commit's parents, in
// order. A root commit has no parents.
func (r *Repo) Parents(commit string) ([]string, error) {
	out, err := r.git(nil, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return nil, fmt.Errorf("no output for commit %s", commit)
	}
	// The first token is the commit itself; the rest are its parents.
	return fields[1:], nil
}

// IsMerge reports whether the provided commit has more than one parent.
func (r *Repo) IsMerge(commit string) (bool, error) {
	parents, err := r.Parents(commit)
	if err != nil {
		return false, err
	}
	return len(parents) > 1, nil
}

// InPrefix reports whether the provided repository-relative path lies
// within the repo's prefix view: equal to the prefix or nested beneath
// it, respecting directory boundaries.
func (r *Repo) InPrefix(p string) bool {
	return pathInPrefix(p, r.prefix)
}

// scopedPath pairs a candidate path's repository-full form with its
// prefix-stripped form: the full path addresses git exactly as the tree
// stores it, the stripped form relocates the path under the destination
// prefix.
type scopedPath struct {
	full string
	rel  string
}

// MergeChangeset computes what reconciling the merge commit into a
// destination requires, as viewed through dstPrefix: entries whose
// content and mode must hold after the merge, and paths that must be
// removed. Candidate paths are those changed between the merge and any
// of its parents; a candidate absent from the merge's own tree is a
// removal (an "-s ours" discard, for instance). A path matching the
// repo's prefix only by letter case aborts loudly rather than being
// silently dropped by tree-level matching.
//
// Entries and removals both carry destination-relative paths: entries
// pin the merge tree's blobs and modes, removals name the paths to
// clear. An empty result means the merge touches nothing in scope.
func (r *Repo) MergeChangeset(commit string, parents []string, dstPrefix string) ([]TreeEntry, []string, error) {
	candidates := map[string]bool{}
	for _, p := range parents {
		out, err := r.git(nil, "diff-tree", "-r", "--name-only", "--no-renames", "-z", p, commit)
		if err != nil {
			return nil, nil, err
		}
		for _, path := range strings.Split(string(out), "\x00") {
			if path != "" {
				candidates[path] = true
			}
		}
	}
	sorted := make([]string, 0, len(candidates))
	for p := range candidates {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	var inScope []scopedPath
	for _, p := range sorted {
		if !pathInPrefix(p, r.prefix) {
			if isCaseMismatch(p, r.prefix) {
				return nil, nil, fmt.Errorf("merge %s changes %s which matches prefix %q only by letter case; fix the configured prefix", commit, p, r.prefix)
			}
			continue
		}
		inScope = append(inScope, scopedPath{
			full: p,
			rel:  strings.TrimPrefix(strings.TrimPrefix(p, r.prefix), "/"),
		})
	}
	if len(inScope) == 0 {
		return nil, nil, nil
	}

	specs := make([]string, len(inScope))
	for i, s := range inScope {
		specs[i] = ":(literal)" + s.full
	}
	out, err := r.git(nil, append([]string{"ls-tree", "-r", "-z", commit, "--"}, specs...)...)
	if err != nil {
		return nil, nil, err
	}
	state := map[string]TreeEntry{}
	for _, line := range strings.Split(string(out), "\x00") {
		if line == "" {
			continue
		}
		meta, p, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, nil, fmt.Errorf("malformed ls-tree output %q", line)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return nil, nil, fmt.Errorf("malformed ls-tree entry %q", line)
		}
		state[p] = TreeEntry{Path: p, Mode: fields[0], Blob: fields[2]}
	}
	var (
		entries  []TreeEntry
		removals []string
	)
	for _, s := range inScope {
		if e, ok := state[s.full]; ok {
			e.Path = dstPrefix + s.rel
			entries = append(entries, e)
		} else {
			removals = append(removals, dstPrefix+s.rel)
		}
	}
	return entries, removals, nil
}

// DesiredTree builds and returns a tree object capturing the provided
// overrides on top of HEAD's current tree: each entry pins its path to
// the given blob and mode; each removal deletes its path. HEAD must
// resolve; an unborn repository fails loudly.
//
// The build runs against a temporary index so that the repository's
// real index and working tree are untouched; the temporary index is
// removed before return. Callers compare the result against
// HEAD^{tree} to detect merges that change nothing in scope.
func (r *Repo) DesiredTree(entries []TreeEntry, removals []string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Join(r.root, ".git"), "grit-index")
	if err != nil {
		return "", err
	}
	indexFile := tmp.Name()
	tmp.Close()
	defer os.Remove(indexFile)
	env := []string{"GIT_INDEX_FILE=" + indexFile}
	if _, err := r.gitEnv(env, nil, "read-tree", "HEAD"); err != nil {
		return "", fmt.Errorf("read-tree HEAD: %v", err)
	}
	for _, e := range entries {
		cacheinfo := fmt.Sprintf("%s,%s,%s", e.Mode, e.Blob, e.Path)
		if _, err := r.gitEnv(env, nil, "update-index", "--add", "--cacheinfo", cacheinfo); err != nil {
			return "", fmt.Errorf("update-index --cacheinfo %s: %v", cacheinfo, err)
		}
	}
	for _, p := range removals {
		if _, err := r.gitEnv(env, nil, "update-index", "--force-remove", p); err != nil {
			return "", fmt.Errorf("update-index --force-remove %s: %v", p, err)
		}
	}
	out, err := r.gitEnv(env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write-tree: %v", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// TreeDiffs returns the diffs that take HEAD's tree to the provided
// tree, parsed through the same machinery that reads serialized
// patches: each Diff's Meta carries the extended header lines up to
// the first hunk header (including full index lines), and Body carries
// everything from there on. Identical trees produce no diffs.
func (r *Repo) TreeDiffs(tree string) ([]Diff, error) {
	headTree, err := r.RevParse("HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	if headTree == tree {
		return nil, nil
	}
	out, err := r.git(nil, "diff-tree", "-p", "--binary", "--full-index",
		"--no-renames", "--no-commit-id", headTree, tree)
	if err != nil {
		return nil, err
	}
	var diffs []Diff
	err = foreach(out, "diff", func(section []byte) error {
		header := scanLine(&section)
		path := parseDiffHeader(header)
		if path == nil {
			return errors.New("diff is missing header")
		}
		meta := next(&section, "@@")
		diffs = append(diffs, Diff{Path: string(path), Meta: meta, Body: section})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return diffs, nil
}

// AuthorIdent returns the commit's author ident in git's
// "Name <email>" form.
func (c *Commit) AuthorIdent() string {
	for _, h := range c.Headers {
		if h.K == "Author" {
			return h.V
		}
	}
	return ""
}

// AuthorTime returns the commit's author date.
func (c *Commit) AuthorTime() (time.Time, error) {
	for _, h := range c.Headers {
		if h.K == "Date" {
			return time.Parse(gitTimeLayout, h.V)
		}
	}
	return time.Time{}, errors.New("commit is missing a Date header")
}
