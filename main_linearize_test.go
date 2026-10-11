package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers -linearize end to end: grit rewrites its cached
// source clone into single-parent history before copying commits,
// recomputing the rewrite each run against the untouched remote.

// buildTangledHistory creates interleaved branches and merges: two side
// branches with disjoint files, merged back at staggered points, with a
// mainline commit between them. Six non-merge commits carry content;
// two merges tangle the topology.
func buildTangledHistory(t *testing.T, src *gritRepo) {
	t.Helper()
	src.write("proj/base.txt", "base")
	src.commit("base")
	src.push()

	src.git("checkout", "-q", "-b", "b1")
	src.write("proj/one.txt", "one")
	src.commit("branch one")
	src.git("checkout", "-q", testBranch)
	src.write("proj/two.txt", "two")
	src.commit("mainline two")

	src.git("merge", "-q", "--no-ff", "-m", "merge b1", "b1")
	src.write("proj/three.txt", "three")
	src.commit("mainline three")

	src.git("checkout", "-q", "-b", "b2", "HEAD~2")
	src.write("proj/four.txt", "four")
	src.commit("branch two")
	src.git("checkout", "-q", testBranch)
	src.git("merge", "-q", "--no-ff", "-m", "merge b2", "b2")
	src.write("proj/five.txt", "five")
	src.commit("mainline five")
	src.push()
}

func linearizeSpecs(src, dst *gritRepo) (string, string) {
	return src.bare + ",proj/," + testBranch, dst.bare + ",," + testBranch
}

// gritLinearize runs grit with -linearize; the flag precedes the
// positional specs, unlike the trailing rule arguments other helpers
// append.
func gritLinearize(t *testing.T, bin, srcSpec, dstSpec string) string {
	t.Helper()
	out, err := runGrit(bin, []string{
		"-config=user.name=test,user.email=" + testAuthorEnv,
		"-linearize", "-push", srcSpec, dstSpec,
	})
	if err != nil {
		t.Fatalf("grit -linearize exited %v:\n%s", err, out)
	}
	return out
}

