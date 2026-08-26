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

## Documentation

See the [package documentation](https://pkg.go.dev/github.com/grailbio/grit) for
detailed documentation on merge replication, synchronization rules, and configuration.
