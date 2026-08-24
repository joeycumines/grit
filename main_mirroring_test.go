// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDuplicateAddConvergence replicates the eventloop fuzz-testdata
// collision: content arrives at the destination through a direct commit,
// and a later source commit adds identical bytes. The duplicate diff must
// be pruned (no conflict, no empty commit) while the rest of the commit
// applies, and a rerun must reach a stable nothing-to-do fixed point.
func TestDuplicateAddConvergence(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Content arrives at the destination through a direct commit.
	payload := "payload-bytes\n"
	dst.write("data.bin", payload)
	dst.commit("direct destination commit")
	dst.push()

	// A source commit adds the identical file AND a real change.
	src.write("proj/data.bin", payload)
	src.write("proj/base.txt", "v2")
	src.commit("side work with duplicate add")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "skipping converged data.bin") {
		t.Fatalf("prune did not drop the identical addition (this is what distinguishes pruning from am --3way's own duplicate handling):\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	// The applied commit carries the real change but — being partially
	// pruned — deliberately no shipit tag, so it stays re-examinable.
	tip := strings.TrimSpace(dst.gitOut("log", "-1", "--format=%B"))
	if strings.Contains(tip, "fbshipit-source-id: ") {
		t.Fatalf("partially-pruned commit was tagged: %q", tip)
	}
	if !strings.Contains(tip, "side work with duplicate add") {
		t.Fatalf("destination tip is not the mirrored commit: %q", tip)
	}

	// Fixed point: a rerun has nothing to do and pushes nothing.
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("rerun did not reach a fixed point:\n%s", out)
	}
}

// TestPartialDriftThreeWay verifies that a patch whose textual
// context has drifted at the destination is rescued by three-way merge:
// the destination edits line 5 — inside the hunk context of the source's
// edit to lines 8-9 — so plain git am fails (verified against raw git),
// while am --3way reconstructs the base tree from the patch's index
// lines and merges both changes.
func TestPartialDriftThreeWay(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i+1)
	}
	src.write("proj/f.txt", strings.Join(lines, "\n")+"\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// The destination drifts line 5, inside the context of the source's
	// upcoming hunk.
	lines[4] = "dst5"
	dst.write("f.txt", strings.Join(lines, "\n")+"\n")
	dst.commit("destination drifts line 5")
	dst.push()

	// The source edits lines 8 and 9.
	lines[7] = "src8"
	lines[8] = "src9"
	lines[4] = "line5"
	src.write("proj/f.txt", strings.Join(lines, "\n")+"\n")
	src.commit("source edits lines 8-9")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if strings.Contains(out, "conflict") {
		t.Fatalf("three-way merge should have converged the drift:\n%s", out)
	}
	dst.pull()

	got := strings.Split(strings.TrimSpace(dstRead(t, dst.dir, "f.txt")), "\n")
	if got[4] != "dst5" || got[7] != "src8" || got[8] != "src9" {
		t.Fatalf("three-way merge lost changes: lines 5,8,9 = %q %q %q", got[4], got[7], got[8])
	}
}

// TestTagSetExclusionExactlyOnce verifies that shipit-tag state makes
// processing exactly-once across runs: after two sibling commits are
// applied, a later incremental run re-selects the first sibling (it is
// not an ancestor of the new anchor, which sits atop the second
// sibling) but must exclude it by its recorded tag rather than replay
// it. Also asserts that new tags record the full source digest.
func TestTagSetExclusionExactlyOnce(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Two sibling branches off the synced base.
	src.git("branch", "s1")
	src.git("checkout", "s1")
	src.write("proj/a.txt", "a")
	src.commit("sibling one")
	src.git("checkout", testBranch)
	src.git("branch", "s2")
	src.git("checkout", "s2")
	src.write("proj/b.txt", "b")
	src.commit("sibling two")
	src.git("checkout", testBranch)
	src.git("merge", "--no-ff", "-m", "merge s1", "s1")
	src.git("merge", "--no-ff", "-m", "merge s2", "s2")
	src.push()

	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	tip := strings.TrimSpace(dst.gitOut("log", "--grep=fbshipit-source-id:", "-n", "1", "--format=%B"))
	if !regexpFullDigest.MatchString(tip) {
		t.Fatalf("newest tag is not a full 40-hex digest: %q", tip)
	}

	// A mainline commit on top of sibling two: the next run must exclude
	// sibling one by tag (it is not an ancestor of the new anchor).
	src.write("proj/c.txt", "c")
	src.commit("mainline child of sibling two")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "skipping already synchronized") {
		t.Fatalf("tag-set exclusion did not fire:\n%s", out)
	}
	// The siblings must be excluded by their tags, not by content
	// pruning. The oracle names the per-path prune signature: merges in
	// the re-selected range legitimately report their own state
	// convergence ("skipping converged merge ..."), which shares no
	// path with the regular-commit message.
	if strings.Contains(out, "skipping converged a.txt") || strings.Contains(out, "skipping converged b.txt") {
		t.Fatalf("regular-commit prune fired where tag exclusion should have:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("no fixed point after exclusion:\n%s", out)
	}
}

