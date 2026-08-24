package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grailbio/base/flock"
	"github.com/grailbio/testutil"
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

// setCacheRoot points the package cache root at a per-test directory,
// restoring the previous value afterwards. Sweep tests must not share
// state or run in parallel.
func setCacheRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "grit")
	old := Dir
	Dir = root
	t.Cleanup(func() { Dir = old })
	return root
}

// ageEntry backdates a cache entry directory and, when it has one, its
// heartbeat file, so that sweep decisions observe the requested age.
func ageEntry(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	target := time.Now().Add(-age)
	if err := os.Chtimes(dir, target, target); err != nil {
		t.Fatal(err)
	}
	hb := filepath.Join(dir, heartbeatName)
	if _, err := os.Stat(hb); err == nil {
		if err := os.Chtimes(hb, target, target); err != nil {
			t.Fatal(err)
		}
	}
}

// mkEntry creates a cache entry directory with an optional heartbeat
// file.
func mkEntry(t *testing.T, root, name string, withHeartbeat bool) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if withHeartbeat {
		if err := os.WriteFile(filepath.Join(dir, heartbeatName), []byte("t"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestResolveCacheDirPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		testTmp    string
		cacheDir   string
		wantSuffix string
	}{
		{"test tmp wins", "/tmp/tt", "/tmp/gritdir", filepath.Join("/tmp/tt", "grit")},
		{"cache dir second", "", "/opt/persistent", "/opt/persistent"},
		{"temp dir default", "", "", filepath.Join(os.TempDir(), "grit")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_TMPDIR", tc.testTmp)
			t.Setenv("GRIT_CACHE_DIR", tc.cacheDir)
			if got := resolveDir(); got != tc.wantSuffix {
				t.Errorf("resolveDir() = %q, want %q", got, tc.wantSuffix)
			}
		})
	}
}

func TestSweepRemovesStaleKeepsFresh(t *testing.T) {
	root := setCacheRoot(t)
	stale := mkEntry(t, root, "stale0001", true)
	fresh := mkEntry(t, root, "fresh0002", true)
	ageEntry(t, stale, 400*24*time.Hour)
	ageEntry(t, fresh, time.Hour)
	lockPath := stale + ".lock"
	if err := os.WriteFile(lockPath, nil, 0644); err != nil {
		t.Fatal(err)
	}

	SweepCache()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale entry survived the sweep: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("swept entry's lock file survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh entry was swept: %v", err)
	}
}

func TestSweepSkipsLockHeldEntry(t *testing.T) {
	root := setCacheRoot(t)
	held := mkEntry(t, root, "held00003", true)
	ageEntry(t, held, 400*24*time.Hour)

	release, ok := tryLockFile(held + ".lock")
	if !ok {
		t.Fatal("setup could not acquire a free entry lock")
	}
	defer release()

	SweepCache()

	if _, err := os.Stat(held); err != nil {
		t.Errorf("entry whose lock was externally held was swept: %v", err)
	}
}

// TestTryLockFileIsNonBlocking pins the sweeper's acquisition primitive:
// a held lock must fail fast rather than wait, and releasing it must
// make the lock available again. A waiting implementation would both
// stall every run behind concurrent operators and, through flock's
// internal goroutine, permanently wedge locks after timeouts.
func TestTryLockFileIsNonBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entry.lock")

	release, ok := tryLockFile(path)
	if !ok {
		t.Fatal("tryLockFile failed on a free lock")
	}
	if _, ok := tryLockFile(path); ok {
		t.Fatal("tryLockFile acquired an already-held lock")
	}
	release()
	if release2, ok := tryLockFile(path); !ok {
		t.Fatal("lock unavailable after release")
	} else {
		release2()
	}
}

// TestSweepSkipsPausedSessions pins the preservation boundary: a cache
// entry holding a paused git am session is manually-resolved work in
// waiting, and must survive sweeps regardless of how long it has been
// idle.
func TestSweepSkipsPausedSessions(t *testing.T) {
	root := setCacheRoot(t)
	paused := mkEntry(t, root, "paused007", true)
	ageEntry(t, paused, 400*24*time.Hour)
	if err := os.MkdirAll(filepath.Join(paused, ".git", "rebase-apply"), 0777); err != nil {
		t.Fatal(err)
	}

	SweepCache()

	if _, err := os.Stat(paused); err != nil {
		t.Errorf("entry with a paused am session was swept: %v", err)
	}
}

// TestAnnounceHistoricalRoot pins the one-time migration notice: when
// the historical default cache root still exists, the first sweep
// announces it so that manually tended sessions there are not silently
// orphaned by the root move.
func TestAnnounceHistoricalRoot(t *testing.T) {
	old := t.TempDir()
	resetAnnouncements()
	if !announceLegacy(old) || announceLegacy(old) {
		t.Fatal("historical-root notice must fire exactly once per path")
	}
}

