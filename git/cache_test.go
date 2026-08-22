package git

import (
	"context"
	"github.com/grailbio/base/flock"
	"path/filepath"
	"testing"
	"time"
)

func TestCachePathDiffersFromLegacy(t *testing.T) {
	const url = "https://example.com/repo.git"
	if CachePath(url, "", "main") == LegacyCachePath(url) {
		t.Fatal("current and legacy cache paths coincide")
	}
	// Distinct endpoints sharing a URL must not share a cache.
	if CachePath(url, "pfx/", "main") == CachePath(url, "", "main") {
		t.Fatal("prefix is not part of the cache key")
	}
	if CachePath(url, "", "main") == CachePath(url, "", "feature") {
		t.Fatal("branch is not part of the cache key")
	}
}

// TestOpenReleasesLockOnFailure verifies that a failed open releases the
// repository's flock: acquiring it directly afterwards must succeed
// immediately rather than block on the leaked handle.
func TestOpenReleasesLockOnFailure(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		out, err := execGit(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "--bare", "-b", "main", filepath.Join(dir, "repo.git"))
	url := filepath.Join(dir, "repo.git")

	if _, err := open(url, "", "missing-branch", false); err == nil {
		t.Fatal("open with a missing branch unexpectedly succeeded")
	}

	lock := flock.New(CachePath(url, "", "missing-branch") + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := lock.Lock(ctx); err != nil {
		t.Fatalf("lock was not released after failed open: %v", err)
	}
	lock.Unlock()
}

func execGit(dir string, args ...string) (string, error) {
	cmd := command(dir, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