// TestGritLinearizeSyncsTangledHistoryOnce proves -linearize replicates every
// non-merge change exactly once while the remote stays pristine (cache-only).
func TestGritLinearizeSyncsTangledHistoryOnce(t *testing.T) {
	src, dst := setupGritRepos(t)
	buildTangledHistory(t, src)
	srcSpec, dstSpec := linearizeSpecs(src, dst)

	pristine := strings.TrimSpace(src.gitOut("rev-parse", testBranch))

	out := gritLinearize(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "applying") {
		t.Fatalf("linearized run applied nothing:\n%s", out)
	}
	dst.pull()
	for name, want := range map[string]string{
		"base.txt": "base", "one.txt": "one", "two.txt": "two",
		"three.txt": "three", "four.txt": "four", "five.txt": "five",
	} {
		if got := dstRead(t, dst.dir, name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	// Every non-merge change arrives exactly once: cutting parents turns
	// each merge into a single-parent commit whose delta is applied once.
	// --full-history defeats path simplification so congruent duplicate
	// commits cannot hide behind an unchanged final state.
	for _, name := range []string{"base.txt", "one.txt", "two.txt",
		"three.txt", "four.txt", "five.txt"} {
		history := strings.TrimSpace(dst.gitOut("log", "--format=%H", "--full-history", "--", name))
		if n := len(strings.Split(history, "\n")); n != 1 {
			t.Fatalf("%s was touched by %d destination commits, want exactly one:\n%s", name, n, history)
		}
	}
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
	if got := dst.shipitCount(); got != 6 {
		t.Fatalf("destination carries %d tagged commits, want 6 (one per content commit)", got)
	}

	if got := strings.TrimSpace(src.gitOut("rev-parse", testBranch)); got != pristine {
		t.Fatalf("-linearize mutated the source remote: %s -> %s", pristine, got)
	}

	// A second consecutive run against the same untransformed remote
	// reaches the same fixed point: nothing about the rewrite persisted.
	out = gritLinearize(t, src.gritBin, srcSpec, dstSpec)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("second -linearize run did not reach a fixed point:\n%s", out)
	}
	if got := strings.TrimSpace(src.gitOut("rev-parse", testBranch)); got != pristine {
		t.Fatalf("second run mutated the source remote: %s -> %s", pristine, got)
	}
	dst.pull()
	compareDirs(t, filepath.Join(src.dir, "proj"), dst.dir)
}

// TestLinearizeKeepsFirstParentOnly pins the parent-filter contract:
// after filter-branch with grit's parent filter, every rewritten commit
// has exactly one parent. The filter keeps fields 2-3 ("-p <first-sha>")
// of the filter-branch parent string, which arrives with a leading space
// (verified live on git 2.56.0). The pre-existing end-to-end linearize
// test covers grit.Linearize itself; this pins the shell contract it
// depends on.
func TestLinearizeKeepsFirstParentOnly(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "master")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "base")
	run("checkout", "-qb", "side")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "side")
	run("checkout", "-q", "master")
	run("merge", "-q", "--no-ff", "-m", "merge side", "side")
	merges := func() string {
		t.Helper()
		out, err := exec.Command("git", "-C", dir, "rev-list", "--merges", "--count", "master").CombinedOutput()
		if err != nil {
			t.Fatalf("rev-list merges: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if merges() == "0" {
		t.Fatal("fixture has no merges to linearize")
	}
	// Same filter grit's Linearize applies.
	if out, err := exec.Command("git", "-C", dir, "filter-branch", "-f", "--parent-filter", `cut -f 2,3 -d " "`, "master").CombinedOutput(); err != nil {
		t.Fatalf("filter-branch: %v\n%s", err, out)
	}
	if got := merges(); got != "0" {
		t.Fatalf("linearized history still has %s merges", got)
	}
	out, err := exec.Command("git", "-C", dir, "rev-list", "--parents", "master").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-list parents: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if fields := strings.Fields(line); len(fields) > 2 {
			t.Fatalf("commit %s has %d parents after Linearize, want at most one", fields[0][:7], len(fields)-1)
		}
	}
}

// TestGritLinearizeFailureAbortsLoudly proves a failing filter-branch
// aborts the whole run before any destination work happens.
func TestGritLinearizeFailureAbortsLoudly(t *testing.T) {
	src, dst := setupGritRepos(t)
	buildTangledHistory(t, src)
	srcSpec, dstSpec := linearizeSpecs(src, dst)

	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "cut"),
		[]byte("#!/bin/sh\nexit 42\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	before := strings.TrimSpace(dst.gitOut("rev-parse", testBranch))
	cmd := exec.Command(src.gritBin,
		"-config=user.name=test,user.email="+testAuthorEnv,
		"-push", "-linearize", srcSpec, dstSpec)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("-linearize survived a failing parent-filter:\n%s", out)
	}
	if !strings.Contains(string(out), "linearize") {
		t.Fatalf("failure output does not identify linearization:\n%s", out)
	}

	if got := strings.TrimSpace(dst.gitOut("rev-parse", testBranch)); got != before {
		t.Fatalf("destination advanced during a failed linearize: %s -> %s", before, got)
	}
	dst.pull()
	if got := dst.shipitCount(); got != 0 {
		t.Fatalf("half-synced state found: %d tagged commits after failed linearize", got)
	}
}

// TestLinearizeHelpDocumentsCacheScopedRewrite pins that the flag's
// help text describes the cache-scoped rewrite the tests observe.
func TestLinearizeHelpDocumentsCacheScopedRewrite(t *testing.T) {
	src, _ := setupGritRepos(t)
	cmd := exec.Command(src.gritBin, "-h")
	out, _ := cmd.CombinedOutput() // usage exits 2 by design
	help := string(out)
	if !strings.Contains(help, "-linearize") ||
		!strings.Contains(help, "left untouched") {
		t.Fatalf("-linearize help does not document the cache-scoped rewrite:\n%s", help)
	}
}
