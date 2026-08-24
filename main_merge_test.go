package main_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file verifies that grit replicates merge-commit content: each merge
// is reconciled at its topological position into an ordinary corrective patch
// tagged with the merge's own digest, so evil resolutions, "-s ours" discards,
// and octopus unions replicate byte-exactly, once, in both directions.

// syncEvilHistory builds and synchronizes a history whose final merge
// hand-resolves proj/f.txt to "holyhand" beyond either parent's state,
// returning the merge commit's digest.
func syncEvilHistory(t *testing.T, src, dst *gritRepo, srcSpec, dstSpec string) string {
	t.Helper()
	src.write("proj/f.txt", "base")
	src.write("proj/k.txt", "keep")
	src.commit("base")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	src.git("branch", "side")
	src.write("proj/f.txt", "main")
	src.commit("main edit")

	src.git("checkout", "side")
	src.write("proj/g.txt", "g")
	src.commit("side work")

	src.git("checkout", testBranch)
	src.git("merge", "--no-ff", "--no-commit", "side")
	src.write("proj/f.txt", "holyhand")
	src.commit("evil merge")
	src.push()

	mHex := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()
	return mHex
}

// TestGritEvilMergeReplicatesByteExact proves an evil merge lands byte-exact,
// tagged with its own digest, and an immediate rerun is a no-op.
func TestGritEvilMergeReplicatesByteExact(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	mHex := syncEvilHistory(t, src, dst, srcSpec, dstSpec)

	if got := dstRead(t, dst.dir, "f.txt"); got != "holyhand" {
		t.Fatalf("evil resolution f.txt = %q, want %q", got, "holyhand")
	}
	if got := dstRead(t, dst.dir, "g.txt"); got != "g" {
		t.Fatalf("side content g.txt = %q, want %q", got, "g")
	}
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	msg := dst.gitOut("log", "-n", "1", "--format=%B")
	wantTag := "fbshipit-source-id: " + mHex
	if !strings.Contains(msg, wantTag) {
		t.Fatalf("newest destination commit %q lacks the merge digest tag %q", msg, wantTag)
	}

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("immediate rerun did not reach a fixed point:\n%s", out)
	}
}

// TestGritOursDiscardReplicatesAndStaysDiscarded proves an "-s ours" merge
// replicates as a side-content revert and is never resurrected later.
func TestGritOursDiscardReplicatesAndStaysDiscarded(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "base")
	src.commit("base")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	src.git("checkout", "-b", "side")
	src.write("proj/f.txt", "side")
	src.write("proj/novel.txt", "novel")
	src.commit("side work")
	src.git("checkout", testBranch)
	src.git("merge", "-s", "ours", "--no-ff", "-m", "discard side", "side")
	src.push()

	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	if got := dstRead(t, dst.dir, "f.txt"); got != "base" {
		t.Fatalf("discarded side content resurrected: f.txt = %q, want %q", got, "base")
	}
	if _, err := os.Stat(filepath.Join(dst.dir, "novel.txt")); !os.IsNotExist(err) {
		t.Fatalf("novel.txt exists at destination, want removed by the discard replication")
	}
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("forward rerun did not hold the fixed point:\n%s", out)
	}
	dst.pull()
	if _, err := os.Stat(filepath.Join(dst.dir, "novel.txt")); !os.IsNotExist(err) {
		t.Fatalf("novel.txt resurrected by the rerun")
	}
}

// TestGritTrivialMergeFree proves a merge matching its ancestors' state adds
// zero destination commits and takes no tag.
func TestGritTrivialMergeFree(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("base")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()
	commitsBefore := dstCommitCount(t, dst)

	src.git("branch", "side")
	src.write("proj/k.txt", "k")
	src.commit("mainline edit")
	src.git("checkout", "side")
	src.write("proj/g.txt", "g")
	src.commit("side work")
	src.git("checkout", testBranch)
	src.git("merge", "--no-ff", "-m", "trivial merge", "side")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "skipping converged merge") {
		t.Fatalf("trivial merge was not recognized as converged:\n%s", out)
	}
	dst.pull()
	if got, want := dstCommitCount(t, dst), commitsBefore+2; got != want {
		t.Fatalf("destination gained %d commits for two changes plus a trivial merge, want exactly 2", got-commitsBefore)
	}
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
}

func dstCommitCount(t *testing.T, r *gritRepo) int {
	t.Helper()
	out := strings.TrimSpace(r.gitOut("rev-list", "--count", "HEAD"))
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("rev-list --count returned %q: %v", out, err)
	}
	return n
}

