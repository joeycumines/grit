// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var regexpFullDigest = regexp.MustCompile(`fbshipit-source-id: [0-9a-f]{40}`)

// gritCloneDir computes the local clone directory that grit's git.Open
// derives for the provided endpoint (url, prefix, branch), so that tests
// can inspect (and resolve) paused sessions. Must be called after
// TEST_TMPDIR has been set. This mirrors the derivation in git/repo.go's
// Open; if Open ever changes its formula, this helper must change with it.
func gritCloneDir(t *testing.T, url, prefix, branch string) string {
	t.Helper()
	base := filepath.Base(url)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	h := sha256.New()
	h.Write([]byte(url))
	h.Write([]byte{0})
	h.Write([]byte(prefix))
	h.Write([]byte{0})
	h.Write([]byte(branch))
	sum := h.Sum(nil)
	return filepath.Join(os.Getenv("TEST_TMPDIR"), "grit",
		fmt.Sprintf("%s%x", base, sum[:16]))
}

// TestV2DuplicateAddConvergence replicates the eventloop fuzz-testdata
// collision: content arrives at the destination through a direct commit,
// and a later source commit adds identical bytes. The duplicate diff must
// be pruned (no conflict, no empty commit) while the rest of the commit
// applies, and a rerun must reach a stable nothing-to-do fixed point.
func TestV2DuplicateAddConvergence(t *testing.T) {
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

	// The applied commit must carry the real change, not be empty.
	tip := strings.TrimSpace(dst.gitOut("log", "-1", "--format=%B"))
	if !strings.Contains(tip, "fbshipit-source-id: ") {
		t.Fatalf("destination tip is not a grit commit: %q", tip)
	}

	// Fixed point: a rerun has nothing to do and pushes nothing.
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("rerun did not reach a fixed point:\n%s", out)
	}
}

// TestV2PartialDriftThreeWay verifies that a patch whose textual
// context has drifted at the destination is rescued by three-way merge:
// the destination edits line 5 — inside the hunk context of the source's
// edit to lines 8-9 — so plain git am fails (verified against raw git),
// while am --3way reconstructs the base tree from the patch's index
// lines and merges both changes.
func TestV2PartialDriftThreeWay(t *testing.T) {
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

// TestV2SessionPauseResume verifies the persistent conflict lifecycle: a
// genuine conflict pauses the session; a fresh grit invocation preserves
// it; manual resolution plus am --continue persists; and the next grit
// invocation pushes the resolved session.
func TestV2SessionPauseResume(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "line7\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Both sides change the same line from the same base: a genuine
	// conflict.
	dst.write("f.txt", "ours7\n")
	dst.commit("destination change")
	dst.push()
	src.write("proj/f.txt", "theirs7\n")
	src.commit("source change")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "conflict") {
		t.Fatalf("expected a conflict, got:\n%s", out)
	}
	clone := gritCloneDir(t, dst.bare, "", testBranch)
	if _, err := os.Stat(filepath.Join(clone, ".git", "rebase-apply")); err != nil {
		t.Fatalf("session not paused in clone: %v", err)
	}

	// A fresh invocation must preserve the paused session and refuse to
	// make pruning decisions against the conflicted worktree.
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "resuming interrupted git am session") {
		t.Fatalf("paused session was not resumed:\n%s", out)
	}
	if !strings.Contains(out, "resolution is still pending") {
		t.Fatalf("pending session did not short-circuit selection:\n%s", out)
	}
	if !fileContains(t, filepath.Join(clone, "f.txt"), "ours7") {
		t.Fatal("paused worktree was disturbed")
	}

	// Resolve manually and continue the session.
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("resolved7\n"), 0666); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "f.txt")
	runGit(t, clone, "-c", "core.editor=true", "am", "--continue")

	// The next invocation pushes the resolved session.
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "pushing previously resolved session") {
		t.Fatalf("resolved session was not pushed:\n%s", out)
	}
	dst.pull()
	if got := dstRead(t, dst.dir, "f.txt"); got != "resolved7\n" {
		t.Fatalf("resolved content did not land at destination: %q", got)
	}
}

func gritOutput(t *testing.T, bin, srcSpec, dstSpec string) string {
	t.Helper()
	cmd := exec.Command(bin,
		"-config=user.name=test,user.email="+testAuthorEnv,
		"-push", srcSpec, dstSpec)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("grit exited non-zero (expected when pausing):\n%s", out)
	}
	return string(out)
}

// gritDumpOutput runs grit in -dump mode (no application, no push) and
// returns its combined output.
func gritDumpOutput(t *testing.T, bin, srcSpec, dstSpec string) string {
	t.Helper()
	cmd := exec.Command(bin,
		"-config=user.name=test,user.email="+testAuthorEnv,
		"-dump", srcSpec, dstSpec)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("grit -dump failed: %v\n%s", err, out)
	}
	return string(out)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func dstRead(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fileContains(t *testing.T, path, substr string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), substr)
}

