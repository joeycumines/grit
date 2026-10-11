// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// Grit copies commits from a source repository to a destination
// repository. It is intended to mirror projects residing in a
// private monorepo to an external project-specific Git repository.
//
// Usage:
//
//	grit [-push] [-dump] [-linearize] [-resolve-empty] src dst rules...
//
// "grit -push src dst rules..." copies commits from the repository
// src to the repository dst, applying the given rules and, if
// successful, pushes the changes to the destination repository.
// Repositories are named by url, prefix, and branch, with one of the
// following syntaxes:
//
//	url
//	url,prefix
//	url,prefix,branch
//
// The default prefix is "" and the default branch is "master". When a
// prefix is specified, grit constructs a view of the repository
// limited to the given prefix path. Changes outside of this prefix are
// discarded.
//
// # Merge commits
//
// Merge commits in the source history are replicated as well. Each
// merge is reconciled at its position in the topological order, after
// all of its ancestors' changes have been applied: the state the
// merge's own tree records for the paths it touches is written to the
// destination, and whatever differs from that state is applied as an
// ordinary corrective commit tagged with the merge's digest. This
// replicates content that no patch can express on its own, such as
// hand-resolved conflict results ("evil merges") and the discards of
// merges made with "-s ours". A merge whose result matches the
// already-replicated state creates no commit at all. Convergence cuts
// both ways: a competing destination-side edit on a path the merge
// resolves is superseded rather than paused, while edits on paths the
// merge's ancestor commits touch pause through their own application.
// Because reconciliation enforces the merged tree's content, a "-s subtree"
// merge whose structural relocation crosses the configured prefix is
// out of scope: its relocated layout cannot be expressed by prefix
// filtering alone.
//
// # Linearization
//
// If the flag -linearize is provided, grit rewrites the cached clone
// of the source repository into a single-parent history before
// copying commits, so that every change arrives as an ordinary patch
// even when the upstream history branches and merges. The rewrite is
// recomputed from the remote at the start of each run, applied only
// to the cached clone, and never pushed: the source repository itself
// is left untouched.
//
// # Incremental synchronization
//
// When resuming from the last synchronized source commit X, grit
// copies every commit in X..<branch>, including commits merged in
// from side branches after X was synced, and applies them in
// topological order, so linear histories are not required for
// correctness. Commits merged in from a history sharing no common
// ancestor with X, such as an entire repository absorbed with "merge
// --allow-unrelated-histories", are excluded from that enumeration:
// their patches assume parent states the destination never held, so
// they are replicated only through the merges' own reconciliation.
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
//	# grailXXXXX is the copy of repoA managed by grit, under the clone
//	# cache root (the OS temporary directory by default; see README).
//	cd /tmp/grit/grailXXXXX
//	# Squash changes into one
//	git reset --soft origin/master && git commit --edit -m"$(git log --reverse HEAD..HEAD@{1})"
//	# Start a regular code review process.
//	arc diff
//	# After the review is accepted, land the changes.
//	arc land
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/grailbio/base/log"
	"github.com/joeycumines/grit/git"
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
	linearize := flag.Bool("linearize", false, "rewrite the cached source clone to a single-parent history before copying commits; the source repository is left untouched")
	resolveEmpty := flag.Bool("resolve-empty", false, "record a paused tagged Grit patch as empty after staging a no-op resolution")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() < 2 {
		flag.Usage()
	}
	if (*push || *resolveEmpty) && *dump {
		flag.Usage()
	}
	srcURL, srcPrefix, srcBranch, err := parseSpec(flag.Arg(0))
	if err != nil {
		log.Fatalf("%v", err)
	}
	dstURL, dstPrefix, dstBranch, err := parseSpec(flag.Arg(1))
	if err != nil {
		log.Fatalf("%v", err)
	}
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
			rule, err := parseRewriteRule(parts[1])
			if err != nil {
				log.Fatalf("%v", err)
			}
			rules.rewrite = append(rules.rewrite, rule)
		default:
			log.Fatalf("invalid rule type %s", parts[0])
		}
	}

	pausedContextID := pausedAMContextID(
		srcURL, srcPrefix, srcBranch, dstURL, dstPrefix, dstBranch,
		*linearize, *configs, flag.Args()[2:],
	)

	// Bound clone cache growth before any repository opens: stale
	// entries are removed here, and entries this run goes on to open
	// were either fresh enough to survive the sweep or are rebuilt in
	// place. Held locks make concurrent operators mutually invisible.
	git.SweepCache()

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

	if *resolveEmpty {
		if err := dst.ResolveEmptyAM(src, pausedContextID); err != nil {
			log.Fatalf("resolve paused patch as empty: %v", err)
		}
		log.Print("empty synchronization commit is in place for the paused patch")
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
			log.Printf("  no-op:     for a tagged patch, restore all tracked changes to HEAD, then re-run with -resolve-empty")
			log.Printf("             use the same source, destination, config, rules, and linearize setting")
			log.Printf("             untagged patches cannot use this option; it refuses tracked changes")
			log.Printf("             that invocation resumes processing; -push pushes on success")
			log.Printf("  regular:   after resolving with git am --continue, re-run this grit command")
			log.Fatalf("session for this source/destination configuration is paused")
		}
	}

	// Make the source repository's objects available in the destination
	// clone, so that three-way merges have the pre-image blobs recorded
	// by patches at their disposal, and merge reconciliation can read
	// the source trees it rewrites into destination state. Open
	// guarantees a resolvable source branch by this point. Dump mode
	// never applies patches or pushes; like every run it still refreshes
	// its repository clones, and it is subject to the preservation gate
	// on foreign unpushed state.
	if err := dst.FetchObjects(src.RepoRoot(), srcBranch); err != nil {
		log.Fatalf("fetch source objects: %v", err)
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
		// The anchor must be grit's own flush-left serialization:
		// Patch.Write appends the tag as the final, unindented body
		// line. A tolerant ^\s* here would let a Log-dedented (or
		// author-indented) quotation of a source digest inside a commit
		// message act as the resume anchor and permanently bound the
		// range past unsynced commits.
		last, err := dst.LogIgnoringPrefix("-1", "--grep", `^\(fb\)\?shipit-source-id: [a-z0-9]\+$`, head)
		if err != nil {
			log.Fatalf("log %s: %v", dst, err)
		}
		if len(last) == 0 {
			break
		}
		// Anchors recording source revisions that no longer exist
		// (rewritten or recreated source histories) cannot bound a
		// resume range: stepping past them lets the run fall through to
		// initial synchronization, where tag-set exclusion drops
		// already-mirrored commits and convergence pruning absorbs
		// equivalent content.
		anchorIDs := last[0].OwnLineShipitIDs()
		if len(anchorIDs) == 0 {
			anchorIDs = last[0].ShipitID()
		}
		resolvable := false
		disjoint := false
		for _, id := range anchorIDs {
			if _, rerr := src.RevParse(id + "^{commit}"); rerr == nil {
				// An id that resolves but is not an ancestor of the
				// configured source branch cannot bound a meaningful
				// resume range: id..srcBranch would enumerate commits
				// unrelated to the branch being mirrored, and replaying
				// them as patches would conflict with states the
				// destination never held. This arises when the cache
				// holds objects from reverse-direction syncs
				// (refs/grit/src), absorbed foreign histories, or
				// rewritten lineages whose pre-rewrite digests still
				// resolve. Treat such anchors the same as unresolvable
				// ones and walk past them.
				isAnc, aerr := src.IsAncestor(id, srcBranch)
				if aerr != nil {
					log.Fatalf("%s: check ancestor of source branch %s for %s: %v", src, srcBranch, id, aerr)
				}
				if isAnc {
					resolvable = true
					break
				}
				disjoint = true
			}
		}
		if len(anchorIDs) > 0 && !resolvable {
			log.Printf("commit %s is not applicable to %s: skipping", last[0], dst)
			if disjoint {
				log.Printf("anchor records source id(s) %v not ancestral to source branch %s; ignoring for resume", anchorIDs, srcBranch)
			} else {
				log.Printf("anchor records unresolvable source id(s) %v; ignoring for resume", anchorIDs)
			}
			head = last[0].Digest.Hex() + "^"
			if _, err := dst.RevParse(head); err != nil {
				log.Printf("anchor walk reached tagged root commit %s with no parent; falling through to initial synchronization", last[0])
				break
			}
			continue
		}
		applies, err := rules.isCommitApplicable(last[0], dst, dst.Prefix())
		if err != nil {
			log.Fatalf("isCommitApplicable %s: %v", last[0], err)
		}
		if !applies {
			// An empty anchor commit (a pure resume marker recording a
			// source revision without any content change) is a valid
			// synchronization point.
			if empty, eerr := dst.PatchIsEmpty(last[0].Digest.Hex()); eerr == nil && empty {
				lastCommit = last[0]
				break
			}
		}
		if applies {
			lastCommit = last[0]
			break
		}
		log.Printf("commit %s is not applicable to %s: skipping", last[0], dst)
		head = last[0].Digest.Hex() + "^"
		if _, err := dst.RevParse(head); err != nil {
			log.Printf("anchor walk reached tagged root commit %s with no parent; falling through to initial synchronization", last[0])
			break
		}
	}
	var commits []*git.Commit
	// Source-side resume revision: the shipit id recorded on lastCommit,
	// empty for initial synchronization.
	var srcAnchor string
	if lastCommit == nil {
		log.Printf("performing initial sync")
		var err error
		// Topological order guarantees that every commit is applied after
		// its ancestors, regardless of authorship dates or merges. Merge
		// commits are included: each is reconciled at its topological
		// position after all of its ancestors' changes have been applied.
		commits, err = src.Log("--topo-order", "--full-history")
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
		srcAnchor = newestID
		var err error
		// Copy every source commit in newestID..srcBranch, not only those
		// that descend from newestID: work merged into srcBranch from side
		// branches ("Merge branch 'wip'") does not descend from an older tip
		// of srcBranch, and must not be silently skipped. --topo-order keeps
		// parents ahead of children so patches apply in dependency order,
		// and --full-history defeats git's default history simplification,
		// which would otherwise hide commits whose prefix effect duplicates
		// a surviving merge parent's tree. Merge commits themselves are
		// included and reconciled at their topological position.
		commits, err = src.Log(newestID+".."+srcBranch, "--topo-order", "--full-history")
		if err != nil {
			log.Fatalf("log %s: %v", src, err)
		}
	}

	// Filter out commits which are themselves copies, so that
	// we can properly support multi-way syncing. Own-line matching is
	// required: a source message merely quoting another mirror's id in
	// prose must not cause its changes to be silently dropped.
	// We also filter out commits that match any stripped commits.
	raw := dropForeignMergedAncestry(src, commits, srcAnchor, srcBranch)
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
	// cannot order with respect to the anchor, notably applied siblings
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
	// An initial sync that finds nothing to copy is legitimate for a
	// brand-new module, but it is indistinguishable from a misspelled
	// prefix. When the prefix never existed anywhere in the source
	// history, say so loudly (non-fatally: legitimately empty modules
	// must stay workable).
	if lastCommit == nil && len(commits) == 0 && srcPrefix != "" {
		existed, err := src.PrefixEverExisted(srcPrefix)
		if err != nil {
			log.Printf("warning: could not probe whether prefix %q ever existed: %v", srcPrefix, err)
		} else if !existed {
			log.Error.Printf("warning: %s: prefix %q does not exist anywhere in the source history; "+
				"check the spelling of the prefix field", src, srcPrefix)
		}
	}
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		isMerge, err := src.IsMerge(c.Digest.Hex())
		if err != nil {
			log.Fatalf("%s: inspecting %s for merge parents: %v", src, c.Digest.Hex()[:7], err)
		}
		if isMerge {
			// Merge content cannot be serialized by format-patch; the
			// merge is reconciled instead: its net effect at this
			// topological position becomes an ordinary corrective patch.
			patch, empty, err := reconcilePatch(src, dst, c)
			if err != nil {
				log.Fatalf("%s: reconcile merge %s: %v", src, c.Digest.Hex()[:7], err)
			}
			if empty {
				log.Printf("skipping converged merge %s", c)
				continue
			}
			// The same path rules gate merged content as gate regular
			// commits: stripped paths never reach the destination, and
			// rewrite rules transform what does. Convergence pruning is
			// unnecessary here: TreeDiffs already emits only paths whose
			// desired state differs from the destination tree.
			var mdiffs []git.Diff
			stripMessage := true
		mdiffloop:
			for _, diff := range patch.Diffs {
				if match, re := rules.isPathStripped(diff.Path); match {
					log.Debug.Printf("file %s matches rule %s: stripping", diff.Path, re)
					continue mdiffloop
				}
				if match, _ := rules.isMessagePathStripped(diff.Path); !match {
					stripMessage = false
				}
				rules.rewriteDiff(&diff)
				mdiffs = append(mdiffs, diff)
			}
			if len(mdiffs) == 0 {
				log.Printf("skipping merge %s: every changed path is stripped", c)
				continue
			}
			total := len(patch.Diffs)
			patch.Diffs = mdiffs
			shipitTag := fmt.Sprintf("fbshipit-source-id: %s", patch.ID.Hex())
			if pruned := total - len(mdiffs); pruned > 0 {
				// A partially stripped merge stays untagged so it is
				// re-examined if the rules change, mirroring regular
				// convergence pruning; the marker documents why it
				// carries no tag.
				patch.Body += fmt.Sprintf("\ngrit-convergence-pruned: %d/%d", pruned, total)
				log.Printf("%s: %d of %d merged paths stripped by rules; merge will remain re-examinable", c, pruned, total)
			} else {
				if patch.Body != "" {
					patch.Body += "\n\n"
				}
				patch.Body += shipitTag
				if stripMessage {
					patch.Subject = "Stripped commit"
					patch.Body = "Commit message stripped.\n\n" + shipitTag
				}
			}
			if *dump {
				// Unlike regular commits, the merge's diffs were already
				// pruned against the pre-run destination tree, so a dump
				// previews what this merge contributes relative to the
				// untouched destination rather than to the state earlier
				// dumps in the same run pretend to have reached.
				if err := patch.Write(os.Stdout); err != nil {
					log.Fatal(err)
				}
				continue
			}
			applyPatch(dst, src, patch, c, &ncommit, pausedContextID)
			continue
		}
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
					if err == nil && (curBlob == newBlob || (isZeroBlob(curBlob) && isZeroBlob(newBlob))) {
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
		// A partially pruned commit stays untagged for the same reason
		// as the fully converged ones above: tagging would freeze the
		// pruned half even if the destination later loses it. Each run
		// re-examines it; kept diffs no-op through three-way
		// application while pruned paths re-converge or resurface.
		pruned := len(diffs) - len(kept)
		tagged := pruned == 0
		if tagged {
			if patch.Body != "" {
				patch.Body += "\n\n"
			}
			patch.Body += shipitTag
		} else {
			// Machine-checkable marker proving grit authored this
			// untagged commit: the authorship gates accept either a
			// shipit tag or this marker, and the count documents why
			// the commit carries no tag.
			patch.Body += fmt.Sprintf("\ngrit-convergence-pruned: %d/%d", pruned, len(diffs))
			if !*dump {
				log.Printf("%s: %d of %d diffs already converged; commit will remain re-examinable", c, pruned, len(diffs))
			}
		}
		// Counts commits that actually moved the destination HEAD:
		// am --3way may legitimately conclude "No changes -- Patch
		// already applied" for a missed exclusion, creating no commit.
		// Such misses self-heal on subsequent runs (the exclusion set
		// grows from the tags that do land), so the imprecision is
		// benign, and counting outcomes keeps untagged re-examinations
		// from spamming empty pushes.
		patch.Diffs = kept
		if stripMessage && tagged {
			patch.Subject = "Stripped commit"
			patch.Body = "Commit message stripped.\n\n" + shipitTag
		}
		if *dump {
			if err := patch.Write(os.Stdout); err != nil {
				log.Fatal(err)
			}
			continue
		}
		applyPatch(dst, src, patch, c, &ncommit, pausedContextID)
	}
	if nskipped > 0 {
		log.Printf("%d commits skipped as already converged", nskipped)
	}

	if !*push {
		return
	}
	// Push when this run applied anything, or when the repository still
	// holds commits from a manually continued session that have not been
	// pushed yet. Open only preserves unpushed state whose every commit
	// is grit-authored (carrying a shipit id or a convergence-pruned
	// marker), so an unpushed HEAD here is grit's own by construction;
	// the re-check keeps that invariant local and loud.
	if ncommit == 0 {
		head, err := dst.Head()
		if err != nil {
			log.Fatal(err)
		}
		if head == dst.OriginHead() {
			log.Print("nothing to do")
			return
		}
		authored, err := dst.PreservedTailIsGritAuthored(dst.OriginHead())
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
		// The local state is left intact: a push can fail for reasons
		// that say nothing about divergence (LFS upload, auth, network,
		// server hooks), and discarding it here would destroy the only
		// copy of manually resolved work. The next run re-opens against
		// the destination's actual tip and its preservation gate decides
		// what survives; a genuine non-fast-forward stays recoverable in
		// this cache until then.
		log.Fatalf("%s: push origin %s: %v", dst, dstBranch, err)
	}
}

// isZeroBlob reports whether the digest is a zero blob of any hash length:
// absent paths read back as zeros, and deletions record zeros whose width
// follows the repository's hash function (40 hex for SHA-1, 64 for SHA-256).
func isZeroBlob(blob string) bool {
	if blob == "" {
		return false
	}
	for _, c := range blob {
		if c != '0' {
			return false
		}
	}
	return true
}

// parseSpec splits a repository spec into its url, prefix, and branch
// components, applying the documented defaults for omitted fields.
func parseSpec(spec string) (url, prefix, branch string, err error) {
	parts := strings.Split(spec, ",")
	switch len(parts) {
	case 1:
		return parts[0], "", "master", nil
	case 2:
		return parts[0], parts[1], "master", nil
	case 3:
		return parts[0], parts[1], parts[2], nil
	default:
		return "", "", "", fmt.Errorf("invalid spec %s: too many comma-separated fields", spec)
	}
}
