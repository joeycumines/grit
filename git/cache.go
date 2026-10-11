package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0644)
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

// holdsUnpushedWork reports whether the entry is a git checkout whose HEAD
// holds unpushed work grit itself authored: commits no remote-tracking tip
// contains, every one carrying grit's serialization (a shipit source id or
// a convergence-pruned marker). That is a continued session whose commits
// have not been pushed yet; sweeping it would destroy the only copy of
// manually resolved work that OpenDestination exists to preserve across
// runs, so it survives like a paused session. Divergence alone does not
// qualify: a source clone rewritten by -linearize (or any foreign unpushed
// state) diverges from its remote tips permanently, and the next open
// discards such state either way — source clones hard-reset to the remote
// tip, and OpenDestination discards tails without grit authorship markers —
// so the sweep applies the same marker gate and lets plain divergence age
// out by TTL. Non-checkouts, checkouts without commits, and checkouts at or
// behind their remote tips report false. Errors bias toward preservation:
// eviction requires a clean reading with HEAD an ancestor-or-equal of every
// tip, or a diverged tail that parses cleanly and holds a commit without
// grit markers.
func holdsUnpushedWork(base string) bool {
	gitDir := filepath.Join(base, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return false
	}
	head, err := execGitDir(base, "rev-parse", "HEAD")
	if err != nil || head == "" {
		// No HEAD commit: nothing unpushed to protect.
		return false
	}
	refs, err := execGitDir(base, "for-each-ref", "--format=%(objectname)", "refs/remotes/")
	if err != nil {
		// Tips unreadable: inconclusive, preserve.
		return true
	}
	sawTip := false
	diverged := false
	var tips []string
	for _, tip := range strings.Split(refs, "\n") {
		tip = strings.TrimSpace(tip)
		if tip == "" {
			continue
		}
		sawTip = true
		tips = append(tips, tip)
		// Eviction requires HEAD an ancestor-or-equal of every tip,
		// so any tip failing that check means unpushed work, while
		// behind and at-tip tips do not. Any failure to answer
		// (missing object, transient error) counts as divergence
		// here; the tail reading below errs back toward preservation
		// whenever it cannot produce a clean answer.
		if _, err := execGitDir(base, "merge-base", "--is-ancestor", head, tip); err != nil {
			diverged = true
		}
	}
	if !sawTip {
		// Zero remote-tracking refs: nothing proves HEAD is pushed, so
		// an inconclusive reading preserves rather than evicts.
		return true
	}
	if !diverged {
		return false
	}
	// A remote-tracking ref that does not peel to a commit (possible
	// only through manual ref surgery; git itself always writes
	// commit-pointing remote-tracking refs) fails the ancestor check
	// above while git log silently drops its "^tip" exclusion,
	// overbroadening the tail to history the entry shares with its
	// remote and risking eviction of preservable work. Verify every
	// tip peels to a commit first; any failure is inconclusive and
	// preserves.
	verify := make([]string, 0, len(tips))
	for _, tip := range tips {
		verify = append(verify, tip+"^{commit}")
	}
	if _, err := execGitDir(base, append([]string{"rev-parse"}, verify...)...); err != nil {
		return true
	}
	// Some tip holds commits HEAD does not descend from. Judge the
	// unpushed tail — commits reachable from HEAD but from no tip —
	// by the same authorship gate open applies, so only work grit
	// itself serialized survives. The format flags pin the output
	// against repository configuration exactly like Log.
	args := []string{"log", "--pretty=medium", "--no-decorate", head}
	for _, tip := range tips {
		args = append(args, "^"+tip)
	}
	out, err := execGitDir(base, args...)
	if err != nil {
		// Tail unreadable: inconclusive, preserve.
		return true
	}
	authored, err := unpushedTailIsGritAuthored(out)
	if err != nil {
		// Tail unparseable: inconclusive, preserve.
		return true
	}
	return authored
}

// unpushedTailIsGritAuthored reports whether every commit in the provided
// medium-format git log output carries grit's authorship markers — the
// per-commit gate PreservedTailIsGritAuthored applies at open. Empty
// output (no unpushed commits) reports true, preserving the entry: a tip
// can fail the ancestor check with an empty tail only on multi-ref
// entries whose other tips already hold all of HEAD's commits, and the
// every-tip eviction contract settles that case toward preservation.
// parseCommits never dereferences its Repo argument, and only the
// Body-level marker methods run on the parsed commits, so the nil repo
// is safe here.
func unpushedTailIsGritAuthored(out string) (bool, error) {
	commits, err := parseCommits(nil, []byte(out))
	if err != nil {
		return false, err
	}
	for _, c := range commits {
		if len(c.OwnLineGritTagIDs()) == 0 && len(c.OwnLineConvergenceMarkers()) == 0 {
			return false, nil
		}
	}
	return true, nil
}

// execGitDir runs git in dir and returns trimmed stdout. Stderr is
// discarded: callers treat any failure as inconclusive, never as proof of
// absence.
func execGitDir(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_LFS_SKIP_SMUDGE=1")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// SweepCache removes clone cache entries that no recent run has used,
// bounding cache growth without operator action. It runs before any
// repository opens in a process, so it never races its own entries:
// every eviction path first acquires the entry's lock without waiting
// (contended entries are skipped), and only then is last use judged
// from the entry's heartbeat or directory mtime under that lock, so a
// concurrent open either wins the lock (entry skipped) or loses loudly
// with rerun recovery. Entries carrying a paused git am session,
// grit-authored unpushed work, or unpushed state the sweep cannot
// conclusively read survive regardless of age. Entries
// without a companion lock file predate per-endpoint keying; the sweep
// creates and holds a guard lock before judging them, so they age out by
// directory mtime alone without racing a concurrent bootstrap. Setting GRIT_CACHE_TTL
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
			if fi, err := os.Stat(base); err == nil {
				return fi.ModTime()
			}
			return now
		}
		// Freshness is judged once, under the entry lock: acquiring the
		// lock first (creating the guard file for legacy entries that
		// predate per-endpoint keying) closes the stat-to-remove race
		// with a concurrent bootstrap, and the single post-lock reading
		// honors a heartbeat refreshed right up to acquisition.
		release, ok := tryLockFile(lockPath)
		if !ok {
			continue
		}
		// Recompute under the lock: the entry may have been touched
		// between the directory listing and the acquisition.
		age := now.Sub(lastUsed())
		if age > ttl && !holdsPausedSession(base) && !holdsUnpushedWork(base) {
			log.Printf("sweeping clone cache %s unused for %s", base, age.Round(24*time.Hour))
			if err := os.RemoveAll(base); err != nil {
				log.Printf("sweeping clone cache %s failed: %v; leaving entry for the next cycle", base, err)
				release()
				continue
			}
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
			if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
				log.Printf("sweeping clone cache %s: removing lock file failed: %v", base, err)
			}
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
