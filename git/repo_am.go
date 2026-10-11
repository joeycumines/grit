// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package git

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/grailbio/base/log"
)

var fullCommitIDRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

const pausedAMContextFile = "grit-context"

// ResolveEmptyAM records the current grit-authored patch as an empty commit
// and clears the paused git am session. The index and tracked worktree must
// already be resolved and match HEAD; this prevents the operation from
// silently discarding a real resolution. The source commit must be reachable
// from the selected source branch.
func (r *Repo) ResolveEmptyAM(source *Repo, contextID string) error {
	if source == nil {
		return errors.New("source repository is required to resolve an empty patch")
	}
	inProgress, err := r.InProgressAM()
	if err != nil {
		return err
	}
	if !inProgress {
		return errors.New("no paused git am session")
	}
	if err := validatePausedAMContextID(contextID); err != nil {
		return err
	}
	storedContext, err := r.amStateFile(pausedAMContextFile)
	contextVerified := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot verify the paused Grit configuration; use the original command or resolve with git am --continue: %w", err)
	}
	if contextVerified && strings.TrimSpace(string(storedContext)) != contextID {
		return errors.New("cannot record an empty resolution: paused patch belongs to a different source, destination, or sync configuration")
	}

	unmerged, err := r.git(nil, "ls-files", "--unmerged", "-z")
	if err != nil {
		return err
	}
	if len(unmerged) != 0 {
		return fmt.Errorf("cannot record an empty resolution while unmerged paths remain; resolve and stage every path first")
	}

	abortSafety, err := r.amStateFile("abort-safety")
	if err != nil {
		return fmt.Errorf("read original HEAD for paused git am session: %w", err)
	}
	originalHead := strings.TrimSpace(string(abortSafety))
	if !fullCommitIDRe.MatchString(originalHead) {
		return fmt.Errorf("cannot record an empty resolution: paused git am session has invalid original HEAD %q", originalHead)
	}

	head, err := r.Head()
	if err != nil {
		return err
	}
	indexTree, err := r.git(nil, "write-tree")
	if err != nil {
		return fmt.Errorf("write resolved index tree: %w", err)
	}
	headTree, err := r.RevParse("HEAD^{tree}")
	if err != nil {
		return err
	}
	indexTreeID := strings.TrimSpace(string(indexTree))
	if indexTreeID != headTree {
		return fmt.Errorf("cannot record an empty resolution: the staged tree differs from HEAD")
	}
	unstaged, err := r.git(nil, "diff", "--name-only", "-z", "--")
	if err != nil {
		return err
	}
	if len(unstaged) != 0 {
		return fmt.Errorf("cannot record an empty resolution: tracked worktree changes are not staged")
	}

	next, err := r.amStateFile("next")
	if err != nil {
		return err
	}
	last, err := r.amStateFile("last")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(next)) != "1" || strings.TrimSpace(string(last)) != "1" {
		return fmt.Errorf("cannot record an empty resolution for a multi-patch git am session")
	}

	message, err := r.amStateFile("final-commit")
	if err != nil {
		return err
	}
	tags := ownLineGritTagRe.FindAllSubmatch(message, -1)
	if len(tags) != 1 || !fullCommitIDRe.Match(tags[0][1]) {
		return fmt.Errorf("cannot record an empty resolution: current patch is not a single tagged Grit patch")
	}
	sourceID := string(tags[0][1])
	resolvedSourceID, err := source.RevParse(sourceID + "^{commit}")
	if err != nil {
		return fmt.Errorf("source commit %s for paused patch is not available in the selected source: %w", sourceID, err)
	}
	if resolvedSourceID != sourceID {
		return fmt.Errorf("source id %s resolves to unexpected commit %s in the selected source", sourceID, resolvedSourceID)
	}
	sourceHead, err := source.RevParse(source.branch)
	if err != nil {
		return fmt.Errorf("resolve selected source branch %s: %w", source.branch, err)
	}
	isSourceAncestor, err := source.IsAncestor(sourceID, sourceHead)
	if err != nil {
		return fmt.Errorf("check paused source commit %s against selected branch %s: %w", sourceID, source.branch, err)
	}
	if !isSourceAncestor {
		return fmt.Errorf("cannot record an empty resolution: paused source commit %s is not reachable from selected source branch %s", sourceID, source.branch)
	}
	if !contextVerified {
		log.Printf("warning: paused git am session has no saved Grit configuration; source ancestry was verified, but source prefix and rules cannot be checked")
	}

	authorEnv, err := source.authorEnvForCommit(sourceID)
	if err != nil {
		return err
	}

	markerHead := head
	hasMarker := false
	if head != originalHead {
		hasMarker, err = r.emptyAnchorAtHead(head, sourceID, originalHead)
		if err != nil {
			return err
		}
		if !hasMarker {
			return fmt.Errorf("cannot record an empty resolution: HEAD moved during the paused git am session (started at %s, now %s)", originalHead, head)
		}
	}
	if !hasMarker {
		commit, err := r.gitEnv(authorEnv, message, "commit-tree", indexTreeID, "-p", head)
		if err != nil {
			return fmt.Errorf("create empty synchronization commit: %w", err)
		}
		markerHead = strings.TrimSpace(string(commit))
		if markerHead == "" {
			return fmt.Errorf("create empty synchronization commit returned no commit id")
		}
		parent, err := r.RevParse(markerHead + "^")
		if err != nil {
			return fmt.Errorf("verify empty synchronization commit parent: %w", err)
		}
		if parent != head {
			return fmt.Errorf("empty synchronization commit parent is %s, want %s", parent, head)
		}
		empty, err := r.PatchIsEmpty(markerHead)
		if err != nil {
			return fmt.Errorf("verify empty synchronization commit: %w", err)
		}
		if !empty {
			return fmt.Errorf("synchronization commit %s unexpectedly changes files", markerHead)
		}
		if _, err := r.git(nil, "update-ref", "-m", "grit: record empty patch resolution "+sourceID, "HEAD", markerHead, head); err != nil {
			return fmt.Errorf("advance HEAD to empty synchronization commit: %w", err)
		}
	}

	if _, err := r.git(nil, "am", "--skip"); err != nil {
		return fmt.Errorf("clear paused git am session after recording empty commit: %w", err)
	}
	inProgress, err = r.InProgressAM()
	if err != nil {
		return err
	}
	if inProgress {
		return fmt.Errorf("git am session remains in progress after skipping its recorded empty patch")
	}
	headAfter, err := r.Head()
	if err != nil {
		return err
	}
	if headAfter != markerHead {
		return fmt.Errorf("git am skip moved HEAD to %s after recording empty commit %s", headAfter, markerHead)
	}
	return nil
}

