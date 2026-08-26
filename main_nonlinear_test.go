package main_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This file contains end-to-end regression tests for grit's incremental
// (resume) synchronization on source repositories whose histories are not
// linear.
//
// Upstream grit resumed syncing with:
//
//	git log <lastSyncedSourceCommit>..<branch> --ancestry-path --no-merges
//
// --ancestry-path restricts results to commits that are DESCENDANTS of the
// last synced source commit. When work is developed on a side branch and
// later merged into the synced branch ("Merge branch 'wip'"), the side
// branch's commits are not descendants of an older tip of the target branch,
// so grit silently skipped them: it printed "nothing to do" and exited 0
// while the destination diverged arbitrarily far from the source.
//
// The fix copies every commit in lastSyncedSourceCommit..<branch> (the
// ancestry-path restriction is dropped) and applies them in topological
// order (--topo-order), so patches are always applied after their parents'
// patches regardless of authorship dates or merge topology. A second fix
// passes --binary to format-patch so that binary file deltas serialize as
// applicable git binary patches instead of unappliable stubs.
//
// These tests are self-contained: they build the grit binary with `go build`
// and construct all repositories with the git CLI, requiring only that git
// understands `-c init.defaultBranch=<branch>` (git >= 2.28). Only the
// standard library testing package is used.

const (
	testBranch    = "main"
	testAuthorEnv = "test@example.com"
	testCommitter = "test"
)

type gritRepo struct {
	t        *testing.T
	dir      string // working clone
	bare     string // bare remote
	gritBin  string
	sequence int
}

func (r *gritRepo) git(args ...string) {
	r.t.Helper()
	r.gitOk(args...)
}

// gitOut runs git and returns stdout, failing the test on error.
func (r *gritRepo) gitOut(args ...string) string {
	r.t.Helper()
	return r.gitOk(args...)
}

func (r *gritRepo) gitOk(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (r *gritRepo) write(path, content string) {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0777); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0666); err != nil {
		r.t.Fatal(err)
	}
}

func (r *gritRepo) writeBytes(path string, content []byte) {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0777); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0666); err != nil {
		r.t.Fatal(err)
	}
}

// commit commits all pending changes with deterministic, strictly increasing
// author/committer dates so that date-order and topology-order are
// distinguishable if a regression ever conflates them.
func (r *gritRepo) commit(msg string) {
	r.t.Helper()
	r.sequence++
	ts := fmt.Sprintf("@%d +0000", 1700000000+r.sequence*60)
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME="+testCommitter,
		"GIT_AUTHOR_EMAIL="+testAuthorEnv,
		"GIT_AUTHOR_DATE="+ts,
		"GIT_COMMITTER_NAME="+testCommitter,
		"GIT_COMMITTER_EMAIL="+testAuthorEnv,
		"GIT_COMMITTER_DATE="+ts,
	)
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-m", msg},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			r.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func (r *gritRepo) push() {
	r.t.Helper()
	r.git("push", "origin", "HEAD")
}

func (r *gritRepo) pull() {
	r.t.Helper()
	r.git("pull", "--ff-only")
}

// gritSync runs the built grit binary to synchronize this repository into
// dst, applying the provided extra arguments (e.g. -push, prefix specs).
func (r *gritRepo) gritSync(dst *gritRepo, extraArgs ...string) {
	r.t.Helper()
	args := append([]string{
		"-config=user.name=test,user.email=" + testAuthorEnv,
	}, extraArgs...)
	cmd := exec.Command(r.gritBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("grit %v: %v\n%s", args, err, out)
	}
}

// compareDirs fails the test unless the two directory trees are identical,
// ignoring .git directories. It shells out to POSIX diff and therefore
// skips on platforms without one.
func compareDirs(t *testing.T, a, b string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX diff is unavailable on windows")
	}
	t.Helper()
	cmd := exec.Command("diff", "-r", "-x", ".git", a, b)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("trees differ between %s and %s:\n%s", a, b, out)
	}
}

