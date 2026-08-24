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

	gritgit "github.com/grailbio/grit/git"
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
func gritOutput(t *testing.T, bin, srcSpec, dstSpec string, extra ...string) string {
	t.Helper()
	args := []string{
		"-config=user.name=test,user.email=" + testAuthorEnv,
		"-push", srcSpec, dstSpec,
	}
	args = append(args, extra...)
	out, err := runGrit(bin, args)
	if err != nil {
		t.Logf("grit exited non-zero (expected when pausing):\n%s", out)
	}
	return out
}

// gritOutputStrict behaves like gritOutput but fails the test on any
// non-zero grit exit: for call sites where a paused conflict session
// is not an expected outcome.
func gritOutputStrict(t *testing.T, bin, srcSpec, dstSpec string, extra ...string) string {
	t.Helper()
	args := []string{
		"-config=user.name=test,user.email=" + testAuthorEnv,
		"-push", srcSpec, dstSpec,
	}
	args = append(args, extra...)
	out, err := runGrit(bin, args)
	if err != nil {
		t.Fatalf("grit %v exited %v:\n%s", args, err, out)
	}
	return out
}

func runGrit(bin string, args []string) (string, error) {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestGritOutputStrictRejectsFailure runs a child go-test whose canary
// feeds the strict harness a non-zero-exiting binary; it must fail.
func TestGritOutputStrictRejectsFailure(t *testing.T) {
	if os.Getenv("GRIT_STRICT_CANARY") != "" {
		bin := filepath.Join(t.TempDir(), "corrupt-grit")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho boom >&2\nexit 7\n"), 0755); err != nil {
			t.Fatal(err)
		}
		gritOutputStrict(t, bin, "src", "dst")
		t.Log("strict harness unexpectedly tolerated the corruption")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestGritStrictCanary", "-test.count=1")
	cmd.Env = append(os.Environ(), "GRIT_STRICT_CANARY=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "exited") {
		t.Fatalf("strict harness did not fail the corrupted invocation (err=%v):\n%s", err, out)
	}
}

// TestGritStrictCanary exists solely for
// TestGritOutputStrictRejectsFailure's child process.
func TestGritStrictCanary(t *testing.T) {
	if os.Getenv("GRIT_STRICT_CANARY") == "" {
		t.Skip("canary for TestGritOutputStrictRejectsFailure")
	}
	bin := filepath.Join(t.TempDir(), "corrupt-grit")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho boom >&2\nexit 7\n"), 0755); err != nil {
		t.Fatal(err)
	}
	gritOutputStrict(t, bin, "src", "dst")
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

// TestGritCloneDirMatchesCachePath pins the helper's clone-path derivation
// against grit's own so session tests cannot silently mislocate clones.
func TestGritCloneDirMatchesCachePath(t *testing.T) {
	t.Setenv("TEST_TMPDIR", t.TempDir())
	const url = "https://example.com/mirror.git"
	for _, tc := range []struct{ prefix, branch string }{
		{"", "main"},
		{"proj/", "main"},
		{"proj/", "feature"},
	} {
		got := filepath.Base(gritCloneDir(t, url, tc.prefix, tc.branch))
		want := filepath.Base(gritgit.CachePath(url, tc.prefix, tc.branch))
		if got != want {
			t.Errorf("gritCloneDir(%q,%q) base = %s, want git.CachePath base = %s", tc.prefix, tc.branch, got, want)
		}
	}
}
