package git

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/grailbio/base/log"
)

// heartbeatName is the per-entry last-used stamp written while an
// entry's lock is held. Directory mtimes are unreliable last-used
// signals (reads do not touch them), so every open refreshes this file
// instead; entries lacking one fall back to their directory mtime.
const heartbeatName = ".grit-heartbeat"

// historicalDefaultRoot is the clone cache root that grit versions
// before the portable default used. Its contents (including manually
// tended clones) are invisible to the moved root, so its existence is
// announced rather than silently orphaned. A var so tests can point it
// at a fixture.
var historicalDefaultRoot = "/var/tmp/grit"

// defaultCacheTTL bounds how long an unused clone cache entry survives
// without a run touching it.
const defaultCacheTTL = 336 * time.Hour

var announcedLegacy sync.Map

func resetAnnouncements() {
	announcedLegacy = sync.Map{}
}

// announceLegacy prints a stale-cache notice for the provided path at
// most once per process: repeated encounters with the same abandoned
// location would otherwise spam identical warnings.
func announceLegacy(path string) bool {
	if _, loaded := announcedLegacy.LoadOrStore(path, true); loaded {
		return false
	}
	return true
}

func resolveDir() string {
	if t := os.Getenv("TEST_TMPDIR"); t != "" {
		return filepath.Join(t, "grit")
	}
	if g := os.Getenv("GRIT_CACHE_DIR"); g != "" {
		return g
	}
	return filepath.Join(os.TempDir(), "grit")
}

// tryLockFile takes an exclusive BSD flock on path without waiting,
// returning a release function when the lock was acquired. Waiting is
// not an option here: the grailbio/base/flock wrapper leaves a ghost
// goroutine behind whenever its context expires, and once the current
// holder releases the lock that goroutine acquires it and then blocks
// forever on its internal result channel, holding the lock permanently.
func tryLockFile(path string) (func(), bool) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR, 0644)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		return nil, false
	}
	return func() {
		syscall.Flock(fd, syscall.LOCK_UN)
		syscall.Close(fd)
	}, true
}

// holdsPausedSession reports whether the entry carries a paused git am
// session. Such an entry holds conflict resolutions a human has not
// finished; its idle age says nothing about its value, so sweeps must
// never judge it stale.
func holdsPausedSession(base string) bool {
	_, err := os.Stat(filepath.Join(base, ".git", "rebase-apply"))
	return err == nil
}

// SweepCache removes clone cache entries that no recent run has used,
// bounding cache growth without operator action. It runs before any
// repository opens in a process, so it never races its own entries:
// entries whose lock another process holds are skipped through a
// non-blocking acquisition attempt, and only then is last use judged
// from the entry's heartbeat or directory mtime. Entries carrying a
// paused git am session survive regardless of age. Entries without a
// companion lock file predate per-endpoint keying; nothing locks them,
// so they age out by directory mtime alone. Setting GRIT_CACHE_TTL
// overrides the default window; values less than or equal to zero
// disable sweeping entirely, and unparseable values keep the default
// with a warning.
func SweepCache() {
	root := Dir
	if root != historicalDefaultRoot {
		if _, err := os.Stat(historicalDefaultRoot); err == nil && announceLegacy(historicalDefaultRoot) {
			log.Printf("note: historical clone cache %s is no longer used; inspect it for paused or manually tended sessions before deleting", historicalDefaultRoot)
		}
	}
	ttl := defaultCacheTTL
	if v := os.Getenv("GRIT_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			log.Printf("warning: ignoring unparseable GRIT_CACHE_TTL %q: %v", v, err)
		case d <= 0:
			return
		default:
			ttl = d
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		base := filepath.Join(root, e.Name())
		lockPath := base + ".lock"
		lastUsed := func() time.Time {
			if fi, err := os.Stat(filepath.Join(base, heartbeatName)); err == nil {
				return fi.ModTime()
			}
			if fi, err := e.Info(); err == nil {
				return fi.ModTime()
			}
			return now
		}
		stale := now.Sub(lastUsed()) > ttl && !holdsPausedSession(base)
		if _, err := os.Stat(lockPath); os.IsNotExist(err) {
			if stale {
				log.Printf("sweeping clone cache %s unused for %s", base, now.Sub(lastUsed()).Round(24*time.Hour))
				os.RemoveAll(base)
			}
			continue
		}
		release, ok := tryLockFile(lockPath)
		if !ok {
			continue
		}
		if stale {
			log.Printf("sweeping clone cache %s unused for %s", base, now.Sub(lastUsed()).Round(24*time.Hour))
			os.RemoveAll(base)
			// Unlinking the lock file while holding its flock leaves one
			// narrow hole: an opener blocked on this very file acquires
			// the lock on the unlinked inode after release, so it no
			// longer excludes a later opener that recreates the file.
			// Only a concurrent reopen of a stale entry can land there;
			// such runs fail loudly (the clone collides) and a rerun
			// recovers, which is why the guarantee is documented as
			// holding except for that window. Closing it at the acquirer
			// side would need the lock fd to validate its own inode, and
			// the flock wrapper keeps that fd private.
			os.Remove(lockPath)
		}
		release()
	}
}

// touchHeartbeat records a use of the provided cache entry while its
// lock is held, creating the stamp if absent.
func touchHeartbeat(entry string) {
	p := filepath.Join(entry, heartbeatName)
	now := time.Now()
	if _, err := os.Stat(p); err == nil {
		os.Chtimes(p, now, now)
		return
	}
	f, err := os.Create(p)
	if err == nil {
		f.Close()
		os.Chtimes(p, now, now)
	}
}

// Portability: grit does not build for GOOS=windows because POSIX
// flock locking is unavailable there. Both this file's tryLockFile and
// the grailbio/base/flock dependency used for repository locks call
// syscall.Flock directly; cache management otherwise uses only
// filepath and os primitives.
