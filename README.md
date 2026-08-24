![](https://github.com/grailbio/grit/workflows/CI/badge.svg)

Grit copies commits from a source repository to a destination
repository. It is intended to mirror projects residing in an
private monorepo to an external project-specific Git repository.
Merge commits are replicated too, including hand-resolved conflict
content and "-s ours" discards; see the package documentation's
"Merge commits" section for the exact semantics.

Usage:

	$ go get [-u] github.com/grailbio/grit
	$ grit [-push] [-dump] src dst rules...

[Documentation](https://godoc.org/github.com/grailbio/grit).
