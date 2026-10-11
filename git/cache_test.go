package git

import (
	"context"
	"os"
	"os/exec"
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
	if _, err := os.Stat(legacy + ".lock"); !os.IsNotExist(err) {
		t.Errorf("swept lockless entry left its guard lock behind: %v", err)
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

// TestSweepPreservesAgedEntryWithUnpushedCommit pins the sweep's
// preservation of a destination-shaped entry whose unpushed tail is
// entirely grit-authored: a continued session's resolved work carries the
// shipit tag, so it must survive the sweep regardless of age.
func TestSweepPreservesAgedEntryWithUnpushedCommit(t *testing.T) {
	root := setCacheRoot(t)
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init -q --bare origin.git
		git clone -q origin.git seed
		cd seed
		git config user.email you@example.com
		git config user.name "your name"
		echo base > base.txt
		git add .
		git commit -qm base
		git push -q origin master
	`)
	entry := filepath.Join(root, "unpushedentry")
	if err := os.MkdirAll(root, 0777); err != nil {
		t.Fatal(err)
	}
	shell(t, dir, `git clone -q origin.git unpushedentry`)
	entryWork := filepath.Join(dir, "unpushedentry")
	shell(t, entryWork, `
		git config user.email you@example.com
		git config user.name "your name"
		echo local > local.txt
		git add .
		git commit -qm "unpushed resolved work" -m "fbshipit-source-id: 0123456789abcdef0123456789abcdef01234567"
	`)
	if out, err := exec.Command("mv", entryWork, entry).CombinedOutput(); err != nil {
		t.Fatalf("mv entry: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(entry, heartbeatName), []byte("t"), 0644); err != nil {
		t.Fatal(err)
	}
	ageEntry(t, entry, 400*24*time.Hour)

	SweepCache()

	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("entry with unpushed grit-authored commits was swept: %v", err)
	}
}

// TestSweepPreservesDivergedEntryWithUnpushedCommit pins the sweep's
// preservation of a destination-shaped entry whose unpushed tail diverged
// from the remote tip (a third-party push moved it) but is entirely
// grit-authored: the resolved session's work must survive regardless of
// age.
func TestSweepPreservesDivergedEntryWithUnpushedCommit(t *testing.T) {
	root := setCacheRoot(t)
	if err := os.MkdirAll(root, 0777); err != nil {
		t.Fatal(err)
	}
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init -q --bare origin.git
		git clone -q origin.git work
		cd work
		git config user.email you@example.com
		git config user.name "your name"
		echo base > base.txt
		git add .
		git commit -qm base
		git push -q origin master
		echo local > local.txt
		git add .
		git commit -qm "unpushed resolved work" -m "fbshipit-source-id: 0123456789abcdef0123456789abcdef01234567"
		cd ..
		git clone -q origin.git other
		cd other
		git config user.email you@example.com
		git config user.name "your name"
		echo remote > remote.txt
		git add .
		git commit -qm "independent remote advance"
		git push -q origin master
		cd ../work
		git fetch -q origin
	`)
	entry := filepath.Join(root, "divergedentry")
	if out, err := exec.Command("mv", filepath.Join(dir, "work"), entry).CombinedOutput(); err != nil {
		t.Fatalf("mv entry: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(entry, heartbeatName), []byte("t"), 0644); err != nil {
		t.Fatal(err)
	}
	ageEntry(t, entry, 400*24*time.Hour)

	SweepCache()

	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("diverged entry with unpushed grit-authored commits was swept: %v", err)
	}
}

func TestSweepRemovesBehindEntry(t *testing.T) {
	root := setCacheRoot(t)
	if err := os.MkdirAll(root, 0777); err != nil {
		t.Fatal(err)
	}
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init -q --bare origin.git
		git clone -q origin.git work
		cd work
		git config user.email you@example.com
		git config user.name "your name"
		echo base > base.txt
		git add .
		git commit -qm base
		git push -q origin master
		cd ..
		git clone -q origin.git other
		cd other
		git config user.email you@example.com
		git config user.name "your name"
		echo remote > remote.txt
		git add .
		git commit -qm "independent remote advance"
		git push -q origin master
		cd ../work
		git fetch -q origin
	`)
	entry := filepath.Join(root, "behindentry")
	if out, err := exec.Command("mv", filepath.Join(dir, "work"), entry).CombinedOutput(); err != nil {
		t.Fatalf("mv entry: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(entry, heartbeatName), []byte("t"), 0644); err != nil {
		t.Fatal(err)
	}
	ageEntry(t, entry, 400*24*time.Hour)

	SweepCache()

	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatalf("behind entry with nothing unpushed survived the sweep: %v", err)
	}
}

// TestSweepRemovesLinearizedSourceEntry pins the -linearize sweep leak fix:
// Linearize rewrites the source clone's history with filter-branch, so the
// entry diverges from its remote tip permanently, and the rewritten commits
// are the source's own, carrying no grit authorship markers. A preserve
// predicate keyed on divergence alone would keep a linearized source clone
// in the cache forever; the authorship gate must let it age out by TTL.
func TestSweepRemovesLinearizedSourceEntry(t *testing.T) {
	root := setCacheRoot(t)
	if err := os.MkdirAll(root, 0777); err != nil {
		t.Fatal(err)
	}
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init -q --bare origin.git
		git clone -q origin.git work
		cd work
		git config user.email you@example.com
		git config user.name "your name"
		echo base > base.txt
		git add .
		git commit -qm base
		git push -q origin master
		git checkout -qb side
		echo side > side.txt
		git add .
		git commit -qm side
		git checkout -q master
		git merge -q --no-ff -m "merge side" side
		git push -q origin master
	`)
	entry := filepath.Join(root, "linearizedentry")
	if out, err := exec.Command("mv", filepath.Join(dir, "work"), entry).CombinedOutput(); err != nil {
		t.Fatalf("mv entry: %v\n%s", err, out)
	}
	// The same filter grit's Linearize applies: rewriting the merge
	// leaves the local history permanently diverged from the remote tip.
	shell(t, entry, `git filter-branch -f --parent-filter 'cut -f 2,3 -d " "'`)
	if out, err := exec.Command("git", "-C", entry, "merge-base", "--is-ancestor", "HEAD", "origin/master").CombinedOutput(); err == nil {
		t.Fatalf("fixture must diverge from the remote tip after linearize, but HEAD is an ancestor: %s", out)
	}
	if err := os.WriteFile(filepath.Join(entry, heartbeatName), []byte("t"), 0644); err != nil {
		t.Fatal(err)
	}
	ageEntry(t, entry, 400*24*time.Hour)

	SweepCache()

	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatalf("linearized source entry survived the sweep: %v", err)
	}
}

