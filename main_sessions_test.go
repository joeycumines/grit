package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionPauseResume verifies the persistent conflict lifecycle: pause,
// preservation across invocations, manual resolution, and a later push.
func TestSessionPauseResume(t *testing.T) {
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

// TestUnbornSourceFailsLoudly documents Open failing loudly for a commitless
// source ("couldn't find remote ref"), identical to pristine upstream d3b81e6.
func TestUnbornSourceFailsLoudly(t *testing.T) {
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
	// Assert the failure loudly names the source repository without
	// coupling to git's exact stderr wording across versions.
	if !strings.Contains(string(out), "src.git") {
		t.Fatalf("expected a loud failure referencing the source:\n%s", out)
	}
}

// TestPauseWithoutPushThenPushLater verifies a conflict paused by a plain run
// persists for resolution and is published by a later push-enabled run.
func TestPauseWithoutPushThenPushLater(t *testing.T) {
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

// TestForeignUnpushedStateAborts verifies unpushed destination commits grit did
// not author abort the run loudly instead of being preserved and published.
func TestForeignUnpushedStateAborts(t *testing.T) {
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
	if !strings.Contains(out, "discarding stale clone cache") {
		t.Fatalf("foreign unpushed state did not trigger cache recovery:\n%s", out)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("recovered run did not reach a fixed point:\n%s", out)
	}
	tracked := dst.gitOut("ls-files")
	if strings.Contains(tracked, "stray") {
		t.Fatalf("foreign content leaked into the destination:\n%s", tracked)
	}
}

// TestSourceCloneStrayStateIsDiscarded verifies the read-through-cache contract:
// stray local commits never abort or leak; the next run mirrors the remote tip.
func TestSourceCloneStrayStateIsDiscarded(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Stray, unpushed, non-grit state inside grit's own source clone.
	sclone := gritCloneDir(t, src.bare, "proj/", testBranch)
	if err := os.WriteFile(filepath.Join(sclone, "stray.txt"), []byte("stray\n"), 0666); err != nil {
		t.Fatal(err)
	}
	runGit(t, sclone, "add", "stray.txt")
	runGit(t, sclone, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "-m", "stray source-clone commit")

	// A real remote change follows.
	src.write("proj/new.txt", "new\n")
	src.commit("real source change")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if strings.Contains(out, "without a grit shipit id") {
		t.Fatalf("source clone stray state aborted the run:\n%s", out)
	}
	if !strings.Contains(out, "applying") {
		t.Fatalf("run did not apply the real source change:\n%s", out)
	}
	dst.pull()
	if got := dstRead(t, dst.dir, "new.txt"); got != "new\n" {
		t.Fatalf("remote change did not land: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dst.dir, "stray.txt")); err == nil {
		t.Fatal("stray source-clone file leaked into the destination")
	}
}

// TestDivergedGritTailIsDiscardedNotPreserved pins the ancestry gate: a diverged
// all-grit tail can never push, so Open discards it instead of deferring loss.
func TestDivergedGritTailIsDiscardedNotPreserved(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1\n")
	src.commit("first source change")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Pending source work for the recovery run.
	src.write("proj/next.txt", "v2\n")
	src.commit("second source change")
	src.push()

	// The destination remote advances independently, so any unpushed
	// clone tail on the old base stops descending from the remote tip.
	dst.write("only.txt", "diverged\n")
	dst.commit("destination diverges")
	dst.push()

	// An entirely grit-authored unpushed tail sits on the stale base.
	clone := gritCloneDir(t, dst.bare, "", testBranch)
	runGit(t, clone, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "--allow-empty", "-m",
		"resolved session\n\nfbshipit-source-id: 0123456789abcdef0123456789abcdef01234567\n")

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "discarding stale clone cache") {
		t.Fatalf("diverged grit-authored tail was preserved instead of discarded:\n%s", out)
	}
	dst.pull()
	if got := dstRead(t, dst.dir, "next.txt"); got != "v2\n" {
		t.Fatalf("pending source change did not land after recovery: %q", got)
	}
	if got := dstRead(t, dst.dir, "only.txt"); got != "diverged\n" {
		t.Fatalf("remote divergence clobbered during recovery: %q", got)
	}
}

// TestIndentedQuotationIsNotGritAuthored verifies the flush-left requirement
// rejects indented id quotations that Log's dedentation would resurrect.
func TestIndentedQuotationIsNotGritAuthored(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	clone := gritCloneDir(t, dst.bare, "", testBranch)
	foreign := "quoted prose\n\n    fbshipit-source-id: 0123456789abcdef0123456789abcdef01234567\n"
	if err := os.WriteFile(filepath.Join(clone, "stray.txt"), []byte(foreign), 0666); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "stray.txt")
	runGit(t, clone, "-c", "user.email=t@e", "-c", "user.name=t",
		"commit", "-m", foreign)

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "discarding stale clone cache") {
		t.Fatalf("indented quotation passed the authorship gate:\n%s", out)
	}
}
