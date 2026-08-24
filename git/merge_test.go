package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grailbio/testutil"
)

// mergeFixture builds a throwaway checkout exercising merge reconciliation
// plumbing. The layout mirrors the reconciliation flow's expectations: a
// diverged side branch, a hand-resolved (evil) merge on master, and a
// fast-forward-free ours-strategy merge for discard semantics.
type mergeFixture struct {
	t    *testing.T
	dir  string // working checkout rooted at dir/master
	repo *Repo  // Repo view with the requested prefix
}

func (f *mergeFixture) rev(rev string) string {
	f.t.Helper()
	out, err := exec.Command("git", "-C", filepath.Join(f.dir, "master"), "rev-parse", rev).CombinedOutput()
	if err != nil {
		f.t.Fatalf("rev-parse %s: %v\n%s", rev, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *mergeFixture) head() string { return f.rev("master") }

// newMergeFixture creates a repository whose master history is:
//
//	base            proj/f.txt=base, proj/k.txt=keep
//	  ├─ main       proj/f.txt=main
//	  └─ side       proj/f.txt=side, proj/g.txt=g
//	evil merge      proj/f.txt=evil (hand resolution), takes side's g
//
// It returns a fixture viewing the checkout with the given prefix.
func newMergeFixture(t *testing.T, prefix string) *mergeFixture {
	t.Helper()
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		mkdir proj
		echo base > proj/f.txt
		echo keep > proj/k.txt
		git add .
		git commit -m'base'
		git branch side

		echo main > proj/f.txt
		git add .
		git commit -m'main edit'

		git checkout -q side
		echo side > proj/f.txt
		echo g > proj/g.txt
		git add .
		git commit -m'side work'

		git checkout -q master
		git merge --no-ff --no-commit side || true
		echo evil > proj/f.txt
		git add .
		git commit -m'evil merge'
	`)
	f := &mergeFixture{t: t, dir: dir,
		repo: &Repo{url: filepath.Join(dir, "origin.git"),
			root: filepath.Join(dir, "master"), prefix: prefix, branch: "master"}}
	return f
}

func TestParents(t *testing.T) {
	f := newMergeFixture(t, "")
	root := f.rev("master~2")

	parents, err := f.repo.Parents(f.head())
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 2 {
		t.Fatalf("merge parents = %v, want two entries", parents)
	}
	if got, want := parents[0], f.rev("master^1"); got != want {
		t.Errorf("first parent = %s, want %s", got, want)
	}
	if got, want := parents[1], f.rev("master^2"); got != want {
		t.Errorf("second parent = %s, want %s", got, want)
	}

	parents, err = f.repo.Parents(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 0 {
		t.Fatalf("root commit parents = %v, want none", parents)
	}
}

func TestMergeChangesetEvilMerge(t *testing.T) {
	f := newMergeFixture(t, "proj/")
	head := f.head()

	parents, err := f.repo.Parents(head)
	if err != nil {
		t.Fatal(err)
	}
	entries, removals, err := f.repo.MergeChangeset(head, parents, "d/")
	if err != nil {
		t.Fatal(err)
	}
	wantF := TreeEntry{Path: "d/f.txt", Mode: "100644", Blob: f.rev(head + ":proj/f.txt")}
	wantG := TreeEntry{Path: "d/g.txt", Mode: "100644", Blob: f.rev(head + ":proj/g.txt")}
	if len(entries) != 2 || entries[0] != wantF || entries[1] != wantG {
		t.Fatalf("entries = %+v, want [%+v %+v]", entries, wantF, wantG)
	}
	if len(removals) != 0 {
		t.Fatalf("removals = %v, want none", removals)
	}
}

func TestMergeChangesetOursStrategyDiscards(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		mkdir proj
		echo base > proj/f.txt
		git add .
		git commit -m'base'
		git branch side

		git checkout -q side
		echo side > proj/f.txt
		echo novel > proj/n.txt
		git add .
		git commit -m'side work'

		git checkout -q master
		git merge -s ours --no-ff -m'discard side' side
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "proj/", branch: "master"}
	head, err := r.RevParse("master")
	if err != nil {
		t.Fatal(err)
	}
	parents, err := r.Parents(head)
	if err != nil {
		t.Fatal(err)
	}
	entries, removals, err := r.MergeChangeset(head, parents, "")
	if err != nil {
		t.Fatal(err)
	}
	// The ours strategy reverts f.txt to the base content (recorded as an
	// override back to the first parent's blob) and deletes the side
	// branch's new file outright.
	wantF := TreeEntry{Path: "f.txt", Mode: "100644", Blob: f_rev(t, dir, head+":proj/f.txt")}
	if len(entries) != 1 || entries[0] != wantF {
		t.Fatalf("entries = %+v, want [%+v]", entries, wantF)
	}
	if len(removals) != 1 || removals[0] != "n.txt" {
		t.Fatalf("removals = %v, want [n.txt]", removals)
	}
}

// TestMergeChangesetRelocatesRemovalsUnderDestinationPrefix pins that
// removals are destination-relative like entries, not root-level paths.
func TestMergeChangesetRelocatesRemovalsUnderDestinationPrefix(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		mkdir proj
		echo base > proj/f.txt
		git add .
		git commit -m'base'
		git branch side

		git checkout -q side
		echo side > proj/f.txt
		echo doomed > proj/doomed.txt
		git add .
		git commit -m'side work'

		git checkout -q master
		git merge -s ours --no-ff -m'discard side' side
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "proj/", branch: "master"}
	head, err := r.RevParse("master")
	if err != nil {
		t.Fatal(err)
	}
	parents, err := r.Parents(head)
	if err != nil {
		t.Fatal(err)
	}
	entries, removals, err := r.MergeChangeset(head, parents, "d/")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, "d/") {
			t.Errorf("entry path %q is not relocated under the destination prefix", e.Path)
		}
	}
	if len(removals) != 1 || removals[0] != "d/doomed.txt" {
		t.Fatalf("removals = %v, want [d/doomed.txt]: removals must carry the destination prefix like entries", removals)
	}
}

// TestMergeChangesetUnslashedPrefix pins that "proj" round-trips against
// "proj/f.txt" as an entry for f.txt, not a phantom removal.
func TestMergeChangesetUnslashedPrefix(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		mkdir proj
		echo base > proj/f.txt
		git add .
		git commit -m'base'
		git branch side

		git checkout -q side
		echo side > proj/f.txt
		git add .
		git commit -m'side work'

		git checkout -q master
		git merge --no-ff --no-commit side >/dev/null 2>&1 || true
		echo evil > proj/f.txt
		git add .
		git commit -m'evil merge'
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "proj", branch: "master"}
	head, err := r.RevParse("master")
	if err != nil {
		t.Fatal(err)
	}
	parents, err := r.Parents(head)
	if err != nil {
		t.Fatal(err)
	}
	entries, removals, err := r.MergeChangeset(head, parents, "")
	if err != nil {
		t.Fatal(err)
	}
	wantF := TreeEntry{Path: "f.txt", Mode: "100644", Blob: f_rev(t, dir, head+":proj/f.txt")}
	if len(entries) != 1 || entries[0] != wantF {
		t.Fatalf("entries = %+v, want [%+v]", entries, wantF)
	}
	if len(removals) != 0 {
		t.Fatalf("removals = %v, want none: an unslashed prefix must not turn modifications into deletions", removals)
	}
}

func f_rev(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", filepath.Join(dir, "master"), "rev-parse", rev).CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v\n%s", rev, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestMergeChangesetIgnoresOutOfPrefixPaths(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		echo base > seed.txt
		git add .
		git commit -m'base'
		git branch side

		git checkout -q side
		echo s > sidefile.txt
		git add .
		git commit -m'side work'

		git checkout -q master
		echo m > mainfile.txt
		git add .
		git commit -m'main work'
		git merge -q --no-ff side
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "proj/", branch: "master"}
	head, err := r.RevParse("master")
	if err != nil {
		t.Fatal(err)
	}
	parents, err := r.Parents(head)
	if err != nil {
		t.Fatal(err)
	}
	entries, removals, err := r.MergeChangeset(head, parents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 || len(removals) != 0 {
		t.Fatalf("out-of-prefix merge produced changeset (%+v, %v), want empty", entries, removals)
	}
}

func TestMergeChangesetCaseMismatchAborts(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		mkdir proj
		echo base > proj/f.txt
		git add .
		git commit -m'base'
		git branch side

		git checkout -q side
		# Record the clashing spelling through index plumbing: on hosts
		# with case-insensitive filesystems, git add would fold the path
		# to the existing directory's casing, and the tree must hold
		# PROJ/clash.txt verbatim for the mismatch to exist at all.
		blob=$(echo clash | git hash-object -w --stdin)
		git update-index --add --cacheinfo 100644,$blob,PROJ/clash.txt
		git commit -q -m'case-clashing work'

		git checkout -q master
		git merge -q --no-ff side
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "proj/", branch: "master"}
	head, err := r.RevParse("master")
	if err != nil {
		t.Fatal(err)
	}
	parents, err := r.Parents(head)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = r.MergeChangeset(head, parents, "")
	if err == nil || !strings.Contains(err.Error(), "letter case") {
		t.Fatalf("case-clashing merge path errored %v, want loud letter-case failure", err)
	}
}

func TestDesiredTree(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		echo aaa > a.txt
		echo bbb > b.txt
		git add .
		git commit -m'base'
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "", branch: "master"}

	newBlob := hashObject(t, dir, "ccc\n")
	before, err := r.RevParse("HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.DesiredTree(
		[]TreeEntry{{Path: "sub/c.txt", Mode: "100644", Blob: newBlob}},
		[]string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}

	// The built tree carries the override and the removal and leaves
	// everything else alone.
	out, err := exec.Command("git", "-C", filepath.Join(dir, "master"), "ls-tree", "-r", tree).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree %s: %v\n%s", tree, err, out)
	}
	listing := string(out)
	if !strings.Contains(listing, "b.txt") {
		t.Errorf("built tree lost untouched path b.txt:\n%s", listing)
	}
	if !strings.Contains(listing, newBlob[:12]) {
		t.Errorf("built tree does not record overridden blob %s:\n%s", newBlob, listing)
	}
	if strings.Contains(listing, "a.txt") {
		t.Errorf("built tree retained removed path a.txt:\n%s", listing)
	}

	// The repository's real state is untouched: HEAD's tree, index, and
	// working tree all stay as they were.
	after, err := r.RevParse("HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("HEAD^{tree} moved during DesiredTree: %s -> %s", before, after)
	}
	if out, err := exec.Command("git", "-C", filepath.Join(dir, "master"), "diff-index", "--quiet", "HEAD").CombinedOutput(); err != nil {
		t.Errorf("real index dirty after DesiredTree: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "master", "a.txt")); err != nil {
		t.Errorf("working tree file a.txt disturbed by DesiredTree: %v", err)
	}
	for _, junk := range []string{"grit-index"} {
		matches, _ := filepath.Glob(filepath.Join(dir, "master", ".git", junk+"*"))
		if len(matches) > 0 {
			t.Errorf("temporary index left behind: %v", matches)
		}
	}
}

func TestDesiredTreeUnbornHeadFails(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `git init -q master`)
	r := &Repo{url: "irrelevant", root: filepath.Join(dir, "master"), prefix: "", branch: "master"}
	if _, err := r.DesiredTree(nil, nil); err == nil {
		t.Fatal("DesiredTree against an unborn HEAD must fail loudly")
	}
}

func TestTreeDiffs(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		echo aaa > a.txt
		echo bbb > b.txt
		git add .
		git commit -m'base'
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "", branch: "master"}

	// Identical trees produce no diffs.
	headTree, err := r.RevParse("HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := r.TreeDiffs(headTree)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("TreeDiffs against HEAD's own tree = %+v, want none", diffs)
	}

	newBlob := hashObject(t, dir, "ccc\n")
	tree, err := r.DesiredTree(
		[]TreeEntry{{Path: "sub/c.txt", Mode: "100644", Blob: newBlob}},
		[]string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	diffs, err = r.TreeDiffs(tree)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range diffs {
		got[d.Path] = true
		if !strings.Contains(string(d.Meta), "index ") {
			t.Errorf("diff for %s lacks a full index line: %s", d.Path, d.Meta)
		}
	}
	if !got["a.txt"] || !got["sub/c.txt"] {
		t.Fatalf("TreeDiffs paths = %v, want a.txt and sub/c.txt", got)
	}
	if len(diffs) != 2 {
		t.Fatalf("TreeDiffs produced %d diffs, want 2 (untouched b.txt excluded)", len(diffs))
	}
}

func TestCommitIdentityHeaders(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		export GIT_AUTHOR_DATE="1700000000 +0000"
		export GIT_COMMITTER_DATE="1700000000 +0000"
		echo x > x.txt
		git add .
		git commit -m'committed'
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "", branch: "master"}
	commits, err := r.LogIgnoringPrefix("-1", "--date=rfc2822", "master")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Fatalf("got %d commits, want 1", len(commits))
	}
	c := commits[0]
	if got, want := c.AuthorIdent(), "your name <you@example.com>"; got != want {
		t.Errorf("AuthorIdent = %q, want %q", got, want)
	}
	when, err := c.AuthorTime()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := when.Unix(), int64(1700000000); got != want {
		t.Errorf("AuthorTime = %d, want %d", got, want)
	}
}

func TestInPrefix(t *testing.T) {
	r := &Repo{prefix: "proj/"}
	for _, p := range []string{"proj/", "proj/f.txt", "proj/sub/f.txt"} {
		if !r.InPrefix(p) {
			t.Errorf("InPrefix(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"project.txt", "prose.txt", "projx/f.txt"} {
		if r.InPrefix(p) {
			t.Errorf("InPrefix(%q) = true, want false", p)
		}
	}
}

// TestMergeChangesetPrefixNamingAFile pins that a repo-root file named
// "proj" under prefix "proj" resolves as a tree entry, not a removal.
func TestMergeChangesetPrefixNamingAFile(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init --bare origin.git
		git clone origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"

		echo base > proj
		git add .
		git commit -m'base'
		git branch side

		git checkout -q side
		echo side > proj
		git add .
		git commit -m'side work'

		git checkout -q master
		git merge --no-ff --no-commit side >/dev/null 2>&1 || true
		echo evil > proj
		git add .
		git commit -m'evil merge'
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "proj", branch: "master"}
	head, err := r.RevParse("master")
	if err != nil {
		t.Fatal(err)
	}
	parents, err := r.Parents(head)
	if err != nil {
		t.Fatal(err)
	}
	entries, removals, err := r.MergeChangeset(head, parents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want the single prefixed-file entry", entries)
	}
	want := f_rev(t, dir, head+":proj")
	if entries[0].Blob != want || entries[0].Mode != "100644" {
		t.Fatalf("entry %+v does not pin the merge's own proj blob %s", entries[0], want)
	}
	if len(removals) != 0 {
		t.Fatalf("removals = %v, want none: a file named like the prefix must not become a deletion", removals)
	}
}

// hashObject shells out to git hash-object --stdin, giving tests an
// independent oracle for blob digests.
func hashObject(t *testing.T, dir, content string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", filepath.Join(dir, "master"), "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hash-object: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}
