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

func TestResolveEmptyAMAndResume(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "base\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	dst.write("f.txt", "destination version\n")
	dst.commit("destination change")
	dst.push()
	destinationHead := strings.TrimSpace(dst.gitOut("rev-parse", "HEAD"))

	src.write("proj/f.txt", "source version\n")
	src.commit("conflicting source commit")
	conflictID := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.git("commit", "--allow-empty", "-m", "genuine empty source commit")
	emptyID := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.push()
	src.write("proj/next.txt", "next commit\n")
	src.commit("later source commit")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "conflict") {
		t.Fatalf("expected a conflict, got:\n%s", out)
	}
	if !strings.Contains(out, "that invocation resumes processing; -push pushes on success") {
		t.Fatalf("paused-session guidance does not explain the one-run resume:\n%s", out)
	}
	clone := gritCloneDir(t, dst.bare, "", testBranch)
	if _, err := os.Stat(filepath.Join(clone, ".git", "rebase-apply")); err != nil {
		t.Fatalf("session not paused in clone: %v", err)
	}

	resolveEmpty := func() (string, error) {
		return runGrit(src.gritBin, []string{
			"-config=user.name=test,user.email=" + testAuthorEnv,
			"-push", "-resolve-empty", srcSpec, dstSpec,
		})
	}
	headBefore := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD"))
	out, err := resolveEmpty()
	if err == nil || !strings.Contains(out, "unmerged paths remain") {
		t.Fatalf("unresolved conflict was accepted as empty (err=%v):\n%s", err, out)
	}
	if got := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD")); got != headBefore {
		t.Fatalf("rejected empty resolution moved HEAD: %s -> %s", headBefore, got)
	}

	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("manual resolution\n"), 0666); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "f.txt")
	out, err = resolveEmpty()
	if err == nil || !strings.Contains(out, "staged tree differs from HEAD") {
		t.Fatalf("non-empty staged resolution was accepted as empty (err=%v):\n%s", err, out)
	}
	if got := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD")); got != headBefore {
		t.Fatalf("rejected non-empty resolution moved HEAD: %s -> %s", headBefore, got)
	}

	runGit(t, clone, "checkout", "HEAD", "--", "f.txt")
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("unstaged resolution\n"), 0666); err != nil {
		t.Fatal(err)
	}
	out, err = resolveEmpty()
	if err == nil || !strings.Contains(out, "tracked worktree changes are not staged") {
		t.Fatalf("unstaged resolution was accepted as empty (err=%v):\n%s", err, out)
	}
	runGit(t, clone, "checkout", "HEAD", "--", "f.txt")

	amContinue := exec.Command("git", "-C", clone, "-c", "core.editor=true", "am", "--continue")
	if out, err := amContinue.CombinedOutput(); err == nil || !strings.Contains(string(out), "No changes") {
		t.Fatalf("expected git am --continue to reject the intentional empty resolution (err=%v):\n%s", err, out)
	}

	out, err = resolveEmpty()
	if err != nil {
		t.Fatalf("resolve paused patch as empty and continue: %v\n%s", err, out)
	}
	if !strings.Contains(out, "empty synchronization commit is in place") {
		t.Fatalf("empty resolution was not reported:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "rebase-apply")); !os.IsNotExist(err) {
		t.Fatalf("git am session remains after no-op resolution: %v", err)
	}

	dst.pull()
	if got := dstRead(t, dst.dir, "f.txt"); got != "destination version\n" {
		t.Fatalf("no-op resolution changed destination content: %q", got)
	}
	if got := dstRead(t, dst.dir, "next.txt"); got != "next commit\n" {
		t.Fatalf("sync did not advance to the later source commit: %q", got)
	}
	anchor := strings.TrimSpace(dst.gitOut("log", "-1", "--format=%H", "--grep=fbshipit-source-id: "+conflictID))
	if anchor == "" {
		t.Fatal("destination history has no synchronization anchor for the no-op source commit")
	}
	if got := strings.TrimSpace(dst.gitOut("diff-tree", "--no-commit-id", "--name-only", "-r", anchor)); got != "" {
		t.Fatalf("no-op synchronization anchor changes files: %s", got)
	}
	if got := strings.TrimSpace(dst.gitOut("rev-parse", anchor+"^")); got != destinationHead {
		t.Fatalf("empty anchor parent = %s, want %s", got, destinationHead)
	}
	wantAuthor := strings.TrimSpace(src.gitOut("show", "-s", "--format=%an%x00%ae%x00%aI", conflictID))
	gotAuthor := strings.TrimSpace(dst.gitOut("show", "-s", "--format=%an%x00%ae%x00%aI", anchor))
	if gotAuthor != wantAuthor {
		t.Fatalf("empty anchor author metadata = %q, want %q", gotAuthor, wantAuthor)
	}
	if got := strings.TrimSpace(dst.gitOut("log", "-1", "--format=%s")); got != "later source commit" {
		t.Fatalf("HEAD did not advance past the no-op patch: %q", got)
	}
	if got := strings.TrimSpace(dst.gitOut("log", "--format=%H", "--grep=fbshipit-source-id: "+emptyID)); got != "" {
		t.Fatalf("genuine empty source commit unexpectedly got its own destination anchor: %s", got)
	}

	out = gritOutputStrict(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("sync did not reach a fixed point after no-op resolution:\n%s", out)
	}
}