// RecordPausedAMContext binds a newly paused patch to the invocation that
// produced it, so a later command cannot resolve it under different sources,
// prefixes, rules, or linearization settings.
func (r *Repo) RecordPausedAMContext(contextID string) error {
	if err := validatePausedAMContextID(contextID); err != nil {
		return err
	}
	inProgress, err := r.InProgressAM()
	if err != nil {
		return err
	}
	if !inProgress {
		return errors.New("cannot record Grit context without a paused git am session")
	}
	return r.writeAMStateFile(pausedAMContextFile, []byte(contextID+"\n"))
}

func validatePausedAMContextID(contextID string) error {
	if len(contextID) != hex.EncodedLen(32) {
		return fmt.Errorf("invalid paused Grit context id length %d", len(contextID))
	}
	if _, err := hex.DecodeString(contextID); err != nil {
		return fmt.Errorf("invalid paused Grit context id: %w", err)
	}
	return nil
}

func (r *Repo) authorEnvForCommit(commitID string) ([]string, error) {
	authorData, err := r.git(nil, "show", "--no-show-signature", "-s", "--format=%an%x00%ae%x00%aD", commitID)
	if err != nil {
		return nil, fmt.Errorf("read source commit author %s: %w", commitID, err)
	}
	authorData = bytes.TrimSuffix(authorData, []byte{'\n'})
	authorFields := bytes.Split(authorData, []byte{0})
	if len(authorFields) != 3 {
		return nil, fmt.Errorf("malformed source author metadata for %s", commitID)
	}
	return []string{
		"GIT_AUTHOR_NAME=" + string(authorFields[0]),
		"GIT_AUTHOR_EMAIL=" + string(authorFields[1]),
		"GIT_AUTHOR_DATE=" + string(authorFields[2]),
	}, nil
}

func (r *Repo) emptyAnchorAtHead(head, sourceID, expectedParent string) (bool, error) {
	commits, err := r.LogIgnoringPrefix("-1", head)
	if err != nil {
		return false, err
	}
	if len(commits) != 1 {
		return false, fmt.Errorf("log -1 %s returned %d commits", head, len(commits))
	}
	ids := commits[0].OwnLineGritTagIDs()
	if len(ids) != 1 || ids[0] != sourceID {
		return false, nil
	}
	parent, err := r.RevParse(head + "^")
	if err != nil {
		return false, err
	}
	if parent != expectedParent {
		return false, nil
	}
	return r.PatchIsEmpty(head)
}

func (r *Repo) writeAMStateFile(name string, contents []byte) error {
	gitPath, err := r.git(nil, "rev-parse", "--git-path", filepath.ToSlash(filepath.Join("rebase-apply", name)))
	if err != nil {
		return err
	}
	path := strings.TrimSuffix(string(gitPath), "\n")
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.root, path)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	gitDir, err := r.git(nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	resolvedGitDir, err := filepath.EvalSymlinks(strings.TrimSuffix(string(gitDir), "\n"))
	if err != nil {
		return err
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(resolvedGitDir, resolvedParent)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("git am metadata %q is outside the repository metadata directory", name)
	}
	return writeAMStateFileAtomically(path, contents)
}

func writeAMStateFileAtomically(path string, contents []byte) error {
	return writeAMStateFileAtomicallyWith(path, contents, func(writer io.Writer, contents []byte) (int, error) {
		return writer.Write(contents)
	})
}

func writeAMStateFileAtomicallyWith(path string, contents []byte, write func(io.Writer, []byte) (int, error)) (retErr error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".grit-am-state-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	closed := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, file.Close())
		}
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("remove temporary Grit metadata file: %w", err))
		}
	}()

	n, err := write(file, contents)
	if err != nil {
		return fmt.Errorf("write temporary Grit metadata file: %w", err)
	}
	if n != len(contents) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temporary Grit metadata file: %w", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary Grit metadata file: %w", err)
	}
	closed = true
	// Rename atomically replaces any existing state left by a prior pause;
	// Link would fail with EEXIST there instead of recording the new pause.
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return nil
}

func (r *Repo) amStateFile(name string) ([]byte, error) {
	gitPath, err := r.git(nil, "rev-parse", "--git-path", filepath.ToSlash(filepath.Join("rebase-apply", name)))
	if err != nil {
		return nil, err
	}
	path := strings.TrimSuffix(string(gitPath), "\n")
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.root, path)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	gitDir, err := r.git(nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	resolvedGitDir, err := filepath.EvalSymlinks(strings.TrimSuffix(string(gitDir), "\n"))
	if err != nil {
		return nil, err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(resolvedGitDir, resolvedPath)
	if err != nil {
		return nil, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("git am metadata %q is outside the repository metadata directory", name)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("git am metadata %q is not a regular file", name)
	}
	return os.ReadFile(path)
}
