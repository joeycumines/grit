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
// Because reconciliation enforces the merged tree's content, a
// "-s subtree"
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
// copies every commit in X..<branch> -- including commits merged in
// from side branches after X was synced -- and applies them in
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
	linearize := flag.Bool("linearize", false, "rewrite the cached source clone to a single-parent history before copying commits; the source repository is left untouched")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() < 2 {
		flag.Usage()
	}
	if *push && *dump {
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
		last, err := dst.LogIgnoringPrefix("-1", "--grep", `^\s*\(fb\)\?shipit-source-id: [a-z0-9]\+$`, head)
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
		for _, id := range anchorIDs {
			if _, rerr := src.RevParse(id + "^{commit}"); rerr == nil {
				resolvable = true
				break
			}
		}
		if len(anchorIDs) > 0 && !resolvable {
			log.Printf("commit %s is not applicable to %s: skipping", last[0], dst)
			log.Printf("anchor records unresolvable source id(s) %v; ignoring for resume", anchorIDs)
			head = last[0].Digest.Hex() + "^"
			if _, err := dst.RevParse(head); err != nil {
				log.Fatalf("anchor walk reached tagged root commit %s with no parent; the oldest recorded shipit id cannot be matched against the source -- retag or rewrite destination history to repair", last[0])
			}
			continue
		}
		applies, err := rules.isCommitApplicable(last[0], dst)
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
			log.Fatalf("anchor walk reached tagged root commit %s with no parent; the oldest recorded shipit id cannot be matched against the source -- retag or rewrite destination history to repair", last[0])
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
	raw := dropForeignMergedAncestry(src, commits, srcAnchor)
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
			applyPatch(dst, src, patch, c, &ncommit)
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
		applyPatch(dst, src, patch, c, &ncommit)
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
		// A rejected push means the destination advanced
		// independently of this run's base. Discard the diverged local
		// state: the next run re-selects against the destination's
		// actual tip, and convergence pruning re-derives whatever is
		// still needed.
		if rerr := dst.ResetToRemote(); rerr != nil {
			log.Printf("reset after failed push also failed: %v", rerr)
		} else {
			log.Printf("discarded local state after failed push; re-run this command to recompute against the destination's current tip")
		}
		log.Fatalf("%s: push origin %s: %v", dst, dstBranch, err)
	}
}

// reconcilePatch derives the corrective patch replicating merge m's net
// effect at its topological position, where every ancestor change has
// already been applied to the destination. The desired state is the
// merge's own tree restricted to the paths the merge touched within the
// source prefix, relocated under the destination prefix. The difference
// is taken against the destination's current tree, so the corrective
// patch converges those paths to the merged state by construction and,
// before rewrite rules transform it, cannot itself conflict: a
// destination-side edit on a
// path the merge resolves is superseded rather than paused. Conflicts
// on paths the merge's ancestor commits touch still pause loudly
// through their own three-way application. Hand-resolved ("evil")
// content and "-s ours" discards are both expressed this way,
// byte-exactly.
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
func applyPatch(dst, src *git.Repo, patch git.Patch, c *git.Commit, ncommit *int) {
	headBeforeApply, err := dst.Head()
	if err != nil {
		log.Fatal(err)
	}
	*ncommit++
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

// dropForeignMergedAncestry removes candidates merged in from a history
// sharing no common ancestor with the source-side resume revision ("repo
// absorption" merges): such patches are expressed against parent states the
// destination never held, so replaying them corrupts drifted files, while
// their net content arrives through the merges' own reconciliation. Internal
// side branches, which do share history with the anchor, keep being copied
// individually. An empty anchor revision (initial synchronization) is passed
// through unchanged.
func dropForeignMergedAncestry(src *git.Repo, commits []*git.Commit, srcAnchor string) []*git.Commit {
	if srcAnchor == "" {
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
		for _, parent := range parents[1:] {
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

type rewriteRule struct {
	pathRe *regexp.Regexp // matched against the pathname
	oldRe  *regexp.Regexp // matched against each line in the file
	new    []byte         // replacement
}

// parseRewriteRule parses the parameter of a rewrite rule into its
// path regexp, line regexp, and replacement.
func parseRewriteRule(rule string) (r rewriteRule, err error) {
	parts := strings.SplitN(rule, ":", 2)
	if len(parts) != 2 {
		return r, fmt.Errorf("invalid rewrite rule %s", rule)
	}
	if r.pathRe, err = regexp.Compile(parts[0]); err != nil {
		return r, fmt.Errorf("rewrite: invalid path regexp %s: %s", parts[0], err)
	}
	if len(parts[1]) < 3 {
		return r, fmt.Errorf("rewrite: rule '%s' must be of form rewrite:pathre:/from_re/to_re/", rule)
	}
	sep := parts[1][0:1]
	parts = strings.Split(parts[1][1:], sep)
	if len(parts) != 3 || parts[2] != "" {
		return r, fmt.Errorf("rewrite: rule '%s' must be of form rewrite:pathre:/from_re/to_re/", rule)
	}
	if r.oldRe, err = regexp.Compile(parts[0]); err != nil {
		return r, fmt.Errorf("rewrite: invalid 'from' regexp %s: %s", parts[0], err)
	}
	r.new = []byte(parts[1])
	return r, nil
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
// Diffs carrying git binary sections are never rewritten: git
// serializes NUL-free binary files as ordinary textual diffs, such
// payloads are indistinguishable from text at this level, and rewrite
// rules must therefore be anchored narrowly enough not to match them.
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
