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
