// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/joeycumines/grit/git"
)

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

// rewriteDiff applies the ruleset's rewrite rules to the provided diff.
// Diffs carrying git binary sections are never rewritten: git
// serializes NUL-free binary files as ordinary textual diffs. Such
// payloads are indistinguishable from text at this level; rewrite
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
// in the provided repository and prefix. Paths are relocated under
// dstPrefix before rules are evaluated, exactly as the application loop
// relocates them: the production caller passes the destination's prefix,
// so the anchor walk and the apply loop judge the same paths.
func (r rules) isCommitApplicable(c *git.Commit, src *git.Repo, dstPrefix string) (bool, error) {
	if r.isStripped(c) {
		return false, nil
	}
	patch, err := src.Patch(c.Digest, dstPrefix)
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
