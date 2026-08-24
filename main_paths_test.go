package main_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnslashedPrefixDoesNotCaptureBoundarySiblings pins directory-boundary
// containment: bare HasPrefix mangles sibling "project.txt" into a phantom file.
func TestUnslashedPrefixDoesNotCaptureBoundarySiblings(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "in scope\n")
	src.write("project.txt", "boundary sibling\n")
	src.write("prose.txt", "unrelated\n")
	src.commit("first commit")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "pushing changes") {
		t.Fatalf("sync did not complete:\n%s", out)
	}
	dst.pull()
	tracked := dst.gitOut("ls-files")
	entries := strings.Split(strings.TrimSpace(tracked), "\n")
	if len(entries) != 1 || entries[0] != "f.txt" {
		t.Fatalf("destination must hold exactly f.txt; tracked:\n%s", tracked)
	}
	for _, phantom := range []string{"ect.txt", "project.txt", "prose.txt"} {
		if strings.Contains(tracked, phantom) {
			t.Fatalf("out-of-scope path leaked into destination as %q; tracked:\n%s", phantom, tracked)
		}
	}
}

// TestCaseOnlyRenameIsNotPruned pins case-sensitive convergence pruning: a
// case-only rename serializes as delete+add and must apply despite OS folding.
func TestCaseOnlyRenameIsNotPruned(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/readme", "contents\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Case-only rename: delete readme, add README with new content.
	runGit(t, src.dir, "rm", "proj/readme")
	src.write("proj/README", "renamed contents\n")
	src.commit("case-only rename")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if strings.Contains(out, "skipping converged") {
		t.Fatalf("case-only rename's add half was falsely pruned:\n%s", out)
	}
	dst.pull()

	// Verify through git: the worktree-level check cannot distinguish
	// cases on an insensitive filesystem.
	tracked := dst.gitOut("ls-files")
	if !strings.Contains(tracked, "README") || strings.Contains(tracked, "readme") {
		t.Fatalf("case-only rename did not land at destination; tracked files:\n%s", tracked)
	}
}

// TestCaseOnlySubdirRenameIsNotPruned pins intermediate-component case
// sensitivity: a case-only SUBDIRECTORY rename must apply, not prune.
func TestCaseOnlySubdirRenameIsNotPruned(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/sub/f.txt", "contents\n")
	src.write("proj/b.txt", "b\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	runGit(t, src.dir, "rm", "proj/sub/f.txt")
	src.write("proj/SUB/f.txt", "renamed contents\n")
	src.commit("case-only subdir rename")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if strings.Contains(out, "skipping converged") {
		t.Fatalf("subdir case-only rename's add half was falsely pruned:\n%s", out)
	}
	dst.pull()

	tracked := dst.gitOut("ls-files")
	if !strings.Contains(tracked, "SUB/f.txt") || strings.Contains(tracked, "sub/f.txt") {
		t.Fatalf("subdir case-only rename did not land; tracked files:\n%s", tracked)
	}
}

// TestCaseMismatchedPrefixAborts pins that a case-mismatched configured prefix
// aborts loudly instead of silently dropping every diff on case-insensitive hosts.
func TestCaseMismatchedPrefixAborts(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",PROJ/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	// Assert the CheckPrefixCasing-specific fragment: the generic
	// "letter case" wording is shared with the deeper Patch-side fatal,
	// so only this fragment proves the configuration guard fired.
	if !strings.Contains(out, "does not match the repository") {
		t.Fatalf("case-mismatched prefix did not abort loudly:\n%s", out)
	}
}

// TestPathsContainingSpaces pins spaced-path header parsing: modern git emits
// symmetric unquoted headers, and first-space truncation used to wedge runs.
func TestPathsContainingSpaces(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	src.write("proj/sp ace.txt", "spaced\n")
	src.commit("add file with space")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "applying") {
		t.Fatalf("spaced path did not apply:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	// An identical file arriving at the destination through a direct
	// commit while the source also adds it must prune as converged,
	// proving the spaced path survives parsing end to end.
	dst.write("sp 2.txt", "second\n")
	dst.commit("destination adds spaced file directly")
	dst.push()
	src.write("proj/sp 2.txt", "second\n")
	src.commit("source adds identical spaced file")
	src.push()
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "skipping converged sp 2.txt") {
		t.Fatalf("spaced-path convergence pruning did not fire:\n%s", out)
	}
}

// TestQuotedPathRoundTrip covers C-quoted diff-header paths (non-ASCII bytes):
// quoted parsing applies additions, and identical destination arrivals prune.
func TestQuotedPathRoundTrip(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	src.write("proj/caf\u00e9.txt", "unicode\n")
	src.commit("add non-ascii path")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "applying") {
		t.Fatalf("quoted path did not apply:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	// An identical non-ASCII path arriving at the destination through a
	// direct commit while the source also adds it must prune as
	// converged, proving quoted paths survive parsing end to end.
	dst.write("caf\u00e92.txt", "second\n")
	dst.commit("destination receives identical non-ascii file directly")
	dst.push()
	src.write("proj/caf\u00e92.txt", "second\n")
	src.commit("source adds identical second non-ascii file")
	src.push()
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "skipping converged caf\u00e92.txt") {
		t.Fatalf("quoted-path convergence pruning did not fire:\n%s", out)
	}
}

// TestCaseMismatchedPrefixComponentAborts covers multi-component prefixes: a
// wrong-cased intermediate component must abort, not silently select nothing.
func TestCaseMismatchedPrefixComponentAborts(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",PROJ/sub/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/sub/f.txt", "v1")
	src.commit("first commit")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	// CheckPrefixCasing-specific fragment: proves the configuration
	// guard (not the deeper Patch-side fatal) rejected this spec.
	if !strings.Contains(out, "does not match the repository") {
		t.Fatalf("multi-component case mismatch did not abort:\n%s", out)
	}
}

