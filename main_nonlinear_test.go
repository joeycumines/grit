// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// ignoring .git directories.
func compareDirs(t *testing.T, a, b string) {
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

// TestGritNonLinearHistoryResume verifies that incremental sync copies
// side-branch commits merged into the source branch after the last synced
// commit. Against the upstream --ancestry-path behavior, the side branch
// work is silently skipped and the final comparison fails.
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
