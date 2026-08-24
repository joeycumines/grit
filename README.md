![](https://github.com/grailbio/grit/workflows/CI/badge.svg)

Grit copies commits from a source repository to a destination
repository. It is intended to mirror projects residing in an
private monorepo to an external project-specific Git repository.
Merge commits are replicated too, including hand-resolved conflict
content and "-s ours" discards; see the package documentation's
"Merge commits" section for the exact semantics.

Clone caches live under the OS temporary directory by default and are
bounded automatically: every grit run sweeps entries unused for 14
days (336 hours). Set `GRIT_CACHE_DIR` (an absolute path) to relocate
caches to durable storage, and `GRIT_CACHE_TTL` (a Go duration) to
change or disable (`0`) the sweep window. Clone entries holding a
paused conflict-resolution session are never swept; other idle state
is discarded after the window, so point `GRIT_CACHE_DIR` at durable
storage before pausing work for longer than that.

## Concurrency

Run one grit process per source/destination pair. On a single host,
a per-endpoint flock in the clone cache serializes concurrent runs
against the same destination, except in the narrow window where the
cache sweep removes a stale entry just as another run reopens it; a
run that hits that window fails loudly and a rerun recovers. Across hosts (no shared filesystem),
concurrent pushes to the same destination branch race: the loser's
push is rejected as non-fast-forward and its local state is discarded,
so simply re-running the losing grit recomputes against the updated
destination.

Usage:

	$ go get [-u] github.com/grailbio/grit
	$ grit [-push] [-dump] src dst rules...

[Documentation](https://godoc.org/github.com/grailbio/grit).