// shipitCount returns the number of commits in the repository carrying a
// grit-applied fbshipit-source-id trailer.
func (r *gritRepo) shipitCount() int {
	r.t.Helper()
	out := r.gitOut("log", "--format=%H", "--grep=^\\s*\\(fb\\)\\?shipit-source-id: [a-z0-9]\\+$")
	n := 0
	for _, line := range bytes.Split([]byte(out), []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}

// setupGritRepos builds the grit binary and creates a bare source
// repository, a bare destination repository, and working clones of both.
// TEST_TMPDIR is overridden so that grit's cached clones under git.Dir are
// isolated per-test.
func setupGritRepos(t *testing.T) (src, dst *gritRepo) {
	t.Helper()
	t.Setenv("TEST_TMPDIR", t.TempDir())

	bin := filepath.Join(t.TempDir(), "grit")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	base := t.TempDir()
	mk := func(name string) *gritRepo {
		return &gritRepo{t: t, gritBin: bin,
			bare: filepath.Join(base, name+".git"),
			dir:  filepath.Join(base, name)}
	}
	src, dst = mk("src"), mk("dst")

	initBare := func(bare string) {
		cmd := exec.Command("git", "-c", "init.defaultBranch="+testBranch, "init", "--bare", "-b", testBranch, bare)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init --bare %s: %v\n%s", bare, err, out)
		}
	}
	initBare(src.bare)
	initBare(dst.bare)

	for _, r := range []*gritRepo{src, dst} {
		clone := exec.Command("git", "clone", r.bare, r.dir)
		if out, err := clone.CombinedOutput(); err != nil && !bytes.Contains(out, []byte("empty repository")) {
			t.Fatalf("git clone %s: %v\n%s", r.bare, err, out)
		}
		r.git("config", "user.email", testAuthorEnv)
		r.git("config", "user.name", testCommitter)
	}
	// Grit requires the destination to contain at least one commit.
	dst.git("commit", "--allow-empty", "-m", "initial commit")
	dst.push()
	return src, dst
}