// TestLegacyAbbreviatedAnchorSuppressesReplay verifies that a
// destination commit carrying a legacy abbreviated (7-hex) shipit id
// resolves as the resume anchor, bounding the selection range so its
// source commit is not replayed. Prefix-matching of abbreviated ids
// during tag-set exclusion is covered by TestIsProcessed.
func TestLegacyAbbreviatedAnchorSuppressesReplay(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// An unsynced source commit...
	src.write("proj/x.txt", "x")
	src.commit("unsynced source commit")
	src.push()
	series := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))

	// ...whose content was already brought to the destination manually,
	// tagged with a legacy abbreviated id.
	dst.write("x.txt", "x")
	dst.git("add", "x.txt")
	dst.git("commit", "-m",
		fmt.Sprintf("manual import\n\nfbshipit-source-id: %s\n", series[:7]))
	dst.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("legacy tag did not suppress replay of the source commit:\n%s", out)
	}
	if !strings.Contains(out, "0 commits to copy") {
		t.Fatalf("expected the legacy anchor to bound the selection:\n%s", out)
	}
}

// TestFullyConvergedCommitSkipped verifies the whole-commit skip path:
// a source commit whose every diff already matches the destination
// creates no destination commit, is reported in the skip accounting, and
// leaves the repository at a stable nothing-to-do fixed point.
func TestFullyConvergedCommitSkipped(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// The destination already contains the change through a direct
	// commit.
	dst.write("only.txt", "already here\n")
	dst.commit("direct destination commit")
	dst.push()

	// The source commit's sole diff is identical content.
	src.write("proj/only.txt", "already here\n")
	src.commit("fully converged source commit")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "skipping converged only.txt") {
		t.Fatalf("converged diff was not pruned:\n%s", out)
	}
	if !strings.Contains(out, "1 commits skipped as already converged") {
		t.Fatalf("skip accounting missing:\n%s", out)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("fully converged commit should leave nothing to push:\n%s", out)
	}
	// No destination commit may exist for the skipped source commit.
	tip := strings.TrimSpace(dst.gitOut("log", "-1", "--format=%B"))
	if strings.Contains(tip, "fully converged source commit") {
		t.Fatalf("empty destination commit was created for a converged source commit: %q", tip)
	}

	// Dump mode previews the unpruned candidate set from the source side
	// only: pruning is state-dependent and therefore deliberately absent.
	dump := gritDumpOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(dump, "b/only.txt") {
		t.Fatalf("dump mode did not include the converged commit's diff:\n%s", dump)
	}
	if strings.Contains(dump, "skipping converged") {
		t.Fatalf("dump mode should not apply destination-state-dependent pruning:\n%s", dump)
	}

	// Converged commits are re-examined on every run rather than
	// permanently skipped: once the destination diverges from the source
	// commit's post-image, the diff stops being prunable and the commit
	// is genuinely reconsidered. For an identical-path content conflict
	// that reconsideration surfaces loudly.
	dst.write("only.txt", "diverged\n")
	dst.commit("destination diverges")
	dst.push()
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "conflict") {
		t.Fatalf("re-examined converged commit after divergence should surface loudly:\n%s", out)
	}
}