// TestNonASCIIPrefixComponentCasing pins CheckPrefixCasing against core.quotePath
// escaping: a correct non-ASCII component syncs; its case-typo aborts loudly.
func TestNonASCIIPrefixComponentCasing(t *testing.T) {
	for _, tc := range []struct {
		prefix    string
		wantAbort bool
	}{
		{"caf\u00e9/", false},
		{"CAF\u00c9/", true},
	} {
		src, dst := setupGritRepos(t)

		srcSpec := src.bare + "," + tc.prefix + "," + testBranch
		dstSpec := dst.bare + ",," + testBranch

		src.write("caf\u00e9/f.txt", "v1\n")
		src.commit("first commit")
		src.push()

		out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
		aborted := strings.Contains(out, "only by letter case")
		if tc.wantAbort && !aborted {
			t.Fatalf("prefix %q: case mismatch did not abort:\n%s", tc.prefix, out)
		}
		if !tc.wantAbort && aborted {
			t.Fatalf("prefix %q: correct spelling was rejected:\n%s", tc.prefix, out)
		}
	}
}

// TestPatchSideLetterCaseFatal pins the deeper Patch-side guard: a case-typo'd
// subdirectory added after an initial sync hits the diff-path fatal.
func TestPatchSideLetterCaseFatal(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	gritOutputStrict(t, src.gritBin, srcSpec, dstSpec)

	// One commit touches base.txt and adds a case-typo'd subdirectory: the
	// pathspec selects it, format-patch surfaces both diffs to Patch, and the
	// typo hits the letter-case fatal (CheckPrefixCasing already passed).
	//
	// Index plumbing stages the typo because a case-insensitive filesystem
	// would fold a worktree write into the existing lowercase directory.
	src.write("proj/base.txt", "v2")
	gitIn := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = src.dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	blob := strings.TrimSpace(gitIn("hash-object", "-w", "--stdin"))
	gitIn("update-index", "--add", "--cacheinfo", "100644,"+blob+",PROJ/g.txt")
	runGit(t, src.dir, "add", "proj/base.txt")
	runGit(t, src.dir, "commit", "-m", "case-typo'd subdir alongside in-prefix change")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "only by letter case") {
		t.Fatalf("patch-side case fatal did not fire:\n%s", out)
	}
	if strings.Contains(out, "does not match the repository") {
		t.Fatalf("configuration guard fired instead of the patch-side guard:\n%s", out)
	}
}
