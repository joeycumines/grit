// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/grailbio/base/log"
	"github.com/joeycumines/grit/git"
)

func pausedAMContextID(srcURL, srcPrefix, srcBranch, dstURL, dstPrefix, dstBranch string, linearize bool, configs string, rules []string) string {
	fields := []string{
		srcURL, srcPrefix, srcBranch,
		dstURL, dstPrefix, dstBranch,
		fmt.Sprint(linearize), configs,
	}
	fields = append(fields, rules...)
	hash := sha256.New()
	for _, field := range fields {
		fmt.Fprintf(hash, "%d:", len(field))
		hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// reconcilePatch derives the corrective patch replicating merge m's net
// effect at its topological position, where every ancestor change has
// already been applied to the destination. The desired state is the
// merge's own tree restricted to the paths the merge touched within the
// source prefix, relocated under the destination prefix. The difference
// is taken against the destination's current tree, so the corrective
// patch converges those paths to the merged state by construction and,
// before rewrite rules transform it, cannot itself conflict: a
// destination-side edit on a path the merge resolves is superseded
// rather than paused. Conflicts on paths the merge's ancestor commits
// touch still pause loudly through their own three-way application.
// Hand-resolved ("evil") content and "-s ours" discards are both
// expressed this way, byte-exactly.
// The returned patch is untagged: the caller decides between the merge's
// shipit tag and a re-examinability marker after applying path rules.
// An empty result reports a converged state: trivial merges replicate
// nothing and take no tag.
func reconcilePatch(src, dst *git.Repo, m *git.Commit) (git.Patch, bool, error) {
	commits, err := src.LogIgnoringPrefix("-1", "--date=rfc2822", m.Digest.Hex())
	if err != nil {
		return git.Patch{}, false, err
	}
	if len(commits) != 1 {
		return git.Patch{}, false, fmt.Errorf("log -1 %s returned %d commits", m.Digest.Hex()[:7], len(commits))
	}
	mc := commits[0]
	parents, err := src.Parents(m.Digest.Hex())
	if err != nil {
		return git.Patch{}, false, err
	}
	entries, removals, err := src.MergeChangeset(m.Digest.Hex(), parents, dst.Prefix())
	if err != nil {
		return git.Patch{}, false, err
	}
	if len(entries) == 0 && len(removals) == 0 {
		return git.Patch{}, true, nil
	}
	desired, err := dst.DesiredTree(entries, removals)
	if err != nil {
		return git.Patch{}, false, err
	}
	diffs, err := dst.TreeDiffs(desired)
	if err != nil {
		return git.Patch{}, false, err
	}
	if len(diffs) == 0 {
		return git.Patch{}, true, nil
	}
	when, err := mc.AuthorTime()
	if err != nil {
		return git.Patch{}, false, err
	}
	subject := mc.Title()
	patch := git.Patch{
		ID:      m.Digest,
		Author:  mc.AuthorIdent(),
		Time:    when,
		Subject: subject,
		Body:    strings.TrimSpace(strings.TrimPrefix(mc.Body, subject)),
		Diffs:   diffs,
	}
	return patch, false, nil
}

// applyPatch applies patch to the destination repository with loud
// pausing on genuine conflicts, counts commits that actually moved the
// destination HEAD into *ncommit, and copies any LFS objects the patch
// touches from the source repository.
func applyPatch(dst, src *git.Repo, patch git.Patch, c *git.Commit, ncommit *int, contextID string) {
	headBeforeApply, err := dst.Head()
	if err != nil {
		log.Fatal(err)
	}
	*ncommit++
	log.Printf("applying %s", c)
	if err := dst.Apply(patch); err != nil {
		if inProgress, _ := dst.InProgressAM(); inProgress {
			if err := dst.RecordPausedAMContext(contextID); err != nil {
				log.Fatalf("could not bind paused patch to this Grit configuration; resolve with git am --continue or abort: %v", err)
			}
			log.Printf("conflict: %s did not apply cleanly", patch)
			log.Printf("the session is paused for manual resolution in %s", dst.RepoRoot())
			log.Printf("  inspect:   git -C %s status", dst.RepoRoot())
			log.Printf("             git -C %s am --show-current-patch=diff", dst.RepoRoot())
			log.Printf("  resolve:   edit the conflicted files, then git -C %s add <files>", dst.RepoRoot())
			log.Printf("             git -C %s am --continue   (or --abort to abandon the session)", dst.RepoRoot())
			log.Printf("  no-op:     for a tagged patch, restore all tracked changes to HEAD, then re-run with -resolve-empty")
			log.Printf("             use the same source, destination, config, rules, and linearize setting")
			log.Printf("             untagged patches cannot use this option; it refuses tracked changes")
			log.Printf("             that invocation resumes processing; -push pushes on success")
			log.Printf("  regular:   after resolving with git am --continue, re-run this grit command")
		}
		log.Fatalf("%s: apply %s: %s", dst, patch, err)
	}
	if headAfterApply, herr := dst.Head(); herr == nil && headAfterApply == headBeforeApply {
		// Three-way application concluded that the destination already
		// contained this change: nothing was committed.
		*ncommit--
		log.Printf("no changes for %s; treated as converged", c)
	}
	if !patch.MaybeContainsLFSPointer() {
		log.Debug.Printf("%s: patch contains no LFS pointers", patch)
		return
	}
	// Copy any LFS objects that were touched by this change.
	// Doing it this way allows us to download only LFS objects
	// that actually need to be transferred.
	// LFS objects are copied after the patch commits: a listing or copy
	// failure must roll the destination back to its pre-apply state.
	// Otherwise the new commit's shipit tag would mark the source commit
	// processed on the next run while its objects are still missing, and
	// the copy would never be retried.
	srcRelative := srcRelativeDiffPaths(patch.Diffs, dst.Prefix())
	ptrs, err := dst.ListLFSPointers()
	if err != nil {
		rollbackApply(dst, headBeforeApply, ncommit)
		log.Fatalf("listing LFS pointers: %v", err)
	}
	for _, ptr := range ptrs {
		if !srcRelative[ptr] {
			continue
		}
		if err := dst.CopyLFSObject(src, ptr); err != nil {
			rollbackApply(dst, headBeforeApply, ncommit)
			log.Fatalf("copying LFS object %s: %v", ptr, err)
		}
	}
}

// invertDst maps a destination path back to its source-relative form: the
// exact inverse of joinDst for paths under the prefix. A path that merely
// shares the prefix's leading bytes ("dstf.txt" under prefix "dst") is not
// under the prefix and passes through untouched, so LFS matching never
// gains phantom members.
func invertDst(path, dstPrefix string) string {
	if dstPrefix == "" {
		return path
	}
	if path == dstPrefix {
		return ""
	}
	if strings.HasSuffix(dstPrefix, "/") {
		return strings.TrimPrefix(path, dstPrefix)
	}
	if path == dstPrefix+"/" {
		return ""
	}
	return strings.TrimPrefix(path, dstPrefix+"/")
}

// rollbackApply undoes a just-committed patch application after a
// post-commit step failed, so the source commit stays unprocessed and the
// next run retries the whole unit. The commit count is restored alongside
// HEAD; rollback itself is best-effort and loud on failure.
func rollbackApply(dst *git.Repo, headBeforeApply string, ncommit *int) {
	if headAfter, herr := dst.Head(); herr == nil && headAfter != headBeforeApply {
		if rerr := dst.ResetToCommit(headBeforeApply); rerr != nil {
			log.Printf("rolling back failed LFS step also failed: %v", rerr)
			return
		}
		*ncommit--
	}
}

// srcRelativeDiffPaths maps destination-relative diff paths back to
// source-relative ones: patch paths carry the destination prefix (the
// source prefix was rewritten away), while git-lfs pointer listings are
// prefix-stripped. Matching happens in source-relative space so that
// prefixed destinations are not silently skipped.
func srcRelativeDiffPaths(diffs []git.Diff, dstPrefix string) map[string]bool {
	srcRelative := make(map[string]bool, len(diffs))
	for _, diff := range diffs {
		srcRelative[invertDst(diff.Path, dstPrefix)] = true
	}
	return srcRelative
}
