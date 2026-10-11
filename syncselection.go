// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"strings"

	"github.com/grailbio/base/log"
	"github.com/joeycumines/grit/git"
)

// dropForeignMergedAncestry removes candidates merged in from a history
// sharing no common ancestor with the source-side resume revision ("repo
// absorption" merges): such patches are expressed against parent states the
// destination never held, so replaying them corrupts drifted files, while
// their net content arrives through the merges' own reconciliation. Internal
// side branches, which do share history with the anchor, keep being copied
// individually.
//
// Every parent of every merge is inspected: git assigns parent order from
// the committer's checkout state at merge time, so an unrelated lineage can
// be the FIRST parent (a foreign branch merged the mainline and was itself
// fast-forwarded), and a foreign history can internally merge anchored side
// branches whose shared ancestry must not mask the foreign first parent.
// Merge reconciliation is parent-symmetric: it unions candidate paths over
// all parents and pins the merge's own tree, so excluding any disjoint
// parent loses no content. A related first parent costs one merge-base call
// and nothing else.
//
// An empty anchor revision (initial synchronization) is passed through
// unchanged: with no synchronized revision there is no reference to be
// disjoint from (every root is equally unrelated), and refusing to
// replay would mirror nothing for sources absorbed before their first sync.
// Overlapping paths across disjoint roots pause loudly through the ordinary
// conflict machinery; merged content still converges through reconciliation.
//
// An anchor sharing no history with the source branch itself (a rewritten
// source history whose pre-rewrite digests still resolve, or similar legacy
// state) also stands the filter down: classifying against such a reference
// would discard legitimate mainline work. An anchor that is itself an
// absorbed foreign commit left behind by older grit versions remains outside
// automatic recovery and needs manual repair; grit no longer creates one,
// because excluded commits are never applied or tagged.
func dropForeignMergedAncestry(src *git.Repo, commits []*git.Commit, srcAnchor, srcTip string) []*git.Commit {
	if srcAnchor == "" {
		return commits
	}
	switch shared, err := src.HasCommonAncestor(srcAnchor, srcTip); {
	case err != nil:
		log.Fatalf("%s: merge-base between resume anchor %s and %s: %v", src, srcAnchor, srcTip, err)
	case !shared:
		log.Printf("resume anchor %s shares no history with source branch %s: "+
			"skipping unrelated-history exclusion for this run", srcAnchor, srcTip)
		return commits
	}
	var foreign map[string]bool
	for _, c := range commits {
		parents, err := src.Parents(c.Digest.Hex())
		if err != nil {
			log.Fatalf("%s: inspecting %s for merge parents: %v", src, c.Digest.Hex()[:7], err)
		}
		if len(parents) < 2 {
			continue
		}
		for _, parent := range parents {
			shared, err := src.HasCommonAncestor(srcAnchor, parent)
			if err != nil {
				log.Fatalf("%s: merge-base against merged-in parent %s: %v", src, parent[:7], err)
			}
			if shared {
				continue
			}
			lineage, err := src.RevListExcluding(parent, srcAnchor)
			if err != nil {
				log.Fatalf("%s: rev-list %s: %v", src, parent[:7], err)
			}
			if foreign == nil {
				foreign = make(map[string]bool, len(lineage))
			}
			for _, hex := range lineage {
				foreign[hex] = true
			}
			log.Printf("merge %s brings unrelated history via parent %s: excluding %d commits from replay; their content arrives through the merge reconciliation", c, parent[:7], len(lineage))
		}
	}
	if len(foreign) == 0 {
		return commits
	}
	kept := make([]*git.Commit, 0, len(commits))
	for _, c := range commits {
		if !foreign[c.Digest.Hex()] {
			kept = append(kept, c)
		}
	}
	return kept
}

// processedSourceIDs returns the set of shipit source ids recorded in
// the destination repository's history. Only ids of at least seven
// characters are trusted, mirroring strip-commit's minimum: shorter ids
// would exclude disproportionately large slices of the digest space.
func processedSourceIDs(dst *git.Repo) (map[string]bool, error) {
	// Tag state is message-level; a prefix pathspec would silently empty
	// the set whenever the prefix subtree is absent from the worktree,
	// defeating exactly-once exclusion and degrading to full replays.
	commits, err := dst.LogIgnoringPrefix("--grep=shipit-source-id:")
	if err != nil {
		return nil, err
	}
	processed := make(map[string]bool)
	for _, c := range commits {
		for _, id := range c.OwnLineShipitIDs() {
			if len(id) >= 7 {
				processed[id] = true
			}
		}
	}
	return processed, nil
}

// isProcessed reports whether the provided full source digest is
// accounted for by a recorded shipit source id, either exactly or (for
// legacy, abbreviated ids) by prefix. Residual ambiguity: a destination
// message that quotes an id on its own line (including one whose
// four-space indentation Log dedents away) is indistinguishable from a
// real tag, the same exposure the anchor walk has always had.
func isProcessed(hex string, processed map[string]bool) bool {
	if processed[hex] {
		return true
	}
	for id := range processed {
		if len(id) < len(hex) && strings.HasPrefix(hex, id) {
			return true
		}
	}
	return false
}