// foreignClone clones the provided bare repository into a disposable
// working copy, simulating a third party pushing out-of-band edits.
func foreignClone(t *testing.T, bare string) (dir string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "grit-foreign")
	if err != nil {
		t.Fatal(err)
	}
	cleanup = func() { os.RemoveAll(dir) }
	cmd := exec.Command("git", "clone", bare, ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil && !bytes.Contains(out, []byte("empty repository")) {
		t.Fatalf("git clone %s: %v\n%s", bare, err, out)
	}
	runGitConfigured(t, dir, "config", "user.email", testAuthorEnv)
	runGitConfigured(t, dir, "config", "user.name", testCommitter)
	return dir, cleanup
}

// runGitConfigured behaves like runGit but supplies a commit identity,
// which session-concluding operations such as am --continue require.
func runGitConfigured(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{
		"-c", "user.name=" + testCommitter,
		"-c", "user.email=" + testAuthorEnv,
	}, args...)
	runGit(t, dir, full...)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0666); err != nil {
		t.Fatal(err)
	}
}

// TestGritOctopusMergeReplicates proves that a merge with more than
// two parents replicates its union effect.
func TestGritOctopusMergeReplicates(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("base")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	src.git("checkout", "-b", "b1")
	src.write("proj/one.txt", "1")
	src.commit("branch one")
	src.git("checkout", testBranch)
	src.git("checkout", "-b", "b2")
	src.write("proj/two.txt", "2")
	src.commit("branch two")
	src.git("checkout", testBranch)
	src.git("merge", "--no-ff", "-m", "octopus merge", "b1", "b2")
	src.push()

	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	for name, want := range map[string]string{"one.txt": "1", "two.txt": "2"} {
		if got := dstRead(t, dst.dir, name); got != want {
			t.Fatalf("octopus content %s = %q, want %q", name, got, want)
		}
	}
	parents := strings.Fields(src.gitOut("rev-list", "--parents", "-n", "1", testBranch))
	if len(parents) != 4 {
		t.Fatalf("fixture error: octopus merge has %d parents, want 3", len(parents)-1)
	}
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("rerun after octopus replication did not reach a fixed point:\n%s", out)
	}
}

// TestGritBinaryEvilMergeRoundTrips proves that binary content
// hand-resolved inside a merge round-trips byte-exactly.
func TestGritBinaryEvilMergeRoundTrips(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	pattern := func(f byte) []byte {
		b := make([]byte, 256)
		for i := range b {
			b[i] = f ^ byte(i)
		}
		return b
	}
	v1, v2 := pattern(0x00), pattern(0xFF)

	src.write("proj/base.txt", "v1")
	src.commit("base")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	src.git("branch", "side")
	src.writeBytes("proj/blob.dat", v1)
	src.commit("add binary")
	src.git("checkout", "side")
	src.write("proj/unrelated.txt", "u")
	src.commit("side work")
	src.git("checkout", testBranch)
	src.git("merge", "--no-ff", "--no-commit", "side")
	src.writeBytes("proj/blob.dat", v2)
	src.commit("evil binary merge")
	src.push()

	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	got, err := os.ReadFile(filepath.Join(dst.dir, "blob.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, v2) {
		t.Fatalf("binary evil resolution did not round-trip: got %d bytes, want the hand-resolved content", len(got))
	}
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
}

// TestGritBidirectionalMergeStability runs forward-reverse-forward over a
// merge-carrying history; both remotes must hold identical trees throughout.
func TestGritBidirectionalMergeStability(t *testing.T) {
	src, dst := setupGritRepos(t)
	forwardSrc := src.bare + ",proj/," + testBranch
	forwardDst := dst.bare + ",," + testBranch

	mHex := syncEvilHistory(t, src, dst, forwardSrc, forwardDst)
	shipitsAfterForward := dst.shipitCount()

	// Reverse leg: nothing may copy back. The source-side commits are
	// excluded by their shipit ids where they appear; anything left
	// converges against already-present content, so the leg ends at a
	// fixed point without applying.
	reverseSrc := dst.bare + ",," + testBranch
	reverseDst := src.bare + ",proj/," + testBranch
	out := gritOutput(t, src.gritBin, reverseSrc, reverseDst)
	if strings.Contains(out, "applying") {
		t.Fatalf("reverse leg applied commits that should have been excluded by their tags:\n%s", out)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("reverse leg did not reach a fixed point:\n%s", out)
	}

	// Second forward leg: the fixed point must hold.
	out = gritOutput(t, src.gritBin, forwardSrc, forwardDst)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("second forward leg did not reach a fixed point:\n%s", out)
	}
	if strings.Contains(out, "pushing changes") {
		t.Fatalf("fixed-point run attempted a push:\n%s", out)
	}
	if got := dst.shipitCount(); got != shipitsAfterForward {
		t.Fatalf("ping-pong changed the destination's tagged-commit count from %d to %d", shipitsAfterForward, got)
	}
	dst.pull()
	if got := dstRead(t, dst.dir, "f.txt"); got != "holyhand" {
		t.Fatalf("merge content drifted: f.txt = %q, want %q (merge %s)", got, "holyhand", mHex[:7])
	}
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
}