// TestGritNonLinearHistoryResume verifies incremental sync copies side-branch
// commits merged into the source branch after the last synced commit; upstream
// --ancestry-path behavior silently skips them.
func TestGritNonLinearHistoryResume(t *testing.T) {
	src, dst := setupGritRepos(t)

	spec := func(remote, prefix string) string {
		return remote + "," + prefix + "," + testBranch
	}
	srcSpec := spec(src.bare, "proj/")
	dstSpec := spec(dst.bare, "")

	// Initial sync: proj/f1 arrives at the destination root as f1.
	src.write("proj/f1", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	// Establish a second sync point (this becomes the resume anchor whose
	// source-id resolves in future runs).
	src.write("proj/f1", "v2")
	src.commit("second commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
	syncPointShipits := dst.shipitCount()

	// Side branch work, forked from BEFORE the latest synced commit: the
	// side commits are not descendants of the resume anchor. Two commits
	// touch the same file sequentially so that correct ordering (parents
	// before children) is observable.
	src.git("branch", "side", "HEAD~1")
	src.git("checkout", "side")
	src.write("proj/side.txt", "a")
	src.commit("side work part 1")
	src.write("proj/side.txt", "b")
	src.commit("side work part 2")

	// Merge the side branch into the source branch, then add an unrelated
	// commit outside the synchronized prefix.
	src.git("checkout", testBranch)
	src.git("merge", "--no-ff", "-m", "Merge branch 'side'", "side")
	src.write("top.txt", "outside the prefix")
	src.commit("out-of-prefix change")
	src.push()

	// Incremental sync must now copy the side-branch commits.
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	// Exactly the three in-range, non-merge, non-empty commits must have
	// been copied as shipit-tagged commits (two side commits + the
	// out-of-prefix commit contributes no prefix diffs and is skipped).
	if got, want := dst.shipitCount(), syncPointShipits+2; got != want {
		t.Fatalf("destination has %d shipit-tagged commits after incremental sync, want %d", got, want)
	}
	sideContent, err := os.ReadFile(filepath.Join(dst.dir, "side.txt"))
	if err != nil {
		t.Fatalf("side.txt missing at destination: %v", err)
	}
	if string(sideContent) != "b" {
		t.Fatalf("side.txt = %q, want final side-branch value %q", sideContent, "b")
	}
}

// TestGritUnrelatedMergedHistoryIsReconciledNotReplayed pins the absorption
// rule: commits merged in from an unrelated history arrive through their
// merge's reconciliation, never as individually replayed patches: their
// diffs assume parent states the destination never held.
func TestGritUnrelatedMergedHistoryIsReconciledNotReplayed(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// A foreign repository's history enters the source repository through
	// an unrelated-histories merge: an orphan lineage sharing no commit
	// object with the source branch.
	src.git("checkout", "--orphan", "foreign")
	src.git("rm", "-rf", ".")
	src.write("proj/upstream-legacy.txt", "v1")
	src.commit("upstream: initial import")
	foreignImport := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.write("proj/upstream-legacy.txt", "v2")
	src.commit("upstream: harden legacy path")
	foreignHarden := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.git("checkout", testBranch)
	src.git("merge", "--allow-unrelated-histories", "--no-ff",
		"-m", "Merge unrelated upstream history", "foreign")

	src.write("proj/main-feature.txt", "F")
	src.commit("mainline feature after absorption")
	src.push()

	out := gritOutputStrict(t, src.gritBin, srcSpec, dstSpec)
	for _, leaked := range []struct{ digest, subject string }{
		{foreignImport, "upstream: initial import"},
		{foreignHarden, "upstream: harden legacy path"},
	} {
		if strings.Contains(out, "applying "+leaked.digest[:8]) {
			t.Fatalf("unrelated merged history was replayed as patches: %s\n%s", leaked.subject, out)
		}
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	subjects := dst.gitOut("log", "--format=%s")
	for _, bad := range []string{"upstream: initial import", "upstream: harden legacy path"} {
		if strings.Contains(subjects, bad) {
			t.Fatalf("commit from unrelated merged history was copied into the destination: %q\nall subjects:\n%s", bad, subjects)
		}
	}

	// Exactly four commits may exist: the seed, the mirrored first
	// commit, the merge's reconciliation, and the mainline feature. Any
	// more means foreign lineage leaked through as individual copies.
	if got := strings.Count(strings.TrimSpace(dst.gitOut("log", "--format=%H")), "\n") + 1; got != 4 {
		t.Fatalf("destination carries %d commits, want exactly 4:\n%s", got, dst.gitOut("log", "--oneline"))
	}

	if got := dstRead(t, dst.dir, "upstream-legacy.txt"); got != "v2" {
		t.Fatalf("merge reconciliation did not land the absorbed content: %q", got)
	}
	if got := dstRead(t, dst.dir, "main-feature.txt"); got != "F" {
		t.Fatalf("post-absorption mainline commit did not land: %q", got)
	}

	out = gritOutputStrict(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("absorbed history did not reach a fixed point:\n%s", out)
	}
}

// TestGritReversedUnrelatedMergeIsReconciledNotReplayed pins that the
// absorption rule does not depend on merge parent order: git records the
// first parent from the committer's checkout state, so absorbing via a
// merge authored ON the unrelated branch (mainline then fast-forwarded to
// the result) puts the foreign tip FIRST, and the exclusion must still fire.
func TestGritReversedUnrelatedMergeIsReconciledNotReplayed(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// The absorption merge is authored on the foreign branch, making the
	// unrelated history the FIRST parent; mainline is fast-forwarded to
	// the result afterwards.
	src.git("checkout", "--orphan", "foreign")
	src.git("rm", "-rf", ".")
	src.write("proj/upstream-legacy.txt", "v1")
	src.commit("upstream: initial import")
	foreignImport := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.write("proj/upstream-legacy.txt", "v2")
	src.commit("upstream: harden legacy path")
	foreignHarden := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.git("merge", "--allow-unrelated-histories", "--no-ff",
		"-m", "Merge mainline into upstream tracking branch", testBranch)
	src.git("checkout", testBranch)
	src.git("merge", "--ff-only", "foreign")

	src.write("proj/main-feature.txt", "F")
	src.commit("mainline feature after reversed absorption")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	for _, leaked := range []struct{ digest, subject string }{
		{foreignImport, "upstream: initial import"},
		{foreignHarden, "upstream: harden legacy path"},
	} {
		if strings.Contains(out, "applying "+leaked.digest[:8]) {
			t.Fatalf("unrelated merged history was replayed as patches: %s\n%s", leaked.subject, out)
		}
	}
	if !strings.Contains(out, "unrelated history") {
		t.Fatalf("exclusion of the foreign first parent was not announced:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	subjects := dst.gitOut("log", "--format=%s")
	for _, bad := range []string{"upstream: initial import", "upstream: harden legacy path"} {
		if strings.Contains(subjects, bad) {
			t.Fatalf("commit from unrelated merged history was copied into the destination: %q\nall subjects:\n%s", bad, subjects)
		}
	}

	// Exactly four commits may exist: the seed, the mirrored first
	// commit, the merge's reconciliation, and the mainline feature. Any
	// more means foreign lineage leaked through as individual copies.
	if got := strings.Count(strings.TrimSpace(dst.gitOut("log", "--format=%H")), "\n") + 1; got != 4 {
		t.Fatalf("destination carries %d commits, want exactly 4:\n%s", got, dst.gitOut("log", "--oneline"))
	}
	if got := dstRead(t, dst.dir, "upstream-legacy.txt"); got != "v2" {
		t.Fatalf("merge reconciliation did not land the absorbed content: %q", got)
	}
	if got := dstRead(t, dst.dir, "main-feature.txt"); got != "F" {
		t.Fatalf("post-absorption mainline commit did not land: %q", got)
	}

	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("reversed absorbed history did not reach a fixed point:\n%s", out)
	}
}

// TestGritForeignFirstParentBehindSharedMergeIsExcluded pins that an
// unrelated lineage is excluded even when it reaches the synced branch
// through a foreign-side merge whose SECOND parent shares the anchor: the
// shared second parent must not mask the disjoint first parent.
func TestGritForeignFirstParentBehindSharedMergeIsExcluded(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// A foreign orphan lineage internally merges a side branch that DOES
	// descend from the synced mainline; the outer merge then absorbs the
	// foreign tip into the mainline.
	src.git("checkout", "--orphan", "foreign")
	src.git("rm", "-rf", ".")
	src.write("proj/legacy-a.txt", "a1")
	src.commit("upstream: import module")
	foreignImport := strings.TrimSpace(src.gitOut("rev-parse", "HEAD"))
	src.git("branch", "side", testBranch)
	src.git("checkout", "side")
	src.write("proj/side-note.txt", "s")
	src.commit("side work on synced history")
	src.git("checkout", "foreign")
	src.git("merge", "--allow-unrelated-histories", "--no-ff",
		"-m", "foreign merges side work", "side")
	src.git("checkout", testBranch)
	src.git("merge", "--allow-unrelated-histories", "--no-ff",
		"-m", "absorb foreign module", "foreign")

	src.write("proj/main-feature.txt", "F")
	src.commit("mainline feature after nested absorption")
	src.push()

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if strings.Contains(out, "applying "+foreignImport[:8]) {
		t.Fatalf("unrelated merged history was replayed as patches:\n%s", out)
	}
	if !strings.Contains(out, "unrelated history") {
		t.Fatalf("exclusion of the foreign first parent was not announced:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	subjects := dst.gitOut("log", "--format=%s")
	if strings.Contains(subjects, "upstream: import module") {
		t.Fatalf("commit from the unrelated lineage was copied into the destination:\n%s", subjects)
	}
	if !strings.Contains(subjects, "side work on synced history") {
		t.Fatalf("anchored side branch was dropped along with the foreign history:\n%s", subjects)
	}
	if got := dstRead(t, dst.dir, "legacy-a.txt"); got != "a1" {
		t.Fatalf("merge reconciliation did not land the absorbed content: %q", got)
	}
	if got := dstRead(t, dst.dir, "side-note.txt"); got != "s" {
		t.Fatalf("anchored side branch content did not land: %q", got)
	}
	if got := dstRead(t, dst.dir, "main-feature.txt"); got != "F" {
		t.Fatalf("post-absorption mainline commit did not land: %q", got)
	}

	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("nested absorbed history did not reach a fixed point:\n%s", out)
	}
}

// TestGritInitialSyncConvergesAbsorbedHistory pins initial synchronization
// semantics for a source absorbed before its first sync: with no resume
// anchor there is no revision to be disjoint from, so every root is
// replayed, and each merge still reconciles net content at its topological
// position: the mirrored tree converges exactly once either way.
func TestGritInitialSyncConvergesAbsorbedHistory(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("mainline root")
	src.git("checkout", "--orphan", "foreign")
	src.git("rm", "-rf", ".")
	src.write("proj/upstream-legacy.txt", "v1")
	src.commit("upstream: initial import")
	src.write("proj/upstream-legacy.txt", "v2")
	src.commit("upstream: harden legacy path")
	src.git("checkout", testBranch)
	src.git("merge", "--allow-unrelated-histories", "--no-ff",
		"-m", "Merge unrelated upstream history", "foreign")
	src.write("proj/main-feature.txt", "F")
	src.commit("mainline feature after absorption")
	src.push()

	gritOutput(t, src.gritBin, srcSpec, dstSpec)
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	if got := dstRead(t, dst.dir, "upstream-legacy.txt"); got != "v2" {
		t.Fatalf("absorbed content did not land during initial sync: %q", got)
	}
	if got := dstRead(t, dst.dir, "main-feature.txt"); got != "F" {
		t.Fatalf("post-absorption mainline commit did not land: %q", got)
	}

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("initially synchronized absorbed history did not reach a fixed point:\n%s", out)
	}
}

// TestGritDisjointResumeAnchorSkipsExclusionFilter pins the guard for
// resume anchors sharing no history with the current source branch (a
// rewritten source history whose pre-rewrite digests still resolve in the
// cached clone): classifying merges against such a reference would discard
// legitimate side branches, so the filter stands down loudly instead.
func TestGritDisjointResumeAnchorSkipsExclusionFilter(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/base.txt", "v1")
	src.commit("first commit")
	src.write("proj/base.txt", "v2")
	src.commit("second commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()

	// Rebuild the source history from an orphan root so the synced anchor
	// digest no longer precedes the branch tip while still resolving in
	// grit's cached source clone. The rebuilt tree keeps base.txt so the
	// mirrored subtree stays comparable across the rewrite.
	src.git("checkout", "--orphan", "rewritten")
	src.git("rm", "-rf", ".")
	src.write("proj/base.txt", "v2")
	src.write("proj/fresh.txt", "f1")
	src.commit("rebuilt root")
	src.git("checkout", "-b", "topic")
	src.write("proj/topic.txt", "t1")
	src.commit("topic work")
	src.git("checkout", "rewritten")
	src.git("merge", "--no-ff", "-m", "Merge branch 'topic'", "topic")
	src.git("push", "--force", "origin", "rewritten:"+testBranch)

	out := gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "shares no history") {
		t.Fatalf("disjoint resume anchor did not stand down the exclusion filter:\n%s", out)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	if got := dstRead(t, dst.dir, "topic.txt"); got != "t1" {
		t.Fatalf("side branch was dropped under a disjoint resume anchor: %q", got)
	}
	if got := dstRead(t, dst.dir, "fresh.txt"); got != "f1" {
		t.Fatalf("rebuilt root content did not land: %q", got)
	}

	out = gritOutput(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("rewritten history did not reach a fixed point:\n%s", out)
	}
}

// TestGritBinaryFiles verifies that binary file changes within an
// incremental sync range are serialized as applicable binary patches and
// converge byte-for-byte. Without --binary, format-patch emits a
// "Binary files differ" stub and git am refuses to apply it.
func TestGritBinaryFiles(t *testing.T) {
	src, dst := setupGritRepos(t)

	srcSpec := src.bare + ",proj/," + testBranch
	dstSpec := dst.bare + ",," + testBranch

	src.write("proj/text.txt", "text v1")
	src.commit("first commit")
	src.push()
	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	full := make([]byte, 256)
	for i := range full {
		full[i] = byte(i)
	}
	reversed := make([]byte, 256)
	for i := range reversed {
		reversed[i] = byte(255 - i)
	}

	// Add a binary file and modify it within the unsynced range.
	src.writeBytes("proj/blob.dat", full)
	src.commit("add binary file")
	src.writeBytes("proj/blob.dat", reversed)
	src.write("proj/text.txt", "text v2")
	src.commit("modify binary file")
	src.push()

	src.gritSync(dst, "-push", srcSpec, dstSpec)
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)

	got, err := os.ReadFile(filepath.Join(dst.dir, "blob.dat"))
	if err != nil {
		t.Fatalf("blob.dat missing at destination: %v", err)
	}
	if !bytes.Equal(got, reversed) {
		t.Fatalf("blob.dat did not converge byte-for-byte: got %d bytes, want final reversed content", len(got))
	}
}