// TestFullHistorySelectionSeesMergeHiddenCommits pins the defeat of
// git's default history simplification: a side-branch commit whose
// prefix effect duplicates the surviving merge parent's tree would be
// hidden from a plain prefixed `git log A..B` (the same silent-skip
// signature the selection fix exists to prevent). With --full-history
// the commit must appear in the run — here as pruned-converged, since
// its content landed through the mainline duplicate.
func TestFullHistorySelectionSeesMergeHiddenCommits(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// The identical file is added on both a side branch and mainline.
	src.git("checkout", "-b", "side")
	src.write("proj/dup.txt", "X")
	src.commit("sibling adds duplicate")
	src.git("checkout", testBranch)
	src.write("proj/dup.txt", "X")
	src.commit("mainline adds duplicate")
	src.git("merge", "--no-ff", "-m", "merge side", "side")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "sibling adds duplicate") {
		t.Fatalf("history simplification hid the sibling commit:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
}

// TestProseQuotedIdDoesNotDropCommit pins the own-line rule for the
// copy filter: a source commit whose message merely quotes a shipit id
// mid-prose must still be mirrored, changes and all.
func TestProseQuotedIdDoesNotDropCommit(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	src.write("proj/prose.txt", "content\n")
	src.commit("reverts the change recorded as shipit-source-id: 05220186 mid-sentence")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "applying") {
		t.Fatalf("prose-quoted id caused the commit to be dropped:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
}

// writeSymlink creates a symbolic link in the working clone.
func (r *gritRepo) writeSymlink(path, target string) {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0777); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		r.t.Fatal(err)
	}
}

// TestSymlinkConvergenceAndDeletion covers BlobHash's link-text
// hashing end to end: an identically re-added symlink is pruned as
// converged (hashing through the target would never match), and a
// broken symlink's deletion applies instead of being falsely pruned.
func TestSymlinkConvergenceAndDeletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks on windows requires privileges")
	}
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "target content\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// The destination adds a symlink directly; an identical source-side
	// addition must prune as converged.
	dst.writeSymlink("lnk", "base.txt")
	dst.commit("destination adds symlink")
	dst.push()
	src.writeSymlink("proj/lnk", "base.txt")
	src.commit("source adds identical symlink")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "skipping converged lnk") {
		t.Fatalf("symlink convergence pruning did not fire:\n%s", out)
	}
	dst.pull()

	// A broken symlink arrives at both sides, then the source deletes
	// it: the deletion must apply rather than be pruned as already
	// converged (the broken link exists at the destination).
	src.writeSymlink("proj/broken.lnk", "nowhere")
	src.commit("add broken symlink")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	runGit(t, src.dir, "rm", "proj/broken.lnk")
	src.commit("delete broken symlink")
	src.push()
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "applying") {
		t.Fatalf("deletion of broken symlink was not applied:\n%s", out)
	}
	dst.pull()
	if _, err := os.Lstat(filepath.Join(dst.dir, "broken.lnk")); !os.IsNotExist(err) {
		t.Fatalf("broken symlink still present at destination: %v", err)
	}
}