// TestGritMergeRespectsStripRules proves strip rules gate merge content like
// regular commits: no smuggling, survivors replicate, corrective untagged.
func TestGritMergeRespectsStripRules(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "base")
	src.commit("base")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	src.git("branch", "side")
	src.write("proj/f.txt", "main")
	src.commit("main edit")
	src.git("checkout", "side")
	src.write("proj/secret.txt", "internal")
	src.commit("internal work")
	src.git("checkout", testBranch)
	src.git("merge", "--no-ff", "--no-commit", "side")
	src.write("proj/f.txt", "holy")
	src.commit("evil merge")
	src.push()

	gritOutput(t, src.gritBin, srcSpec, dstSpec, "strip:^secret.txt$")
	dst.pull()

	if _, err := os.Stat(filepath.Join(dst.dir, "secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("stripped path leaked into the destination through merge reconciliation")
	}
	if got := dstRead(t, dst.dir, "f.txt"); got != "holy" {
		t.Fatalf("evil resolution did not replicate alongside the strip: f.txt = %q", got)
	}
	msg := dst.gitOut("log", "-n", "1", "--format=%B")
	if strings.Contains(msg, "fbshipit-source-id:") {
		t.Fatalf("partially stripped merge was permanently tagged:\n%s", msg)
	}
	if !strings.Contains(msg, "grit-convergence-pruned:") {
		t.Fatalf("partially stripped merge lacks the re-examinability marker:\n%s", msg)
	}

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec, "strip:^secret.txt$")
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("rerun with strip rules did not reach a fixed point:\n%s", out)
	}
	dst.pull()
	if _, err := os.Stat(filepath.Join(dst.dir, "secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("stripped path resurrected on rerun")
	}
}

// TestGritForeignEditOnMergedPathPausesThenConverges proves a foreign edit on
// a merged path pauses loudly, then converges byte-exact after resolution.
func TestGritForeignEditOnMergedPathPausesThenConverges(t *testing.T) {
	src, dst := setupGritRepos(t)
	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/f.txt", "base")
	src.commit("base")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// A foreign party pushes a competing edit to the same path.
	foreign, cleanup := foreignClone(t, dst.bare)
	defer cleanup()
	writeFile(t, filepath.Join(foreign, "f.txt"), "alien")
	runGit(t, foreign, "add", "-A")
	runGit(t, foreign, "commit", "-m", "foreign edit")
	runGit(t, foreign, "push", "origin", "HEAD:"+testBranch)

	// The source diverges on that path and merges, resolving by hand.
	src.git("checkout", "-b", "side")
	src.write("proj/f.txt", "sideedit")
	src.commit("side edit")
	src.git("checkout", testBranch)
	src.git("merge", "--no-ff", "--no-commit", "side")
	src.write("proj/f.txt", "holyhand")
	src.commit("evil merge")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "conflict") || !strings.Contains(out, "paused") {
		t.Fatalf("foreign edit on a merged path did not pause loudly:\n%s", out)
	}
	cloneDir := gritCloneDir(t, dst.bare, "", testBranch)
	if _, err := os.Stat(filepath.Join(cloneDir, ".git", "rebase-apply")); err != nil {
		t.Fatalf("no paused am session found in %s: %v", cloneDir, err)
	}

	// Resolve manually in favor of the incoming side edit, conclude the
	// session, and re-run: the remainder, including the merge, must
	// converge to the source state.
	writeFile(t, filepath.Join(cloneDir, "f.txt"), "sideedit")
	runGitConfigured(t, cloneDir, "add", "f.txt")
	runGitConfigured(t, cloneDir, "am", "--continue")
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	if got := dstRead(t, dst.dir, "f.txt"); got != "holyhand" {
		t.Fatalf("post-resolution convergence f.txt = %q, want %q", got, "holyhand")
	}
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
}

