// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCaseOnlyRenameIsNotPruned pins case-sensitive path resolution in
// convergence pruning: on case-insensitive filesystems, a case-only
// rename serializes as delete+add, and the add half must not be pruned
// just because the OS resolves it to the differently-cased existing
// file. Without exact-name matching, the destination silently loses the
// file.
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

// TestCaseOnlySubdirRenameIsNotPruned pins case sensitivity for
// intermediate path components: a case-only SUBDIRECTORY rename arrives
// as delete+add, and on a case-insensitive host the add half resolves
// through the still-existing old-case directory. Pruning it would
// silently drop the file; it must be applied instead.
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

// TestCaseMismatchedPrefixAborts pins that a configured prefix whose
// letter case does not match the repository's paths aborts loudly
// instead of silently dropping every diff on case-insensitive hosts.
func TestCaseMismatchedPrefixAborts(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",PROJ/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()

	out := gritOutput(t, src.gritBin, "-push", srcSpec, dstSpec)
	if !strings.Contains(out, "letter case") {
		t.Fatalf("case-mismatched prefix did not abort loudly:\n%s", out)
	}
}

// TestPathsContainingSpaces pins diff-header parsing for paths with
// spaces: modern git emits symmetric unquoted headers, and a header
// truncated at the first space used to wedge every subsequent run. The
// addition must apply, and an identical re-add must prune as converged.
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
	// commit while the source also adds it must prune as converged —
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

// TestQuotedPathRoundTrip covers paths git C-quotes in diff headers
// (non-ASCII bytes): the addition must apply through grit's quoted
// parsing and emission, and an identical arrival at the destination
// must prune as converged.
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
	// converged — proving quoted paths survive parsing end to end.
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

// TestCaseMismatchedPrefixComponentAborts covers multi-component
// prefixes: a wrong-cased intermediate component must abort loudly even
// though tree-level pathspecs would silently select nothing.
func TestCaseMismatchedPrefixComponentAborts(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",PROJ/sub/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/sub/f.txt", "v1")
	src.commit("first commit")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "only by letter case") {
		t.Fatalf("multi-component case mismatch did not abort:\n%s", out)
	}
}

// TestNonASCIIPrefixComponentCasing pins CheckPrefixCasing against
// core.quotePath escaping: a correctly spelled non-ASCII prefix
// component must be recognized (sync proceeds), and its case-typo must
// abort loudly.
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