// TestModeFlipIsNotConverged pins that convergence pruning compares
// modes as well as content: identical bytes with a pending exec-bit flip
// must apply, leaving the destination executable — not silently pruned
// into permanent divergence.
func TestModeFlipIsNotConverged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bits are not representable when core.filemode=false")
	}
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// The destination directly creates the file non-executable; the
	// source commit adds identical bytes AND the executable bit.
	dst.write("tool.txt", "v2\n")
	dst.commit("destination adds tool non-executable")
	dst.push()
	src.write("proj/tool.txt", "v2\n")
	if err := os.Chmod(filepath.Join(src.dir, "proj", "tool.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	src.commit("source adds tool with exec bit")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if strings.Contains(out, "skipping converged") {
		t.Fatalf("mode flip was falsely treated as converged:\n%s", out)
	}
	if !strings.Contains(out, "conflict") {
		t.Fatalf("expected the mode difference to surface as a loud conflict:\n%s", out)
	}

	// Resolve by adopting the executable bit, then continue.
	clone := gritCloneDir(t, dst.bare, "", testBranch)
	if err := os.Chmod(filepath.Join(clone, "tool.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "tool.txt")
	runGit(t, clone, "-c", "core.editor=true", "am", "--continue")

	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "pushing previously resolved session") {
		t.Fatalf("resolved mode conflict was not pushed:\n%s", out)
	}
	dst.pull()

	fi, err := os.Stat(filepath.Join(dst.dir, "tool.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0100 == 0 {
		t.Fatalf("executable bit did not land at destination: %v", fi.Mode())
	}
}

// TestRewrittenPathNeverPruned pins that convergence pruning is
// disabled for rewrite-rule paths: the recorded post-image is pre-rewrite,
// so blob equality would silently freeze un-rewritten content into the
// destination. With the rule active, raw upstream content must be
// rewritten on application.
func TestRewrittenPathNeverPruned(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch
	rule := `rewrite:f\.txt$:/RAWVALUE/DONEVALUE/`

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec, rule)
	dst.pull()

	// Raw upstream form arrives at the destination through a direct
	// commit — exactly what a rewrite rule exists to translate.
	dst.write("f.txt", "RAWVALUE\n")
	dst.commit("destination receives raw form")
	dst.push()

	src.write("proj/f.txt", "RAWVALUE\n")
	src.commit("source change requiring rewrite")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec, rule)
	if strings.Contains(out, "skipping converged") {
		t.Fatalf("rewritable path was falsely pruned as converged:\n%s", out)
	}
	// Raw-at-destination versus rewritten-in-source is a genuine content
	// difference: it must surface loudly, never be frozen in by pruning.
	if !strings.Contains(out, "conflict") {
		t.Fatalf("expected the raw/rewritten divergence to surface as a conflict:\n%s", out)
	}

	// Resolve by adopting the rewritten form, then publish.
	clone := gritCloneDir(t, dst.bare, "", testBranch)
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("DONEVALUE\n"), 0666); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "f.txt")
	runGit(t, clone, "-c", "core.editor=true", "am", "--continue")

	out = gritOutput(t, src.gritBin, srcSpec, dstSpec, rule)
	if !strings.Contains(out, "pushing previously resolved session") {
		t.Fatalf("resolved rewrite conflict was not pushed:\n%s", out)
	}
	dst.pull()

	got := strings.TrimSpace(dstRead(t, dst.dir, "f.txt"))
	if got != "DONEVALUE" {
		t.Fatalf("rewrite did not land at destination: %q", got)
	}
}