func TestSweepAgesLegacyDirsByDirMtime(t *testing.T) {
	root := setCacheRoot(t)
	legacy := mkEntry(t, root, "legacy0004", false)
	ageEntry(t, legacy, 400*24*time.Hour)
	recentLegacy := mkEntry(t, root, "recent05", false)
	ageEntry(t, recentLegacy, 24*time.Hour)

	SweepCache()

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("stale legacy directory survived the sweep: %v", err)
	}
	if _, err := os.Stat(recentLegacy); err != nil {
		t.Errorf("recent legacy directory was swept: %v", err)
	}
}

func TestSweepDisabledByNonPositiveTTL(t *testing.T) {
	root := setCacheRoot(t)
	stale := mkEntry(t, root, "stale0005", true)
	legacy := mkEntry(t, root, "legacy06", false)
	ageEntry(t, stale, 400*24*time.Hour)
	ageEntry(t, legacy, 400*24*time.Hour)
	t.Setenv("GRIT_CACHE_TTL", "0")

	SweepCache()

	if _, err := os.Stat(stale); err != nil {
		t.Errorf("TTL=0 must disable sweeping; entry removed: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("TTL=0 must disable sweeping; legacy removed: %v", err)
	}
}

func TestOpenRefreshesHeartbeat(t *testing.T) {
	setCacheRoot(t)
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init -q --bare origin.git
		git clone -q origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"
		echo x > x.txt
		git add .
		git commit -qm one
		git push -q origin master
	`)
	url := filepath.Join(dir, "origin.git")
	before := time.Now().Add(-time.Minute)
	r, err := Open(url, "", "master")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	hb := filepath.Join(r.RepoRoot(), heartbeatName)
	fi, err := os.Stat(hb)
	if err != nil {
		t.Fatalf("open did not leave a heartbeat: %v", err)
	}
	if fi.ModTime().Before(before) {
		t.Errorf("heartbeat mtime %v predates the open %v", fi.ModTime(), before)
	}
}

// TestAnnounceHistoricalRootEvenWhenSweepDisabled pins that the
// migration notice does not depend on sweeping being enabled: an
// operator who disables the janitor still needs to learn that
// /var/tmp/grit holds manually tended sessions.
func TestAnnounceHistoricalRootEvenWhenSweepDisabled(t *testing.T) {
	root := setCacheRoot(t)
	old := filepath.Join(root, "..", "var-tmp-grit")
	if err := os.MkdirAll(old, 0777); err != nil {
		t.Fatal(err)
	}
	// Point the historical constant at the fake through the same
	// mechanism production uses: re-resolve after stubbing the const.
	prev := historicalDefaultRoot
	historicalDefaultRoot = old
	t.Cleanup(func() { historicalDefaultRoot = prev })
	resetAnnouncements()
	t.Setenv("GRIT_CACHE_TTL", "0")

	SweepCache()

	if _, ok := announcedLegacy.Load(old); !ok {
		t.Fatal("historical-root notice was suppressed by GRIT_CACHE_TTL=0")
	}
}

// TestSweepSkipsDirectoryShapedLockFile pins the boundary where an
// entry's companion lock path exists as a directory: opening it for
// flock must fail safely and the entry must be skipped rather than
// deleted or crashed upon.
func TestSweepSkipsDirectoryShapedLockFile(t *testing.T) {
	root := setCacheRoot(t)
	entry := mkEntry(t, root, "dirlocked8", true)
	ageEntry(t, entry, 400*24*time.Hour)
	if err := os.MkdirAll(entry+".lock", 0777); err != nil {
		t.Fatal(err)
	}

	SweepCache()

	if _, err := os.Stat(entry); err != nil {
		t.Errorf("entry behind a directory-shaped lock file was swept: %v", err)
	}
}

// TestSweepKeepsDefaultOnUnparseableTTL pins that a malformed
// GRIT_CACHE_TTL degrades to the default window rather than disabling
// or crashing the sweep.
func TestSweepKeepsDefaultOnUnparseableTTL(t *testing.T) {
	root := setCacheRoot(t)
	stale := mkEntry(t, root, "stale0009", true)
	ageEntry(t, stale, 400*24*time.Hour)
	t.Setenv("GRIT_CACHE_TTL", "not-a-duration")

	SweepCache()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("unparseable TTL did not fall back to the default window: %v", err)
	}
}

func TestAnnounceOncePerProcess(t *testing.T) {
	p := t.TempDir()
	resetAnnouncements()
	if !announceLegacy(p) || announceLegacy(p) {
		t.Fatal("legacy announcement must fire exactly once per path")
	}
	resetAnnouncements()
	if !announceLegacy(p) {
		t.Fatal("announcement must re-arm after a reset")
	}
}