func TestResolveEmptyAMPushesOnlyEmptyAnchor(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "base\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	dst.write("f.txt", "destination version\n")
	dst.commit("destination change")
	dst.push()
	destinationHead := strings.TrimSpace(dst.gitOut("rev-parse", "HEAD"))

	src.write("proj/f.txt", "source version\n")
	src.commit("conflicting source commit")
	conflictID := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.git("commit", "--allow-empty", "-m", "genuine empty source commit")
	emptyID := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "conflict") {
		t.Fatalf("expected a conflict, got:\n%s", out)
	}
	clone := gritCloneDir(t, dst.bare, "", testBranch)
	runGit(t, clone, "checkout", "HEAD", "--", "f.txt")

	if err := os.Remove(filepath.Join(clone, ".git", "rebase-apply", "grit-context")); err != nil {
		t.Fatalf("simulate a pre-context paused session: %v", err)
	}
	finalCommit, err := os.ReadFile(filepath.Join(clone, ".git", "rebase-apply", "final-commit"))
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(runGitOut(t, clone, "write-tree"))
	markerCmd := exec.Command("git", "-C", clone, "commit-tree", tree, "-p", destinationHead)
	markerCmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL="+testAuthorEnv,
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL="+testAuthorEnv,
	)
	markerCmd.Stdin = strings.NewReader(string(finalCommit))
	markerBytes, err := markerCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("simulate interrupted marker creation: %v\n%s", err, markerBytes)
	}
	markerHead := strings.TrimSpace(string(markerBytes))
	runGit(t, clone, "update-ref", "HEAD", markerHead, destinationHead)

	cmd := exec.Command(src.gritBin,
		"-config=user.name=test,user.email="+testAuthorEnv,
		"-resolve-empty", srcSpec, dstSpec)
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve empty patch without pushing: %v\n%s", err, outBytes)
	}
	if !strings.Contains(string(outBytes), "source ancestry was verified, but source prefix and rules cannot be checked") {
		t.Fatalf("legacy paused-session context limitation was not reported:\n%s", outBytes)
	}
	if strings.Contains(string(outBytes), "pushing ") {
		t.Fatalf("resolve without -push unexpectedly pushed:\n%s", outBytes)
	}
	if got := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD")); got != markerHead {
		t.Fatalf("idempotent resolution changed the existing marker: %s -> %s", markerHead, got)
	}
	if got := strings.TrimSpace(runGitOut(t, clone, "log", "--format=%H", "--grep=fbshipit-source-id: "+conflictID)); got != markerHead {
		t.Fatalf("idempotent resolution created duplicate markers: got %s, want %s", got, markerHead)
	}
	if got := strings.TrimSpace(dst.gitOut("rev-parse", "HEAD")); got != destinationHead {
		t.Fatalf("resolve without -push moved remote HEAD: %s -> %s", destinationHead, got)
	}

	out = gritOutputStrict(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "pushing previously resolved session") {
		t.Fatalf("later -push did not preserve and publish the empty anchor:\n%s", out)
	}

	dst.pull()
	anchor := strings.TrimSpace(dst.gitOut("rev-parse", "HEAD"))
	if got := strings.TrimSpace(dst.gitOut("diff-tree", "--no-commit-id", "--name-only", "-r", anchor)); got != "" {
		t.Fatalf("empty-only synchronization anchor changes files: %s", got)
	}
	if got := strings.TrimSpace(dst.gitOut("rev-parse", anchor+"^")); got != destinationHead {
		t.Fatalf("empty anchor parent = %s, want %s", got, destinationHead)
	}
	if !strings.Contains(dst.gitOut("show", "-s", "--format=%B", anchor), "fbshipit-source-id: "+conflictID) {
		t.Fatalf("empty anchor does not identify the source commit %s", conflictID)
	}
	if got := strings.TrimSpace(dst.gitOut("log", "--format=%H", "--grep=fbshipit-source-id: "+emptyID)); got != "" {
		t.Fatalf("genuine empty source commit unexpectedly got its own destination anchor: %s", got)
	}

	out = gritOutputStrict(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("empty-only sync did not reach a fixed point:\n%s", out)
	}
}