// TestSweepRemovesDivergedEntryWithPartiallyForeignTail pins the
// every-commit authorship gate: a diverged tail mixing grit-authored
// commits with foreign ones is not preservable (open discards such a
// lineage via PreservedTailIsGritAuthored), so the sweep must not keep it
// forever either.
func TestSweepRemovesDivergedEntryWithPartiallyForeignTail(t *testing.T) {
	root := setCacheRoot(t)
	if err := os.MkdirAll(root, 0777); err != nil {
		t.Fatal(err)
	}
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init -q --bare origin.git
		git clone -q origin.git work
		cd work
		git config user.email you@example.com
		git config user.name "your name"
		echo base > base.txt
		git add .
		git commit -qm base
		git push -q origin master
		echo resolved > resolved.txt
		git add .
		git commit -qm "resolved session work" -m "fbshipit-source-id: 0123456789abcdef0123456789abcdef01234567"
		echo foreign > foreign.txt
		git add .
		git commit -qm "a foreign commit without grit markers"
		cd ..
		git clone -q origin.git other
		cd other
		git config user.email you@example.com
		git config user.name "your name"
		echo remote > remote.txt
		git add .
		git commit -qm "independent remote advance"
		git push -q origin master
		cd ../work
		git fetch -q origin
	`)
	entry := filepath.Join(root, "mixedtailentry")
	if out, err := exec.Command("mv", filepath.Join(dir, "work"), entry).CombinedOutput(); err != nil {
		t.Fatalf("mv entry: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(entry, heartbeatName), []byte("t"), 0644); err != nil {
		t.Fatal(err)
	}
	ageEntry(t, entry, 400*24*time.Hour)

	SweepCache()

	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatalf("diverged entry with a partially foreign tail survived the sweep: %v", err)
	}
}

// TestSweepPreservesEntryWithNonCommitRemoteTip pins the tip-verification
// arm of the authorship gate: a remote-tracking ref pointing at a
// non-commit object (possible only through manual ref surgery) fails the
// ancestor check, and git log would silently drop its exclusion —
// overbroadening the tail to the entry's full history, whose unmarked
// base commits would wrongly evict preservable work. The sweep must
// treat the unpeelable tip as inconclusive and preserve.
func TestSweepPreservesEntryWithNonCommitRemoteTip(t *testing.T) {
	root := setCacheRoot(t)
	if err := os.MkdirAll(root, 0777); err != nil {
		t.Fatal(err)
	}
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init -q --bare origin.git
		git clone -q origin.git work
		cd work
		git config user.email you@example.com
		git config user.name "your name"
		echo base > base.txt
		git add .
		git commit -qm base
		git push -q origin master
		echo resolved > resolved.txt
		git add .
		git commit -qm "unpushed resolved work" -m "fbshipit-source-id: 0123456789abcdef0123456789abcdef01234567"
		blob=$(git hash-object base.txt)
		git update-ref refs/remotes/origin/master "$blob"
	`)
	entry := filepath.Join(root, "blobtipentry")
	if out, err := exec.Command("mv", filepath.Join(dir, "work"), entry).CombinedOutput(); err != nil {
		t.Fatalf("mv entry: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(entry, heartbeatName), []byte("t"), 0644); err != nil {
		t.Fatal(err)
	}
	ageEntry(t, entry, 400*24*time.Hour)

	SweepCache()

	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("entry with a non-commit remote tip was swept: %v", err)
	}
}

