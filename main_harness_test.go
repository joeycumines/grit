// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main_test

import (
	"crypto/sha256"
	"fmt"
	gritgit "github.com/grailbio/grit/git"
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
func gritOutput(t *testing.T, bin, srcSpec, dstSpec string, extra ...string) string {
	t.Helper()
	args := []string{
		"-config=user.name=test,user.email=" + testAuthorEnv,
		"-push", srcSpec, dstSpec,
	}
	args = append(args, extra...)
	cmd := exec.Command(bin, args...)
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

// TestGritCloneDirMatchesCachePath pins the test helper's clone-path
// derivation against grit's own: drift between them would silently
// mislocate paused sessions in every session-related test.
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