// TestGritBidirectionalFixedPoint: after forward-reverse-forward over plain
// history, the last leg is a true fixed point leaving identical remote trees.
func TestGritBidirectionalFixedPoint(t *testing.T) {
	src, dst := setupGritRepos(t)
	forwardSrc := src.bare + ",proj/," + testBranch
	forwardDst := dst.bare + ",," + testBranch

	src.write("proj/one.txt", "1")
	src.commit("first")
	src.write("proj/two.txt", "2")
	src.commit("second")
	src.write("proj/three.txt", "3")
	src.commit("third")
	src.push()

	gritOutputStrict(t, src.gritBin, forwardSrc, forwardDst)

	reverseSrc := dst.bare + ",," + testBranch
	reverseDst := src.bare + ",proj/," + testBranch
	out := gritOutputStrict(t, src.gritBin, reverseSrc, reverseDst)
	if strings.Contains(out, "applying") {
		t.Fatalf("reverse leg applied commits the destination already accounted for:\n%s", out)
	}

	out = gritOutputStrict(t, src.gritBin, forwardSrc, forwardDst)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("third leg did not reach a fixed point:\n%s", out)
	}
	if strings.Contains(out, "pushing changes") {
		t.Fatalf("fixed-point run attempted a push:\n%s", out)
	}
	if strings.Contains(out, "applying") {
		t.Fatalf("fixed-point run applied commits:\n%s", out)
	}

	bareTree := func(bare, rev string) string {
		t.Helper()
		out, err := exec.Command("git", "-C", bare,
			"rev-parse", rev).CombinedOutput()
		if err != nil {
			t.Fatalf("rev-parse %s in %s: %v\n%s", rev, bare, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// The destination mirrors the source's proj/ subtree at its root.
	srcTree := bareTree(src.bare, testBranch+":proj")
	dstTree := bareTree(dst.bare, testBranch+"^{tree}")
	if srcTree != dstTree {
		srcRaw, _ := exec.Command("git", "-C", src.bare, "cat-file", "-p", srcTree).CombinedOutput()
		dstRaw, _ := exec.Command("git", "-C", dst.bare, "cat-file", "-p", dstTree).CombinedOutput()
		t.Fatalf("bare remotes diverged: src %s:proj=%s dst main^tree=%s\nsrc raw:\n%s\ndst raw:\n%s",
			testBranch, srcTree, dstTree, srcRaw, dstRaw)
	}
}

// TestGritIndentedQuoteIsNotAnAnchor pins that only grit's flush-left tags
// anchor the resume walk; quoted digests fall to the tolerant exclusion set.
func TestGritIndentedQuoteIsNotAnAnchor(t *testing.T) {
	src, dst := setupGritRepos(t)
	forwardSrc := src.bare + ",proj/," + testBranch
	forwardDst := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "base")
	src.commit("base")
	src.push()
	gritOutputStrict(t, src.gritBin, forwardSrc, forwardDst)

	// Two unsynced source commits: U lands before S, so a tolerant walk
	// bound at U's digest would permanently skip U.
	src.write("proj/u.txt", "u")
	src.commit("unsynced older")
	uHex := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.write("proj/s.txt", "s")
	src.commit("unsynced newer")
	src.push()

	// A foreign commit atop the synced base quotes U's digest (uHex)
	// on an indented line and carries the convergence marker so the
	// preservation gate accepts it as grit-authored state.
	foreign, cleanup := foreignClone(t, dst.bare)
	defer cleanup()
	writeFile(t, filepath.Join(foreign, "q.txt"), "q")
	runGit(t, foreign, "add", "-A")
	runGitConfigured(t, foreign, "commit", "-m", "quoted anchor",
		"-m", "    fbshipit-source-id: "+uHex,
		"-m", "grit-convergence-pruned: 1/1")
	runGitConfigured(t, foreign, "push", "origin", "HEAD:"+testBranch)

	out := gritOutputStrict(t, src.gritBin, forwardSrc, forwardDst)
	// THE WALK FIX, pinned: the selection range must span back to the
	// genuine base anchor (2 commits) rather than stopping at the
	// quoted digest (which yielded "1 commits to copy" under the old
	// ^\s*-tolerant walk).
	if !strings.Contains(out, "2 commits to copy") {
		t.Fatalf("indented quotation bounded the resume range:\n%s", out)
	}
	// Deliberately tolerant tag-exclusion (a documented residual
	// ambiguity shared with the anchor's historic tolerance) then drops
	// U because Q quotes its digest; S still replicates. Re-tightening
	// the exclusion set is outside this change's scope.
	if !strings.Contains(out, "skipping already synchronized "+uHex[:7]) {
		t.Fatalf("quoted digest was not recognized by tag exclusion:\n%s", out)
	}
	dst.pull()
	if got := dstRead(t, dst.dir, "s.txt"); got != "s" {
		t.Fatalf("commit newer than the quoted digest did not land: s.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst.dir, "u.txt")); !os.IsNotExist(err) {
		t.Fatalf("tag-excluded commit unexpectedly landed: %v", err)
	}
}