// TestSweepPreservesCheckoutWithoutRemoteRefs pins the preservation
// bias: a checkout holding commits but zero remote-tracking refs is
// inconclusive (its tips cannot be read), so it must survive the sweep
// rather than fall through to eviction. Reproduces the pre-fix hole where
// an empty for-each-ref listing skipped the loop and returned false.
func TestSweepPreservesCheckoutWithoutRemoteRefs(t *testing.T) {
	root := setCacheRoot(t)
	if err := os.MkdirAll(root, 0777); err != nil {
		t.Fatal(err)
	}
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init -q --bare origin.git
		git clone -q origin.git work
		cd work
		git config user.email you@example.com
		git config user.name "your name"
		echo base > base.txt
		git add .
		git commit -qm base
		git push -q origin master
		echo local > local.txt
		git add .
		git commit -qm "unpushed resolved work"
		git remote remove origin
	`)
	entry := filepath.Join(root, "noremoterefs")
	if out, err := exec.Command("mv", filepath.Join(dir, "work"), entry).CombinedOutput(); err != nil {
		t.Fatalf("mv entry: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", entry, "for-each-ref", "--format=%(objectname)", "refs/remotes/").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("fixture must have zero remote-tracking refs, got %q, %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(entry, heartbeatName), []byte("t"), 0644); err != nil {
		t.Fatal(err)
	}
	ageEntry(t, entry, 400*24*time.Hour)

	SweepCache()

	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("checkout without remote refs was swept: %v", err)
	}
}