// TestUntrackedLeftoverDoesNotSilenceAddition pins that convergence
// pruning consults HEAD's committed tree, never the worktree: an
// untracked leftover identical to an incoming addition cannot silence
// it. The collision fails loudly at application time; after cleaning
// the leftover, the same run converges.
func TestUntrackedLeftoverDoesNotSilenceAddition(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Untracked leftover inside grit's own destination clone — exactly
	// where grit's pause guidance tells users to edit.
	payload := "payload\n"
	clone := gritCloneDir(t, dst.bare, "", testBranch)
	if err := os.MkdirAll(filepath.Join(clone, "data"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "data", "blob.txt"), []byte(payload), 0666); err != nil {
		t.Fatal(err)
	}

	// The source adds the identical file through a real commit.
	src.write("proj/data/blob.txt", payload)
	src.commit("add data blob")
	src.push()

	// The collision must fail loudly at application time: pruning may
	// not consult the untracked leftover.
	gritRun := func() (string, error) {
		cmd := exec.Command(src.gritBin,
			"-config=user.name=test,user.email="+testAuthorEnv,
			"-push", srcSpec, dstSpec)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, runErr := gritRun()
	if strings.Contains(out, "skipping converged") {
		t.Fatalf("untracked leftover silenced an incoming addition:\n%s", out)
	}
	if runErr == nil {
		t.Fatalf("untracked collision should fail loudly at apply time:\n%s", out)
	}

	// Discard the failed application's session and the leftover, then
	// converge: the addition must land.
	runGit(t, clone, "am", "--abort")
	runGit(t, clone, "clean", "-fd")
	out, runErr = gritRun()
	if runErr != nil {
		t.Fatalf("post-cleanup run failed:\n%s", out)
	}
	if !strings.Contains(out, "applying") {
		t.Fatalf("addition did not apply after cleanup:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
}

// TestAllTagsIneligibleIsFixedPoint pins that a destination whose
// every tagged commit becomes anchor-ineligible under newly tightened
// strip rules falls through to initial sync, where tag-set exclusion
// drops the already-mirrored commits — a clean fixed point with the
// exclusion actively engaged, not an error.
func TestAllTagsIneligibleIsFixedPoint(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	// Mirror a commit normally: it lands tagged at the destination.
	src.write("proj/f.txt", "one\n")
	src.commit("mirrored change")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Tighten the strip rules so that the recorded tag becomes
	// anchor-ineligible, then synchronize again.
	out := gritOutput(t, src.gritBin, srcSpec, dstSpec, "strip:^f\\.txt$")
	if !strings.Contains(out, "is not applicable") {
		t.Fatalf("ineligible anchor was not detected and stepped past:\n%s", out)
	}
	if !strings.Contains(out, "skipping already synchronized") {
		t.Fatalf("tag-set exclusion did not drop the already-mirrored commit:\n%s", out)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("all-ineligible state did not reach a fixed point:\n%s", out)
	}
}

// TestPartiallyPrunedCommitReexamined pins the lifecycle of a commit
// whose diffs partially converge: it lands without a shipit tag, its
// kept change applies, an unchanged rerun is a quiet fixed point, and —
// critically — if the destination later loses the applied half, the
// re-examination restores it instead of freezing forever.
func TestPartiallyPrunedCommitReexamined(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/a.txt", "A1\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// The source commit carries one duplicate half and one real half.
	src.write("proj/a.txt", "A2\n")
	src.write("proj/c.txt", "C\n")
	src.commit("source adds duplicate and real change")
	src.push()
	// Order matters for the fixture: the destination must already hold
	// the duplicate when the source commit is mirrored.
	dst.write("c.txt", "C\n")
	dst.commit("destination receives duplicate directly")
	dst.push()
	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "1 of 2 diffs already converged") {
		t.Fatalf("partial convergence was not detected:\n%s", out)
	}
	if strings.Contains(strings.TrimSpace(dst.gitOut("log", "-1", "--format=%B")), "fbshipit-source-id:") {
		t.Fatalf("partially-pruned commit was tagged, freezing it against re-examination")
	}
	dst.pull()

	// Quiet fixed point while nothing diverges.
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("partial commit did not reach a fixed point:\n%s", out)
	}

	// The destination loses the applied half via a pure revert;
	// re-examination must then restore it without noise. (A divergent
	// rewrite would surface as a resolvable conflict instead — same
	// lifecycle as TestSessionPauseResume.)
	dst.write("a.txt", "A1\n")
	dst.commit("destination reverts the applied half")
	dst.push()
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if strings.Contains(out, "nothing to do") {
		t.Fatalf("reverted half was not re-examined:\n%s", out)
	}
	dst.pull()
	if got := dstRead(t, dst.dir, "a.txt"); got != "A2\n" {
		t.Fatalf("applied half was not restored: %q", got)
	}
}

// TestStripMessagePartialStaysUntagged pins that strip-message rules
// cannot re-tag a partially-pruned commit: the surviving half applies,
// the commit lands untagged (re-examinable), and the stripped-subject
// override is reserved for fully-tagged commits.
func TestStripMessagePartialStaysUntagged(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// gen/a.txt arrives at the destination directly; the source commit
	// modifies it and adds gen/b.txt. Every path matches the
	// strip-message rule, and half the diffs converge.
	dst.write("a.txt", "A2\n")
	dst.commit("destination receives a.txt directly")
	dst.push()
	src.write("proj/a.txt", "A2\n")
	src.write("proj/b.txt", "B\n")
	src.commit("gen update")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec,
		"strip-message:^gen/")
	if !strings.Contains(out, "skipping converged") {
		t.Fatalf("converged half was not pruned:\n%s", out)
	}
	if !strings.Contains(out, "remain re-examinable") {
		t.Fatalf("partial commit was not kept re-examinable:\n%s", out)
	}
	dst.pull()

	tip := strings.TrimSpace(dst.gitOut("log", "-1", "--format=%B"))
	if strings.Contains(tip, "fbshipit-source-id:") || strings.Contains(tip, "Commit message stripped.") {
		t.Fatalf("partially-pruned commit landed tagged or stripped: %q", tip)
	}
	if got := dstRead(t, dst.dir, "b.txt"); got != "B\n" {
		t.Fatalf("surviving half did not land: %q", got)
	}
}
