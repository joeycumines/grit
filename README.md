> This is a maintained fork of [Grit](https://github.com/grailbio/grit).

# Grit

![](https://github.com/joeycumines/grit/workflows/CI/badge.svg)

Grit synchronizes Git repository branches, copying commits from a source
repository to a destination repository. It is designed to mirror project
subtrees residing in a monorepo to standalone external repositories.

Merge commits are replicated, including hand-resolved conflict resolutions
("evil merges"), `-s ours` discards, and octopus joins.

## Installation

```sh
go install github.com/joeycumines/grit@latest
```

## Usage

```sh
grit [-push] [-dump] [-linearize] [-resolve-empty] <source> <destination> [rules...]
```

Repositories are specified as `url[,prefix[,branch]]`. The prefix defaults to
`""` (repository root) and branch defaults to `master`.

When a paused, single-patch tagged conflict should make no destination content
changes, restore every tracked change from the attempted patch to `HEAD`,
including paths that merged cleanly, so both the index and tracked worktree
match `HEAD`. For example, use
`git -C <clone> restore --source=HEAD --staged --worktree -- <paths>` for all
tracked paths changed by the attempt, including paths that merged cleanly.
Then rerun the same command with `-resolve-empty`. New paused sessions are
bound to the source and destination specs, rules, `-config` values, and
linearization setting used to create them. The flag refuses a changed
configuration, unmerged paths, untagged patches, a moved `HEAD`, or any
staged or unstaged tracked changes. For older paused sessions without saved
configuration, Grit can verify source-commit ancestry but warns that it cannot
check the original prefix or rules. Untracked files are not part of this check.

Grit records an empty commit carrying the paused source patch's synchronization
tag and resumes processing in that invocation. With `-push`, it pushes on
success; without `-push`, the commits remain local in the destination cache
until a later invocation pushes them. A later conflict pauses again.

Genuinely empty source commits (or commits with no applicable diffs after
prefixes and rules) are skipped and are not copied as empty destination
commits. A later applied, tagged commit whose ancestry includes them advances
the synchronization anchor past them; trailing empty commits alone do not
advance it.

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

See the [package documentation](https://pkg.go.dev/github.com/joeycumines/grit) for
detailed documentation on merge replication, synchronization rules, and configuration.