// TestV2TagSetExclusionExactlyOnce verifies that shipit-tag state makes
// processing exactly-once across runs: after two sibling commits are
// applied, a later incremental run re-selects the first sibling (it is
// not an ancestor of the new anchor, which sits atop the second
// sibling) but must exclude it by its recorded tag rather than replay
// it. Also asserts that new tags record the full source digest.
func TestV2TagSetExclusionExactlyOnce(t *testing.T) {
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
	if strings.Contains(out, "skipping converged") {
		t.Fatalf("prune fired where tag exclusion should have:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("no fixed point after exclusion:\n%s", out)
	}
}

// TestV2LegacyAbbreviatedAnchorSuppressesReplay verifies that a
// destination commit carrying a legacy abbreviated (7-hex) shipit id
// resolves as the resume anchor, bounding the selection range so its
// source commit is not replayed. Prefix-matching of abbreviated ids
// during tag-set exclusion is covered by TestIsProcessed. Prefix matching for legacy ids that exclude
// non-ancestor commits is covered by TestIsProcessed.
func TestV2LegacyAbbreviatedAnchorSuppressesReplay(t *testing.T) {
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

// TestV2FullyConvergedCommitSkipped verifies the whole-commit skip path:
// a source commit whose every diff already matches the destination
// creates no destination commit, is reported in the skip accounting, and
// leaves the repository at a stable nothing-to-do fixed point.
func TestV2FullyConvergedCommitSkipped(t *testing.T) {
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

// TestV2UnbornSourceFailsLoudly documents the pre-existing contract that
// grit cannot synchronize from a repository without any commits: Open
// fails loudly at fetch ("couldn't find remote ref"), identical to
// pristine upstream d3b81e6 behavior. Object seeding therefore only ever
// runs for sources whose branch exists.
func TestV2UnbornSourceFailsLoudly(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	cmd := exec.Command(src.gritBin,
		"-config=user.name=test,user.email="+testAuthorEnv,
		"-push", srcSpec, dstSpec)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("unborn source unexpectedly succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), "couldn't find remote ref") {
		t.Fatalf("expected a loud missing-branch failure:\n%s", out)
	}
}

// TestV2PauseWithoutPushThenPushLater verifies that a conflict paused by
// a plain (non -push) run persists for resolution, and that a later
// push-enabled run publishes the resolved session — the other half of
// the preservation heuristic.
func TestV2PauseWithoutPushThenPushLater(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "line7\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Conflict without -push: application still happens, so the session
	// pauses exactly as it would with -push.
	dst.write("f.txt", "ours7\n")
	dst.commit("destination change")
	dst.push()
	src.write("proj/f.txt", "theirs7\n")
	src.commit("source change")
	src.push()

	cmd := exec.Command(src.gritBin,
		"-config=user.name=test,user.email="+testAuthorEnv,
		srcSpec, dstSpec)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected a conflicting non-push run to fail:\n%s", out)
	}
	clone := gritCloneDir(t, dst.bare, "", testBranch)
	if _, err := os.Stat(filepath.Join(clone, ".git", "rebase-apply")); err != nil {
		t.Fatalf("session not paused: %v", err)
	}

	// Resolve and continue; the state must remain unpushed.
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("resolved7\n"), 0666); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "f.txt")
	runGit(t, clone, "-c", "core.editor=true", "am", "--continue")

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "pushing previously resolved session") {
		t.Fatalf("later push-enabled run did not publish the resolved session:\n%s", out)
	}
	dst.pull()
	if got := dstRead(t, dst.dir, "f.txt"); got != "resolved7\n" {
		t.Fatalf("resolved content did not land at destination: %q", got)
	}
}

// TestV2FullHistorySelectionSeesMergeHiddenCommits pins the defeat of
// git's default history simplification: a side-branch commit whose
// prefix effect duplicates the surviving merge parent's tree would be
// hidden from a plain prefixed `git log A..B` (the same silent-skip
// signature the selection fix exists to prevent). With --full-history
// the commit must appear in the run — here as pruned-converged, since
// its content landed through the mainline duplicate.
func TestV2FullHistorySelectionSeesMergeHiddenCommits(t *testing.T) {
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

// TestV2ForeignUnpushedStateAborts verifies that unpushed commits in the
// destination clone that grit did not author (no shipit id at HEAD) abort
// the run loudly instead of being preserved and later published.
func TestV2ForeignUnpushedStateAborts(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Stray, non-grit work left in grit's own clone, ahead of origin.
	clone := gritCloneDir(t, dst.bare, "", testBranch)
	runGit(t, clone, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "--allow-empty", "-m", "stray local commit")

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "without a grit shipit id") {
		t.Fatalf("foreign unpushed state did not abort loudly:\n%s", out)
	}
}

// TestV2ProseQuotedIdDoesNotDropCommit pins the own-line rule for the
// copy filter: a source commit whose message merely quotes a shipit id
// mid-prose must still be mirrored, changes and all.
func TestV2ProseQuotedIdDoesNotDropCommit(t *testing.T) {
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

// TestV2SymlinkConvergenceAndDeletion covers BlobHash's link-text
// hashing end to end: an identically re-added symlink is pruned as
// converged (hashing through the target would never match), and a
// broken symlink's deletion applies instead of being falsely pruned.
func TestV2SymlinkConvergenceAndDeletion(t *testing.T) {
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