func TestResolveEmptyAMRejectsDifferentSourceScope(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "base\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	dst.write("f.txt", "destination version\n")
	dst.commit("destination change")
	dst.push()
	src.write("proj/f.txt", "source version\n")
	src.commit("conflicting source commit")
	src.push()
	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "conflict") {
		t.Fatalf("expected a conflict, got:\n%s", out)
	}

	clone := gritCloneDir(t, dst.bare, "", testBranch)
	runGit(t, clone, "checkout", "HEAD", "--", "f.txt")
	headBefore := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD"))
	otherSpec := src.bare + ",," + testBranch
	out, err := runGrit(src.gritBin, []string{
		"-config=user.name=test,user.email=" + testAuthorEnv,
		"-resolve-empty", "-push", otherSpec, dstSpec,
	})
	if err == nil {
		t.Fatal("different source prefix was allowed to resolve the paused patch")
	}
	if !strings.Contains(out, "different source, destination, or sync configuration") {
		t.Fatalf("wrong-source-scope refusal was not explicit:\n%s", out)
	}
	if got := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD")); got != headBefore {
		t.Fatalf("wrong-source-scope refusal moved HEAD: %s -> %s", headBefore, got)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "rebase-apply")); err != nil {
		t.Fatalf("wrong-source-scope refusal cleared the paused session: %v", err)
	}
}

func TestResolveEmptyAMRejectsMovedHead(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "base\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	dst.write("f.txt", "destination version\n")
	dst.commit("destination change")
	dst.push()
	src.write("proj/f.txt", "source version\n")
	src.commit("conflicting source commit")
	src.push()
	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "conflict") {
		t.Fatalf("expected a conflict, got:\n%s", out)
	}

	clone := gritCloneDir(t, dst.bare, "", testBranch)
	runGit(t, clone, "checkout", "HEAD", "--", "f.txt")
	originalHead := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(runGitOut(t, clone, "write-tree"))
	movedHead := strings.TrimSpace(runGitOut(t, clone,
		"-c", "user.name=test",
		"-c", "user.email="+testAuthorEnv,
		"commit-tree", tree, "-p", originalHead, "-m", "manual commit during pause"))
	runGit(t, clone, "update-ref", "HEAD", movedHead, originalHead)

	out, err := runGrit(src.gritBin, []string{
		"-config=user.name=test,user.email=" + testAuthorEnv,
		"-resolve-empty", srcSpec, dstSpec,
	})
	if err == nil {
		t.Fatal("moved HEAD was accepted as the original paused am base")
	}
	if !strings.Contains(out, "HEAD moved during the paused git am session") {
		t.Fatalf("moved-HEAD refusal was not explicit:\n%s", out)
	}
	if got := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD")); got != movedHead {
		t.Fatalf("moved-HEAD refusal changed HEAD: %s -> %s", movedHead, got)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "rebase-apply")); err != nil {
		t.Fatalf("moved-HEAD refusal cleared the paused session: %v", err)
	}
}

func TestResolveEmptyAMRejectsUntaggedPatch(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "base f\n")
	src.write("proj/g.txt", "base g\n")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	dst.write("f.txt", "destination f\n")
	dst.write("g.txt", "source g\n")
	dst.commit("destination change")
	dst.push()
	src.write("proj/f.txt", "source f\n")
	src.write("proj/g.txt", "source g\n")
	src.commit("partially converged source commit")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "conflict") {
		t.Fatalf("expected a conflict, got:\n%s", out)
	}

	clone := gritCloneDir(t, dst.bare, "", testBranch)
	runGit(t, clone, "checkout", "HEAD", "--", "f.txt")
	headBefore := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD"))
	out, err := runGrit(src.gritBin, []string{
		"-config=user.name=test,user.email=" + testAuthorEnv,
		"-resolve-empty", srcSpec, dstSpec,
	})
	if err == nil || !strings.Contains(out, "not a single tagged Grit patch") {
		t.Fatalf("untagged paused patch was not refused clearly (err=%v):\n%s", err, out)
	}
	if got := strings.TrimSpace(runGitOut(t, clone, "rev-parse", "HEAD")); got != headBefore {
		t.Fatalf("untagged-patch refusal moved HEAD: %s -> %s", headBefore, got)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "rebase-apply")); err != nil {
		t.Fatalf("untagged-patch refusal cleared the paused session: %v", err)
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
