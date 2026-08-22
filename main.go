// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// Grit copies commits from a source repository to a destination
// repository. It is intended to mirror projects residing in an
// private monorepo to an external project-specific Git repository.
//
// Usage:
//
//	grit [-push] [-dump] [-linearize] src dst rules...
//
// "grit -push src dst rules..." copies commits from the repository
// src to the repository dst, applying the the given rules and, if
// successful, pushes the changes to the destination repository.
// Repositories are named by url, prefix, and branch, with one of the
// following syntaxes:
//
//	url
//	url,prefix
//	url,prefix,branch
//
// The default prefix is "" and the default branch is "master". When a
// prefix is specified, Grit considers constructs a view of the repository
// limited to the given prefix path. Changes outside of this prefix are
// discarded.
//
// # Linearization
//
// If the flag -linearize is provided, then the source repository's
// history is linearized before copying commits. Linearization is
// done by ensuring that every commit has a single parent, so that
// the repository contains no merge commits. This is useful to ensure
// that grit can cleanly apply patches from repositories whose
// histories are not linear (e.g., when accepting patches from
// GitHub).
//
// # Incremental synchronization
//
// When resuming from the last synchronized source commit X, grit
// copies every commit in X..<branch> -- including commits merged in
// from side branches after X was synced -- and applies them in
// topological order, so linear histories are not required for
// correctness.
//
// # Rules
//
// Grit can apply a set of rewrite rules to source commits before
// they are copied to the destination repository. Rules are specified
// as "kind:param". Rules kinds are:
//
//	strip:regexp
//	  Strips diffs applied to files matching the given regular
//	  expression.
//
//	strip-message:regexp
//	  Strips commit messages when all files with changes match the given
//	  regular expression. This rule can be used to push internal cross-repo
//	  maintenance changes that do not need a context in the external world. For
//	  example, go.mod and go.sum files.
//
//	strip-commit:hash
//	  Strip the commit named by the given hash. This is useful for excluding
//	  troublesome commits that you know are safe to ignore.
//
//	rewrite:regexp:/old_re/new_re/
//	  For each file whose path matches regexp, regexp-replace each line in the
//	  file from old_re to new_re. For example, rule
//
//	rewrite:go.mod$:/replace .* => .*//
//	  will remove all "replace from => to" directives from go.mod
//	  files.  The 2nd letter after the path regexp ('/' in the example)
//	  determines the separator character for the old and the new regexps. The
//	  previous example can also be written as
//
//	rewrite:go.mod$:!replace .* => .*!!
//
// # One way sync
//
// Copy commits from the "project/" directory in repository
// ssh://git@git.company.com/foo.git to the root directory in the
// repository https://github.com/company/project.git. Diffs applied
// to files named BUILD are skipped.
//
//	grit -push ssh://git@git.company.com/foo.git,project/ \
//		https://github.com/company/project.git "strip:^BUILD$" "strip:/BUILD$"
//
// # Two-way sync
//
// Assume we want to sync bidirectionally between two repositories:
//
//	repoA=ssh://git@company.example.com,go/src/github.com/grailbio/project.git
//	repoB=ssh://git@github.com:github.com/grailbio/project.git
//
// We usually develop on repoA and mirror changes to repoB. We also want to
// accept external contributions or upstream changes from repoB and push them to
// repoA. To sync from repoA to repoB, do the following:
//
//	grit -push $repoA $repoB
//
// To sync from repoB to repo A, do the following:
//
//	# Pull changes from repoB to repoA. But don't push it automatically, since we want to
//	# review them internally.
//	grit $repoB $repoA
//	# grailXXXXX is the copy of repoA managed by grit
//	cd /var/tmp/grit/grailXXXXX
//	# Squash changes into one
//	git reset --soft origin/master && git commit --edit -m"$(git log --reverse HEAD..HEAD@{1})"
//	# Start a regular code review process.
//	arc diff
//	# After the review is accepted, land the changes.
//	arc land
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/grailbio/base/log"
	"github.com/grailbio/grit/git"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
	grit src dst rules...
	grit -push src dst rules...
	grit -dump src dst rules`)
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	log.SetPrefix("")
	log.AddFlags()
	dump := flag.Bool("dump", false, "dump patches to stdout instead of applying them to the destination repository")
	push := flag.Bool("push", false, "push applied changes to the destination repository's remote")
	configs := flag.String("config", "", "comma-separated key-value pairs that should be passed to git")
	linearize := flag.Bool("linearize", false, "linearize source repository history before copying commits")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() < 2 {
		flag.Usage()
	}
	if *push && *dump {
		flag.Usage()
	}
	srcURL, srcPrefix, srcBranch := parseSpec(flag.Arg(0))
	dstURL, dstPrefix, dstBranch := parseSpec(flag.Arg(1))
	if srcURL == dstURL {
		log.Error.Printf("source and destination cannot be the same")
		flag.Usage()
	}

	var rules rules
	for _, rule := range flag.Args()[2:] {
		parts := strings.SplitN(rule, ":", 2)
		if len(parts) != 2 {
			log.Fatalf("invalid rule %s", rule)
		}
		switch parts[0] {
		case "strip":
			r, err := regexp.Compile(parts[1])
			if err != nil {
				log.Fatalf("invalid regexp %s: %s", parts[1], err)
			}
			rules.strip = append(rules.strip, r)
		case "strip-message":
			r, err := regexp.Compile(parts[1])
			if err != nil {
				log.Fatalf("invalid regexp %s: %s", parts[1], err)
			}
			rules.stripMessagePaths = append(rules.stripMessagePaths, r)
		case "strip-commit":
			// Digests are matched against the lowercase hex produced by
			// git; normalize and validate here so that uppercase input
			// is not silently accepted and then never matched.
			hash := strings.ToLower(parts[1])
			if len(hash) < 7 {
				log.Fatalf("invalid commit prefix %s: must have at least 7 digits", hash)
			}
			if len(hash) > 40 {
				log.Fatalf("invalid commit prefix %s: longer than a full digest", hash)
			}
			for _, d := range hash {
				if (d < '0' || d > '9') && (d < 'a' || d > 'f') {
					log.Fatalf("invalid commit prefix %s: invalid hex digit %c", hash, d)
				}
			}
			rules.stripCommits = append(rules.stripCommits, hash)
		case "rewrite":
			rules.rewrite = append(rules.rewrite, parseRewriteRule(parts[1]))
		default:
			log.Fatalf("invalid rule type %s", parts[0])
		}
	}

	log.Printf("synchronizing repo:%s prefix:%s branch:%s -> repo:%s prefix:%s branch:%s",
		srcURL, srcPrefix, srcBranch, dstURL, dstPrefix, dstBranch)
	open := func(openFn func(url, prefix, branch string) (*git.Repo, error), url, prefix, branch string) *git.Repo {
		r, err := openFn(url, prefix, branch)
		if err != nil {
			log.Fatalf("open %s: %v", url, err)
		}
		for _, kv := range strings.Split(*configs, ",") {
			if kv == "" {
				continue
			}
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				log.Fatalf("bad config %s", kv)
			}
			r.Configure(parts[0], parts[1])
		}
		return r
	}
	// Open repositories in URL order so that we don't deadlock across
	// multiple repositories.
	var src, dst *git.Repo
	if srcURL < dstURL {
		src = open(git.Open, srcURL, srcPrefix, srcBranch)
		dst = open(git.OpenDestination, dstURL, dstPrefix, dstBranch)
	} else {
		dst = open(git.OpenDestination, dstURL, dstPrefix, dstBranch)
		src = open(git.Open, srcURL, srcPrefix, srcBranch)
	}
	defer src.Close()
	defer dst.Close()

	if *linearize {
		if err := src.Linearize(); err != nil {
			log.Fatalf("linearize %s: %v", src, err)
		}
	}

	// A paused session takes priority over everything else: selection
	// and convergence pruning must not make decisions while resolution
	// is outstanding. Dump mode applies nothing, pushes nothing, and
	// previews the unpruned candidate set, so it may proceed for
	// read-only inspection even mid-pause.
	if !*dump {
		if inProgress, err := dst.InProgressAM(); err != nil {
			log.Fatal(err)
		} else if inProgress {
			log.Printf("conflict resolution is still pending in %s", dst.RepoRoot())
			log.Printf("  inspect:   git -C %s status", dst.RepoRoot())
			log.Printf("             git -C %s am --show-current-patch=diff", dst.RepoRoot())
			log.Printf("  resolve:   edit the conflicted files, then git -C %s add <files>", dst.RepoRoot())
			log.Printf("             git -C %s am --continue   (or --abort to abandon the session)", dst.RepoRoot())
			log.Printf("  then:      re-run this grit command to finish the remaining commits and push")
			log.Fatalf("session for this source/destination configuration is paused")
		}
	}

	// Make the source repository's objects available in the destination
	// clone, so that three-way merges have the pre-image blobs recorded
	// by patches at their disposal. Open guarantees a resolvable source
	// branch by this point. Dump mode never applies patches or pushes;
	// like every run it still refreshes its repository clones, and it is
	// subject to the preservation gate on foreign unpushed state.
	if !*dump {
		if err := dst.FetchObjects(src.RepoRoot(), srcBranch); err != nil {
			log.Fatalf("fetch source objects: %v", err)
		}
	}

	// Configuration mistakes that would silently mirror nothing must
	// abort before any selection happens. A source branch that does not
	// resolve would degrade every selection into an empty result.
	if _, err := src.RevParse(srcBranch); err != nil {
		log.Fatalf("%s: source branch %q does not resolve: %v", src, srcBranch, err)
	}
	if err := src.CheckPrefixCasing(srcPrefix); err != nil {
		log.Fatalf("%s: %v", src, err)
	}
	if err := dst.CheckPrefixCasing(dstPrefix); err != nil {
		log.Fatalf("%s: %v", dst, err)
	}

	// Last synchronized commit that applies, if any. We apply the
	// rewrite rules here, so that we skip commits that may be tagged
	// with shipit IDs, but wouldn't actually come from the source
	// repository. This can happen if a repository is the destination
	// for multiple repositories, and commits sourced from one repo can
	// touch those in another. A common source of this is Bazel BUILD
	// files and go.{mod,sum} files that may be modified independently
	// in the source and destination repositories.
	var lastCommit *git.Commit
	for head := "HEAD"; ; {
		// The anchor is message-level state: it must be found even when
		// the destination prefix subtree is absent from the worktree.
		last, err := dst.LogIgnoringPrefix("-1", "--grep", `^\s*\(fb\)\?shipit-source-id: [a-z0-9]\+$`, head)
		if err != nil {
			log.Fatalf("log %s: %v", dst, err)
		}
		if len(last) == 0 {
			break
		}
		applies, err := rules.isCommitApplicable(last[0], dst)
		if err != nil {
			log.Fatalf("isCommitApplicable %s: %v", last[0], err)
		}
		if applies {
			lastCommit = last[0]
			break
		}
		log.Printf("commit %s is not applicable to %s: skipping", last[0], dst)
		head = last[0].Digest.Hex() + "^"
		if _, err := dst.RevParse(head); err != nil {
			log.Fatalf("anchor walk reached tagged root commit %s with no parent; the oldest recorded shipit id cannot be matched against the source -- retag or rewrite destination history to repair", last[0])
		}
	}
	var commits []*git.Commit
	if lastCommit == nil {
		log.Printf("performing initial sync")
		var err error
		// Topological order guarantees that every commit is applied after
		// its ancestors, regardless of authorship dates or merges.
		commits, err = src.Log("--no-merges", "--topo-order", "--full-history")
		if err != nil {
			log.Fatalf("log %s: %v", src, err)
		}
	} else {
		log.Printf("synchronizing: last diff: %v, source: %v", lastCommit.Digest, lastCommit.ShipitID())
		// Prefer ids recorded on their own line; the body-wide scan is
		// a logged fallback for hand-tagged anchors. Legacy anchors may
		// carry abbreviations shorter than seven characters; git
		// resolves them exactly as upstream did, so the exclusion set's
		// length floor deliberately does not apply here.
		ids := lastCommit.OwnLineShipitIDs()
		if len(ids) == 0 {
			log.Printf("anchor %s carries no own-line id; scanning the body", lastCommit)
			ids = lastCommit.ShipitID()
		}
		if len(ids) == 0 {
			log.Fatalf("no fbshipit-source-id found in commit: %+v", lastCommit)
		}
		newestID := ids[len(ids)-1]
		// Fail loudly when the anchor's source commit is no longer
		// resolvable (rewritten or garbage-collected source history):
		// depending on the git version, an unresolvable revision could
		// otherwise degrade into an empty selection and a silent stall.
		if _, err := src.RevParse(newestID + "^{commit}"); err != nil {
			log.Fatalf("resume anchor %s does not resolve in %s: %v", newestID, src, err)
		}
		var err error
		// Copy every source commit in newestID..srcBranch, not only those
		// that descend from newestID: work merged into srcBranch from side
		// branches ("Merge branch 'wip'") does not descend from an older tip
		// of srcBranch, and must not be silently skipped. --topo-order keeps
		// parents ahead of children so patches apply in dependency order,
		// and --full-history defeats git's default history simplification,
		// which would otherwise hide commits whose prefix effect duplicates
		// a surviving merge parent's tree.
		commits, err = src.Log(newestID+".."+srcBranch, "--topo-order", "--no-merges", "--full-history")
		if err != nil {
			log.Fatalf("log %s: %v", src, err)
		}
	}

	// Filter out commits which are themselves copies, so that
	// we can properly support multi-way syncing. Own-line matching is
	// required: a source message merely quoting another mirror's id in
	// prose must not cause its changes to be silently dropped.
	// We also filter out commits that match any stripped commits.
	raw := commits
	commits = nil
commitsLoop:
	for _, commit := range raw {
		if ids := commit.OwnLineShipitIDs(); len(ids) > 0 {
			log.Printf("skipping %s: already carries shipit source id(s) %v", commit, ids)
			continue commitsLoop
		}
		if rules.isStripped(commit) {
			log.Debug.Printf("commit %s: stripped by strip-commit rule", commit.Digest)
			continue commitsLoop
		}
		commits = append(commits, commit)
	}

	log.Printf("%d commits to copy", len(commits))
	// Exclude commits the destination already accounts for: their source
	// digests appear as shipit source ids somewhere in the destination
	// history. Shipit tags are the only synchronization state that
	// persists through grit's push-only model, so they are what makes
	// exactly-once processing hold for commits that selection alone
	// cannot order with respect to the anchor — notably applied siblings
	// of a failed run, which are not ancestors of its last applied
	// commit. Legacy ids shorter than a full digest match by prefix,
	// with the same collision window the anchor walk has always
	// accepted; new tags record the full digest. Ids shorter than seven
	// characters are ignored outright, mirroring strip-commit's minimum.
	processed, err := processedSourceIDs(dst)
	if err != nil {
		log.Fatalf("processed source ids: %v", err)
	}
	{
		filtered := commits[:0]
		for _, c := range commits {
			if isProcessed(c.Digest.Hex(), processed) {
				log.Printf("skipping already synchronized %s", c)
				continue
			}
			filtered = append(filtered, c)
		}
		commits = filtered
	}
	var ncommit int
	var nskipped int
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		patch, err := src.Patch(c.Digest, dst.Prefix())
		if err != nil {
			log.Fatalf("%s: patch %s: %v", src, c.Digest.Hex()[:7], err)
		}
		if patch.Body != "" {
			patch.Body += "\n\n"
		}
		shipitTag := fmt.Sprintf("fbshipit-source-id: %s", patch.ID.Hex())
		// The tag is attached after convergence pruning below: commits
		// with pruned diffs must land untagged so they remain
		// re-examinable.
		// Apply filepath specific rules.
		// Prefixes are already rewritten by the repo.
		var diffs []git.Diff
		stripMessage := true
	diffloop:
		for _, diff := range patch.Diffs {
			if match, re := rules.isPathStripped(diff.Path); match {
				log.Debug.Printf("file %s matches rule %s: stripping", diff.Path, re)
				continue diffloop
			}
			if match, re := rules.isMessagePathStripped(diff.Path); match {
				log.Debug.Printf("file %s matches rule %s for stripping commit messages", diff.Path, re)
			} else {
				stripMessage = false
			}
			rules.rewriteDiff(&diff)
			diffs = append(diffs, diff)
		}
		if len(diffs) == 0 {
			log.Printf("skipping empty patch %s", patch.ID.Hex()[:7])
			continue
		}
		// Drop diffs whose post-image already matches the destination:
		// the change is present through another route (cherry-picks,
		// direct merges), and applying it again would only conflict with
		// itself. Commits whose diffs all drop are fully converged and
		// are skipped without creating a destination commit; they carry
		// no shipit tag, so they are simply re-examined (and re-pruned)
		// on future runs until they become ancestors of an applied
		// anchor. Convergence requires content and, when the diff
		// declares one, mode to match; a pure mode change records no
		// index line at all, so NewBlob reports !ok and such diffs are
		// kept rather than pruned. Dump mode skips
		// pruning entirely: it previews the unpruned candidate set from
		// the source side only, since it cannot know the destination
		// state that a real run will have built up when it reaches each
		// patch.
		var kept []git.Diff
		if *dump {
			kept = diffs
		} else {
			for _, diff := range diffs {
				// Rewritable paths are exempt from convergence pruning:
				// the recorded post-image is pre-rewrite, while
				// application materializes rewritten content, so blob
				// equality would compare unrelated forms and either
				// freeze un-rewritten content into the destination or
				// miss legitimately converged rewrites.
				if rules.mayRewrite(diff.Path) {
					kept = append(kept, diff)
					continue
				}
				newBlob, ok := diff.NewBlob()
				if ok {
					curBlob, curMode, err := dst.HeadEntry(diff.Path)
					if err == nil && curBlob == newBlob {
						if newMode, ok := diff.NewMode(); !ok || newMode == curMode {
							log.Printf("skipping converged %s in %s", diff.Path, c)
							continue
						}
					}
				}
				kept = append(kept, diff)
			}
		}
		if len(kept) == 0 {
			nskipped++
			continue
		}
		// A commit with pruned diffs is deliberately left without a
		// shipit tag: tagging would permanently exclude it, freezing
		// the pruned half even if the destination later loses that
		// content. Untagged, it is re-examined on every run — the kept
		// diffs no-op through three-way application while the pruned
		// paths re-converge or resurface.
		pruned := len(diffs) - len(kept)
		tagged := pruned == 0
		if tagged {
			if patch.Body != "" {
				patch.Body += "\n\n"
			}
			patch.Body += shipitTag
		} else if !*dump {
			log.Printf("%s: %d of %d diffs already converged; commit will remain re-examinable", c, pruned, len(diffs))
		}
		// Counts commits that actually moved the destination HEAD:
		// am --3way may legitimately conclude "No changes -- Patch
		// already applied" for a missed exclusion, creating no commit.
		// Such misses self-heal on subsequent runs (the exclusion set
		// grows from the tags that do land), so the imprecision is
		// benign, and counting outcomes keeps untagged re-examinations
		// from spamming empty pushes.
		headBeforeApply, err := dst.Head()
		if err != nil {
			log.Fatal(err)
		}
		ncommit++
		patch.Diffs = kept
		if stripMessage && tagged {
			patch.Subject = "Stripped commit"
			patch.Body = "Commit message stripped.\n\n" + shipitTag
		}
		if *dump {
			if err := patch.Write(os.Stdout); err != nil {
				log.Fatal(err)
			}
		} else {
			log.Printf("applying %s", c)
			if err := dst.Apply(patch); err != nil {
				if inProgress, _ := dst.InProgressAM(); inProgress {
					log.Printf("conflict: %s did not apply cleanly", patch)
					log.Printf("the session is paused for manual resolution in %s", dst.RepoRoot())
					log.Printf("  inspect:   git -C %s status", dst.RepoRoot())
					log.Printf("             git -C %s am --show-current-patch=diff", dst.RepoRoot())
					log.Printf("  resolve:   edit the conflicted files, then git -C %s add <files>", dst.RepoRoot())
					log.Printf("             git -C %s am --continue   (or --abort to abandon the session)", dst.RepoRoot())
					log.Printf("  then:      re-run this grit command to finish the remaining commits and push")
				}
				log.Fatalf("%s: apply %s: %s", dst, patch, err)
			}
			if headAfterApply, herr := dst.Head(); herr == nil && headAfterApply == headBeforeApply {
				// Three-way application concluded that the destination
				// already contained this change: nothing was committed.
				ncommit--
				log.Printf("no changes for %s; treated as converged", c)
			}
			if !patch.MaybeContainsLFSPointer() {
				log.Debug.Printf("%s: patch contains no LFS pointers", patch)
				continue
			}
			// Copy any LFS objects that were touched by this change.
			// Doing it this way allows us to download only LFS objects
			// that actually need to be transferred.
			srcRelative := srcRelativeDiffPaths(patch.Diffs, dst.Prefix())
			ptrs, err := dst.ListLFSPointers()
			if err != nil {
				log.Fatal(err)
			}
			for _, ptr := range ptrs {
				if !srcRelative[ptr] {
					continue
				}
				if err := dst.CopyLFSObject(src, ptr); err != nil {
					log.Fatalf("copying LFS object %s: %v", ptr, err)
				}
			}
		}
	}
	if nskipped > 0 {
		log.Printf("%d commits skipped as already converged", nskipped)
	}

	if !*push {
		return
	}
	// Push when this run applied anything, or when the repository still
	// holds commits from a manually continued session that have not been
	// pushed yet. Open only ever preserves such state when HEAD carries
	// a grit shipit id, so an unpushed HEAD here is grit-authored by
	// construction; the re-check keeps that invariant local and loud.
	if ncommit == 0 {
		head, err := dst.Head()
		if err != nil {
			log.Fatal(err)
		}
		if head == dst.OriginHead() {
			log.Print("nothing to do")
			return
		}
		authored, err := dst.UnpushedCommitsAreGritAuthored(dst.OriginHead())
		if err != nil {
			log.Fatal(err)
		}
		if !authored {
			log.Fatalf("%s holds unpushed commits without a grit shipit id at HEAD; refusing to publish them as synchronization work", dst.RepoRoot())
		}
		log.Print("pushing previously resolved session")
	}
	log.Printf("pushing changes to %s %s", dstURL, dstBranch)
	if err := dst.Push("origin", dstBranch); err != nil {
		log.Fatalf("%s: push origin %s: %v", dst, dstBranch, err)
	}
}

// processedSourceIDs returns the set of shipit source ids recorded in
// the destination repository's history. Only ids of at least seven
// characters are trusted, mirroring strip-commit's minimum: shorter ids
// would exclude disproportionately large slices of the digest space.
func processedSourceIDs(dst *git.Repo) (map[string]bool, error) {
	// Tag state is message-level; a prefix pathspec would silently empty
	// the set whenever the prefix subtree is absent from the worktree,
	// defeating exactly-once exclusion and degrading to full replays.
	commits, err := dst.LogIgnoringPrefix()
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

// srcRelativeDiffPaths maps destination-relative diff paths back to
// source-relative ones: patch paths carry the destination prefix (the
// source prefix was rewritten away), while git-lfs pointer listings are
// prefix-stripped. Matching happens in source-relative space so that
// prefixed destinations are not silently skipped.
func srcRelativeDiffPaths(diffs []git.Diff, dstPrefix string) map[string]bool {
	srcRelative := make(map[string]bool, len(diffs))
	for _, diff := range diffs {
		srcRelative[strings.TrimPrefix(diff.Path, dstPrefix)] = true
	}
	return srcRelative
}

// isProcessed reports whether the provided full source digest is
// accounted for by a recorded shipit source id, either exactly or (for
// legacy, abbreviated ids) by prefix. Residual ambiguity: a destination
// message that quotes an id on its own line — including one whose
// four-space indentation Log dedents away — is indistinguishable from a
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

func parseSpec(spec string) (url, prefix, branch string) {
	parts := strings.Split(spec, ",")
	switch len(parts) {
	case 1:
		return parts[0], "", "master"
	case 2:
		return parts[0], parts[1], "master"
	case 3:
		return parts[0], parts[1], parts[2]
	default:
		log.Fatalf("invalid spec %s", spec)
	}
	panic("not reached")
}

type rewriteRule struct {
	pathRe *regexp.Regexp // matched against the pathname
	oldRe  *regexp.Regexp // matched against each line in the file
	new    []byte         // replacement
}

func parseRewriteRule(rule string) (r rewriteRule) {
	parts := strings.SplitN(rule, ":", 2)
	if len(parts) != 2 {
		log.Fatalf("invalid rewrite rule %s", rule)
	}
	var err error
	if r.pathRe, err = regexp.Compile(parts[0]); err != nil {
		log.Fatalf("rewrite: invalid path regexp %s: %s", parts[0], err)
	}
	if len(parts[1]) < 3 {
		log.Fatalf("rewrite: rule '%s' must be of form rewrite:pathre:/from_re/to_re/", rule)
	}
	sep := parts[1][0:1]
	parts = strings.Split(parts[1][1:], sep)
	if len(parts) != 3 || parts[2] != "" {
		log.Fatalf("rewrite: rule '%s' must be of form rewrite:pathre:/from_re/to_re/", rule)
	}
	if r.oldRe, err = regexp.Compile(parts[0]); err != nil {
		log.Fatalf("rewrite: invalid 'from' regexp %s: %s", parts[0], err)
	}
	r.new = []byte(parts[1])
	return r
}

func (r *rewriteRule) rewrite(diff []byte) []byte {
	result := bytes.Buffer{}
	for _, line := range bytes.Split(diff, []byte("\n")) {
		line = r.oldRe.ReplaceAll(line, r.new)
		result.Write(line)
		result.WriteByte('\n')
	}
	return result.Bytes()
}

type rules struct {
	strip             []*regexp.Regexp
	stripMessagePaths []*regexp.Regexp
	// We store strip prefixes as strings since digesters refuse
	// to parse odd-length hex strings and git typically gives out
	// a prefix with 7 digits.
	stripCommits []string
	rewrite      []rewriteRule
}

// isStripped returns whether this commit matches the strip rules of
// the rule set r.
func (r rules) isStripped(c *git.Commit) bool {
	for _, stripped := range r.stripCommits {
		if strings.HasPrefix(c.Digest.Hex(), stripped) {
			return true
		}
	}
	return false
}

// mayRewrite reports whether any rewrite rule targets the provided
// path.
func (r rules) mayRewrite(path string) bool {
	for _, rule := range r.rewrite {
		if rule.pathRe.MatchString(path) {
			return true
		}
	}
	return false
}

// isPathStripped returns whether the provided path is stripped by the
// ruleset's strip path rules.
func (r rules) isPathStripped(path string) (bool, *regexp.Regexp) {
	for _, re := range r.strip {
		if re.MatchString(path) {
			return true, re
		}
	}
	return false, nil
}

// isMessagePathStripped returns whether the provided path is stripped
// by the ruleset's message strip rules.
func (r rules) isMessagePathStripped(path string) (bool, *regexp.Regexp) {
	for _, re := range r.stripMessagePaths {
		if re.MatchString(path) {
			return true, re
		}
	}
	return false, nil
}

var binaryPatchMarker = []byte("GIT binary patch")

// rewriteDiff applies the rulesets rewrite rules to the provided diff.
// Diffs carrying git binary sections are never rewritten. Note that git
// serializes NUL-free binary files as ordinary textual diffs; such
// payloads are indistinguishable from text at this level, so rewrite
// rules should be anchored narrowly enough that they cannot match their
// bytes.
func (r rules) rewriteDiff(diff *git.Diff) {
	// The parser places binary payloads (including their "GIT binary
	// patch" marker) in Meta and leaves Body empty; textual diffs carry
	// the payload in Body. Neither may be rewritten, and rewriting an
	// empty body must not manufacture content.
	if bytes.Contains(diff.Meta, binaryPatchMarker) || bytes.Contains(diff.Body, binaryPatchMarker) {
		return
	}
	if len(diff.Body) == 0 {
		return
	}
	for _, r := range r.rewrite {
		if r.pathRe.MatchString(diff.Path) {
			diff.Body = r.rewrite(diff.Body)
		}
	}
}

// isCommitApplicable returns whether the provided commit is non-empty
// in the provided repository and prefix.
func (r rules) isCommitApplicable(c *git.Commit, src *git.Repo) (bool, error) {
	if r.isStripped(c) {
		return false, nil
	}
	patch, err := src.Patch(c.Digest, "")
	if err != nil {
		return false, err
	}
	var ndiff int
	for _, diff := range patch.Diffs {
		if match, _ := r.isPathStripped(diff.Path); match {
			continue
		}
		ndiff++
	}
	return ndiff > 0, nil
}
