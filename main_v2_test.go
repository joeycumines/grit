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
// derives for the provided repository URL, so that tests can inspect (and
// resolve) paused sessions. Must be called after TEST_TMPDIR has been set.
func gritCloneDir(t *testing.T, url string) string {
	t.Helper()
	base := filepath.Base(url)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(os.Getenv("TEST_TMPDIR"), "grit",
		fmt.Sprintf("%s%02x%02x%02x%02x", base, sum[0], sum[1], sum[2], sum[3]))
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

	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	// The applied commit must carry the real change, not be empty.
	tip := strings.TrimSpace(dst.gitOut("log", "-1", "--format=%B"))
	if !strings.Contains(tip, "fbshipit-source-id: ") {
		t.Fatalf("destination tip is not a grit commit: %q", tip)
	}

	// Fixed point: a rerun has nothing to do and pushes nothing.
	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("rerun did not reach a fixed point:\n%s", out)
	}
}

// TestV2PartialDriftThreeWay verifies that a patch whose context has
// drifted at the destination merges three-way instead of failing: the
// destination edits line 2 directly, the unsynced source commit edits
// distant lines 8 and 9, and all changes must coexist afterwards.
// (Adjacent-region edits remain genuine conflicts by git's merge rules;
// that lifecycle is covered by TestV2SessionPauseResume.)
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

	// The destination drifts line 2.
	lines[1] = "dst2"
	dst.write("f.txt", strings.Join(lines, "\n")+"\n")
	dst.commit("destination drifts line 2")
	dst.push()

	// The source edits distant lines 8 and 9.
	lines[7] = "src8"
	lines[8] = "src9"
	lines[1] = "line2"
	src.write("proj/f.txt", strings.Join(lines, "\n")+"\n")
	src.commit("source edits distant lines")
	src.push()

	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	got := strings.Split(strings.TrimSpace(dstRead(t, dst.dir, "f.txt")), "\n")
	if got[1] != "dst2" || got[7] != "src8" || got[8] != "src9" {
		t.Fatalf("three-way merge lost changes: lines 2,8,9 = %q %q %q", got[1], got[7], got[8])
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
	clone := gritCloneDir(t, dst.bare)
	if _, err := os.Stat(filepath.Join(clone, ".git", "rebase-apply")); err != nil {
		t.Fatalf("session not paused in clone: %v", err)
	}

	// A fresh invocation must preserve the paused session.
	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if strings.Contains(out, "nothing to do") || !fileContains(t, filepath.Join(clone, "f.txt"), "ours7") {
		t.Fatalf("paused session was not preserved across invocations:\n%s", out)
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

// TestV2LegacyTagSuppressesReplay verifies that a destination commit
// carrying a legacy abbreviated (7-hex) shipit id anchors the resume and
// suppresses replay of its source commit, matching the anchor walk's
// long-standing semantics. Prefix matching for legacy ids that exclude
// non-ancestor commits is covered by TestIsProcessed.
func TestV2LegacyTagSuppressesReplay(t *testing.T) {
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
