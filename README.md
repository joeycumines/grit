# Grit

![](https://github.com/grailbio/grit/workflows/CI/badge.svg)

Grit synchronizes Git repository branches, copying commits from a source
repository to a destination repository. It is designed to mirror project
subtrees residing in a monorepo to standalone external repositories.

Merge commits are replicated, including hand-resolved conflict resolutions
("evil merges"), `-s ours` discards, and octopus joins.

## Installation

```sh
go install github.com/grailbio/grit@latest
```

## Usage

```sh
grit [-push] [-dump] [-linearize] <source> <destination> [rules...]
```

Repositories are specified as `url[,prefix[,branch]]`. The prefix defaults to
`""` (repository root) and branch defaults to `master`.

## Cache Management

Clone caches reside in the operating system temporary directory by default and
are swept automatically: entries unused for 14 days (336 hours) are removed.
Set `GRIT_CACHE_DIR` to an absolute path to relocate caches to persistent
storage, and `GRIT_CACHE_TTL` to modify or disable (`0`) the sweep window.
Paused conflict-resolution sessions are never swept.

## Concurrency

Run one grit process per source/destination pair. On a single host, a
per-endpoint lock in the clone cache serializes concurrent runs against the
same destination. Across hosts without a shared filesystem, competing pushes
to the same destination branch fail non-fast-forward; grit discards its local
state on rejection, and rerunning recomputes against the updated destination tip.

## Documentation

See the [package documentation](https://pkg.go.dev/github.com/grailbio/grit) for
detailed documentation on merge replication, synchronization rules, and configuration.
