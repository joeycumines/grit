// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.
package git

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grailbio/testutil"
)

var (
	nocleanup  = flag.Bool("nocleanup", false, "don't clean up git state after tests are run")
	shelltrace = flag.Bool("shelltrace", false, "trace shell execution")
)

func TestLog(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	if *nocleanup {
		log.Println("directory", dir)
	} else {
		defer cleanup()
	}
	shell(t, dir, `
		git init --bare repo
		git clone repo checkout
		cd checkout
		git config user.email you@example.com
		git config user.name "your name"
		mkdir adir
		echo test file > adir/file1
		echo test file > file1
		git add .
		git commit -m'first commit'
		echo ok > file2
		git add .
		git commit -m'second commit'
		git push
	`)
	repo, err := Open(filepath.Join(dir, "repo"), "adir/", "master")
	if err != nil {
		t.Fatal(err)
	}
	commits, err := repo.Log()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(commits), 1; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	c := commits[0]
	if got, want := c.Title(), "first commit"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	patch, err := repo.Patch(c.Digest, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := patch.Subject, "[PATCH] first commit"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, want := patch.Author, `your name <you@example.com>`; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, want := len(patch.Diffs), 1; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	diff := patch.Diffs[0]
	if got, want := diff.Path, "file1"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	if !bytes.HasPrefix(diff.Meta, []byte("new file mode 100644\nindex 0000000")) {
		t.Errorf("bad diff meta %s", diff.Meta)
	}
	if !bytes.HasSuffix(diff.Meta, []byte("--- /dev/null\n+++ b/file1")) {
		t.Errorf("bad diff meta %s", diff.Meta)
	}
	if got, want := string(diff.Body), `@@ -0,0 +1 @@
+test file`; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPatchApply(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	if *nocleanup {
		log.Println("directory", dir)
	} else {
		defer cleanup()
	}
	shell(t, dir, `
		mkdir repos

		# Set up source repository and add a couple of commits:
		# - add a file to dir1
		# - move this file to dir2
		git init --bare repos/src
		git clone repos/src src
		cd src
		git config user.email you@example.com
		git config user.name "your name"
		mkdir dir1
		echo "test file" > dir1/file1
		git add dir1
		git commit -m'first commit'
		mkdir dir2
		git mv dir1/file1 dir2
		git commit -m'second commit'
		git push

		cd ..

		# Set up second, empty repository. Note that grit cannot
		# initialize empty repositories, so we add a first commit.
		git init --bare repos/dst
		git clone repos/dst dst
		cd dst
		git config user.email you@example.com
		git config user.name "your name"
		echo license > LICENSE
		git add .
		git commit -m'first commit'
		git push
	`)
	src, err := Open(filepath.Join(dir, "repos/src"), "dir2/", "master")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := Open(filepath.Join(dir, "repos/dst"), "", "master")
	if err != nil {
		t.Fatal(err)
	}
	// Needs to be configured for committer.
	dst.Configure("user.email", "committer@grailbio.com")
	dst.Configure("user.name", "committer")
	commits, err := src.Log()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(commits), 1; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	patch, err := src.Patch(commits[0].Digest, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(patch.Diffs) == 0 {
		t.Fatal("empty patch")
	}
	if err := dst.Apply(patch); err != nil {
		t.Fatalf("failed to apply patch: %v\n%s", err, patch.Patch())
	}
	if err := dst.Push("origin", "master"); err != nil {
		t.Fatal(err)
	}
	// Make sure the file is actually there.
	shell(t, dir, `
		git -C dst pull
		cmp src/dir2/file1 dst/file1 || error file1
	`)
}

// TestPrefixPatchApply verifies that applying patches to a destination with a
// prefix behaves correctly.
func TestPrefixPatchApply(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	if *nocleanup {
		log.Println("directory", dir)
	} else {
		defer cleanup()
	}
	shell(t, dir, `
		mkdir repos

		# Set up source repository and add a couple of commits:
		# - add a file file1
		# - add a file dir2/file2
		git init --bare repos/src
		git clone repos/src src
		cd src
		git config user.email you@example.com
		git config user.name "your name"
		echo "test file 1" > file1
		git add file1
		git commit -m'first commit'
		mkdir dir2
		echo "test file 2" > dir2/file2
		git add dir2
		git commit -m'second commit'
		git push

		cd ..

		# Set up second, empty repository. Note that grit cannot
		# initialize empty repositories, so we add a first commit.
		git init --bare repos/dst
		git clone repos/dst dst
		cd dst
		git config user.email you@example.com
		git config user.name "your name"
		mkdir pfx
		touch pfx/.gitignore
		git add .
		git commit -m'first commit'
		git push
	`)
	src, err := Open(filepath.Join(dir, "repos/src"), "", "master")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := Open(filepath.Join(dir, "repos/dst"), "pfx/", "master")
	if err != nil {
		t.Fatal(err)
	}
	// Needs to be configured for committer.
	dst.Configure("user.email", "committer@grailbio.com")
	dst.Configure("user.name", "committer")
	commits, err := src.Log()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(commits), 2; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := len(commits) - 1; i >= 0; i-- {
		patch, err := src.Patch(commits[i].Digest, "pfx/")
		if err != nil {
			t.Fatal(err)
		}
		if len(patch.Diffs) == 0 {
			t.Fatal("empty patch")
		}
		log.Printf("path:%v", patch.Diffs[0].Path)
		if err := dst.Apply(patch); err != nil {
			t.Fatalf("failed to apply patch: %v\n%s", err, patch.Patch())
		}
	}
	if err := dst.Push("origin", "master"); err != nil {
		t.Fatal(err)
	}
	// Make sure the file is actually there.
	shell(t, dir, `
		git -C dst pull
		cmp src/file1 dst/pfx/file1 || error file1
		cmp src/dir2/file2 dst/pfx/dir2/file2 || error file2
	`)
}

func TestLFS(t *testing.T) {
	_, err := exec.LookPath("lfs-test-server")
	if err != nil {
		t.Skip("lfs-test-server not installed")
	}
	dir, cleanup := testutil.TempDir(t, "", "")
	if *nocleanup {
		log.Println("directory", dir)
	} else {
		defer cleanup()
	}

	// Reserve a kernel-assigned port so concurrent test runs cannot
	// collide on a fixed lfs-test-server endpoint.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Wait()
	defer cancel()
	go func() {
		cmd := exec.CommandContext(ctx, "lfs-test-server")
		cmd.Env = []string{
			"LFS_ADMINUSER=user",
			"LFS_ADMINPASS=pass",
			"LFS_CONTENTPATH=" + dir,
			// LFS_LISTEN binds the listener; LFS_HOST is what the
			// server embeds into the upload hrefs it hands back, and
			// defaults to localhost:8080 without it.
			"LFS_LISTEN=tcp://127.0.0.1:" + strconv.Itoa(port),
			"LFS_HOST=127.0.0.1:" + strconv.Itoa(port),
		}
		err := cmd.Run()
		if err != nil && err != context.Canceled && !strings.HasSuffix(err.Error(), "signal: killed") {
			log.Panicf("lfs-test-server: %v", err)
		}
		wg.Done()
	}()
	waitForListener(t, port)

	shell(t, dir, fmt.Sprintf(`
		mkdir repos

		git init --bare repos/src
		git clone repos/src src
		cd src
		git config user.email you@example.com
		git config user.name "your name"
		git lfs install
		git config -f .lfsconfig lfs.url http://user:pass@127.0.0.1:%d
		git config lfs.url http://user:pass@127.0.0.1:%d
		git add .lfsconfig
		git commit -a -m "lfsconfig"`, port, port)+`
		echo bigfile >bigfile
		git lfs track bigfile
		git add .
		git commit -a -m "big file"
		git push

		cd ..
		# Create the destination repository. Note that we don't install
		# LFS hooks and instead maintain the pointers manually.
		git init --bare repos/dst
		git clone repos/dst dst
		cd dst
		git config user.email you@example.com
		git config user.name "your name"
		# Manually install the pointer for 'bigfile' into this repository.
		git -C ../src show HEAD:bigfile > bigfile
		git add bigfile
		git commit -m'first commit'
		git push
	`)

	src, err := Open(filepath.Join(dir, "repos/src"), "", "master")
	if err != nil {
		t.Fatal(err)
	}
	ptrs, err := src.ListLFSPointers()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(ptrs), 1; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := ptrs[0], "bigfile"; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}

	dst, err := Open(filepath.Join(dir, "repos/dst"), "", "master")
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.CopyLFSObject(src, ptrs[0]); err != nil {
		t.Fatal(err)
	}
}

func TestResetToRemoteUsesOriginHead(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	if *nocleanup {
		log.Println("directory", dir)
	} else {
		defer cleanup()
	}
	shell(t, dir, `
		git init --bare dst.git
		git clone dst.git dst
		cd dst
		git config user.email you@example.com
		git config user.name "your name"
		echo dst1 > file.txt
		git add file.txt
		git commit -m'dst1'
		git push origin master
		cd ..
		git init --bare src.git
		git clone src.git src
		cd src
		git config user.email you@example.com
		git config user.name "your name"
		echo src1 > file.txt
		git add file.txt
		git commit -m'src1'
		git push origin master
	`)
	dstRepo, err := Open(filepath.Join(dir, "dst.git"), "", "master")
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dstRepo.Close()
	originHead := dstRepo.OriginHead()
	if originHead == "" {
		t.Fatalf("originHead empty")
	}
	shell(t, dir, `
		git -C `+dstRepo.RepoRoot()+` config user.email you@example.com
		git -C `+dstRepo.RepoRoot()+` config user.name "your name"
		echo local > `+dstRepo.RepoRoot()+`/local.txt
		git -C `+dstRepo.RepoRoot()+` add local.txt
		git -C `+dstRepo.RepoRoot()+` commit -m'local dst commit'
	`)
	if err := dstRepo.FetchObjects(filepath.Join(dir, "src.git"), "master"); err != nil {
		t.Fatalf("FetchObjects: %v", err)
	}
	// FetchObjects deliberately clobbers FETCH_HEAD with the source tip
	// (no --no-write-fetch-head: that flag needs git >= 2.29 and nothing
	// reads FETCH_HEAD after open captures the snapshot). The invariant
	// under test is that ResetToRemote ignores the clobbered file and
	// restores the captured destination tip.
	fetchHead, err := dstRepo.RevParse("FETCH_HEAD")
	if err != nil {
		t.Fatalf("FETCH_HEAD: %v", err)
	}
	srcHead, err := dstRepo.RevParse("refs/grit/src")
	if err != nil {
		t.Fatalf("refs/grit/src: %v", err)
	}
	if fetchHead != srcHead {
		t.Fatalf("test setup drifted: FETCH_HEAD %s != seeded source tip %s", fetchHead, srcHead)
	}
	if err := dstRepo.ResetToRemote(); err != nil {
		t.Fatalf("ResetToRemote: %v", err)
	}
	head, err := dstRepo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head != originHead {
		t.Fatalf("ResetToRemote reset to %s (FETCH_HEAD holds source tip %s), want originHead %s", head, fetchHead, originHead)
	}
	if head == srcHead {
		t.Fatalf("ResetToRemote incorrectly reset to source tip %s", srcHead)
	}
	if _, err := os.Stat(filepath.Join(dstRepo.RepoRoot(), "local.txt")); !os.IsNotExist(err) {
		t.Fatalf("local.txt still exists after reset, should have been discarded")
	}
}

func TestPatchIsEmptyWithDiffStringInMessage(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	if *nocleanup {
		log.Println("directory", dir)
	} else {
		defer cleanup()
	}
	shell(t, dir, `
		git init --bare repo.git
		git clone repo.git checkout
		cd checkout
		git config user.email you@example.com
		git config user.name "your name"
		echo hello > file.txt
		git add file.txt
		git commit -m'first'
		git push
		cd ..
		git clone repo.git checkout2
		cd checkout2
		git config user.email you@example.com
		git config user.name "your name"
		git commit --allow-empty -m'empty commit with diff --git in body

This message mentions diff --git a/foo b/foo inside prose
and also has --- marker'
		git push
	`)
	repo, err := Open(filepath.Join(dir, "checkout2"), "", "master")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	commits, err := repo.LogIgnoringPrefix("--all")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	var emptyID string
	for _, c := range commits {
		if c.Title() == "empty commit with diff --git in body" {
			emptyID = c.Digest.Hex()
			break
		}
	}
	if emptyID == "" {
		t.Fatalf("empty commit not found")
	}
	empty, err := repo.PatchIsEmpty(emptyID)
	if err != nil {
		t.Fatalf("PatchIsEmpty: %v", err)
	}
	if !empty {
		t.Fatalf("PatchIsEmpty returned false for empty commit containing diff --git in message, want true")
	}
	var nonEmptyID string
	for _, c := range commits {
		if c.Title() == "first" {
			nonEmptyID = c.Digest.Hex()
			break
		}
	}
	nonEmpty, err := repo.PatchIsEmpty(nonEmptyID)
	if err != nil {
		t.Fatalf("PatchIsEmpty non-empty: %v", err)
	}
	if nonEmpty {
		t.Fatalf("PatchIsEmpty returned true for non-empty commit, want false")
	}
}

func TestOwnLineConvergenceMarkers(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "plain",
			body: "subject\n\ngrit-convergence-pruned: 1/2\n",
			want: []string{"1/2"},
		},
		{
			name: "crlf terminated",
			body: "subject\n\ngrit-convergence-pruned: 12/34\r\n",
			want: []string{"12/34"},
		},
		{
			name: "multiple mixed endings",
			body: "grit-convergence-pruned: 1/2\r\nprose\ngrit-convergence-pruned: 3/4\n",
			want: []string{"1/2", "3/4"},
		},
		{
			name: "indented quotation is not own-line",
			body: "  grit-convergence-pruned: 9/9\n",
			want: nil,
		},
		{
			name: "no markers",
			body: "plain body\n",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Commit{Body: tc.body}
			got := c.OwnLineConvergenceMarkers()
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestPathInPrefix(t *testing.T) {
	for _, tc := range []struct {
		p, prefix string
		want      bool
	}{
		{"docs.txt", "docs", false},
		{"docs/x", "docs", true},
		{"docs", "docs", true},
		{"docs/", "docs/", true},
		{"docs/x", "docs/", true},
		{"docs.txt", "docs/", false},
		{"docsx/y", "docs/", false},
		{"adir/file1", "adir/", true},
		{"other/file1", "adir/", false},
		{"anything", "", true},
	} {
		if got := pathInPrefix(tc.p, tc.prefix); got != tc.want {
			t.Errorf("pathInPrefix(%q, %q) = %v, want %v", tc.p, tc.prefix, got, tc.want)
		}
	}
}

func TestIsCaseMismatch(t *testing.T) {
	for _, tc := range []struct {
		diffPath, prefix string
		want             bool
	}{
		{"Foo/bar", "foo", true},
		{"foo/bar", "foo", false},
		{"foobar", "foo", false},
		{"Foobar", "foo", false},
		{"Foo", "foo", true},
		{"foo", "foo", false},
		{"FOO", "foo", true},
		{"Foo/bar", "foo/", true},
		{"foo/bar", "foo/", false},
		{"foobar", "foo/", false},
		{"ς/x", "Σ/", true},
		{"σ/x", "Σ/", true},
		{"Σ/x", "Σ/", false},
		{"Föo/x", "föo", true},
		{"föo/x", "föo", false},
		{"föobar", "föo", false},
		{"short", "longer-prefix", false},
		{"", "foo", false},
		{"foo", "", false},
	} {
		if got := isCaseMismatch(tc.diffPath, tc.prefix); got != tc.want {
			t.Errorf("isCaseMismatch(%q, %q) = %v, want %v", tc.diffPath, tc.prefix, got, tc.want)
		}
	}
}

// TestPatchIsEmptyWhitespacePathname pins that a commit whose only
// change is a file named entirely of whitespace is not misclassified
// as empty: git permits such names, and trimming diff-tree's raw
// path output erased the only evidence of the change.
func TestPatchIsEmptyWhitespacePathname(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	if *nocleanup {
		log.Println("directory", dir)
	} else {
		defer cleanup()
	}
	shell(t, dir, `
		git init --bare repo.git
		git clone repo.git checkout
		cd checkout
		git config user.email you@example.com
		git config user.name "your name"
		echo hello > file.txt
		git add file.txt
		git commit -m'first'
		git push
		cd ..
		git clone repo.git checkout2
		cd checkout2
		git config user.email you@example.com
		git config user.name "your name"
		echo spaced > ' '
		git add ' '
		git commit -m'whitespace pathname change'
		git push
	`)
	repo, err := Open(filepath.Join(dir, "checkout2"), "", "master")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	commits, err := repo.LogIgnoringPrefix("--all")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	var wsID string
	for _, c := range commits {
		if c.Title() == "whitespace pathname change" {
			wsID = c.Digest.Hex()
			break
		}
	}
	if wsID == "" {
		t.Fatalf("whitespace-pathname commit not found")
	}
	empty, err := repo.PatchIsEmpty(wsID)
	if err != nil {
		t.Fatalf("PatchIsEmpty: %v", err)
	}
	if empty {
		t.Fatalf("PatchIsEmpty returned true for commit changing file named ' ', want false")
	}
}

const gritAuthoredMessage = "resolved session\n\nfbshipit-source-id: 0123456789abcdef0123456789abcdef01234567\n"

// TestPreservedTailRequiresRemoteAncestry pins the preservation gate's
// scope: an unpushed tail qualifies for preservation only when it
// descends from the provided remote tip. A diverged tail (sharing only
// a deeper merge base) can never be pushed — every push fails
// non-fast-forward — so it must report unpreservable and let the caller
// reclone, instead of deferring the loss to a doomed push cycle.
func TestPreservedTailRequiresRemoteAncestry(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	if *nocleanup {
		log.Println("directory", dir)
	} else {
		defer cleanup()
	}
	shell(t, dir, `
		git init --bare repo.git
		git clone repo.git w1
		cd w1
		git config user.email you@example.com
		git config user.name "your name"
		echo base > f.txt
		git add f.txt
		git commit -m'base'
		git push
		cd ..
		git clone repo.git w2
		cd w2
		git config user.email you@example.com
		git config user.name "your name"
	`)
	base, err := exec.Command("git", "-C", filepath.Join(dir, "w2"), "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse base: %v\n%s", err, base)
	}
	baseHex := strings.TrimSpace(string(base))

	// Local grit-authored commit: strictly ahead of the remote tip.
	shell(t, dir, `
		cd `+filepath.Join(dir, "w2")+`
		echo local > resolved.txt
		git add resolved.txt
		git commit -m'`+gritAuthoredMessage+`'
	`)

	// Independent remote advance: the histories are now diverged.
	shell(t, dir, `
		cd `+filepath.Join(dir, "w1")+`
		echo r1 > advanced.txt
		git add advanced.txt
		git commit -m'remote advance'
		git push
	`)

	r := &Repo{url: filepath.Join(dir, "repo.git"), root: filepath.Join(dir, "w2"), prefix: "", branch: "master"}

	authored, err := r.PreservedTailIsGritAuthored(baseHex)
	if err != nil {
		t.Fatalf("strictly-ahead: %v", err)
	}
	if !authored {
		t.Fatal("strictly-ahead grit-authored tail must be preserved")
	}

	out, err := exec.Command("git", "-C", filepath.Join(dir, "w1"), "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse advanced: %v\n%s", err, out)
	}
	divergedHex := strings.TrimSpace(string(out))
	// Mirror openLocked, which always fetches before the gate runs:
	// without the fetch the advanced tip is unknown here and
	// merge-base would fail for missing-object reasons instead of
	// answering the ancestry question.
	shell(t, dir, `
		git -C `+filepath.Join(dir, "w2")+` fetch -q origin
	`)
	authored, err = r.PreservedTailIsGritAuthored(divergedHex)
	if err != nil {
		t.Fatalf("diverged: %v", err)
	}
	if authored {
		t.Fatal("diverged tail (merge base below remote tip) must not be preserved; a later push could never succeed")
	}

	// Unrelated history: nothing local descends from such a tip either.
	shell(t, dir, `
		git init --bare unrelated.git
		git clone unrelated.git u
		cd u
		git config user.email you@example.com
		git config user.name "your name"
		echo other > other.txt
		git add other.txt
		git commit -m'unrelated root'
	`)
	out, err = exec.Command("git", "-C", filepath.Join(dir, "u"), "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse unrelated: %v\n%s", err, out)
	}
	unrelatedHex := strings.TrimSpace(string(out))
	authored, err = r.PreservedTailIsGritAuthored(unrelatedHex)
	if err != nil {
		t.Fatalf("unrelated: %v", err)
	}
	if authored {
		t.Fatal("unrelated history must not be preserved")
	}
}

// TestParseCommitsMalformedHeader pins that a git-log stream carrying a
// header line without a colon produces a wrapped error instead of an
// index-out-of-range panic.
func TestParseCommitsMalformedHeader(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	t.Cleanup(func() {
		if !*nocleanup {
			cleanup()
		}
	})
	shell(t, dir, `
		git init -q --bare origin.git
		git clone -q origin.git master
		cd master
		git config user.email you@example.com
		git config user.name "your name"
		echo x > x.txt
		git add .
		git commit -qm one
	`)
	r := &Repo{url: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "master"),
		prefix: "", branch: "master"}

	stream := []byte("commit 0123456789abcdef0123456789abcdef01234567\nnot-a-header-line\n\n    body\n")
	if _, err := parseCommits(r, stream); err == nil {
		t.Fatal("malformed commit header parsed without error")
	} else if !strings.Contains(err.Error(), "malformed commit header") {
		t.Fatalf("error %v does not identify the malformed header", err)
	}
}

// waitForListener blocks until the TCP port accepts connections, so the
// fixture's first LFS request never races the server's bind. An
// initial refusal makes git-lfs fall back to its compiled-in default
// endpoint, which silently targets the wrong server.
func waitForListener(t *testing.T, port int) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lfs-test-server never listened on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func shell(t *testing.T, dir, script string) {
	t.Helper()
	cmd := exec.Command("bash", "-e", "-x")
	cmd.Dir = dir
	// Pin the initial branch for test repositories (git >= 2.28), so that
	// tests pass regardless of the user's init.defaultBranch configuration:
	// grit and these tests assume "master".
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=init.defaultBranch",
		"GIT_CONFIG_VALUE_0=master",
	)
	script = `
		function error {
			echo "$@" 1>&2
			exit 1
		}
	` + script
	cmd.Stdin = strings.NewReader(script)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if *shelltrace {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Run(); err != nil {
		if *shelltrace {
			t.Fatal("script failed")
		}
		t.Fatalf("script failed: %v\n%s", err, stderr.String())
	}
	t.Log(stderr.String())
}

func TestHasCommonAncestorAndRevListExcluding(t *testing.T) {
	dir, cleanup := testutil.TempDir(t, "", "")
	if *nocleanup {
		log.Println("directory", dir)
	} else {
		defer cleanup()
	}
	shell(t, dir, `
		git init --bare repo
		git clone repo checkout
		cd checkout
		git config user.email you@example.com
		git config user.name "your name"
		echo base > base.txt
		git add .
		git commit -m'base'
		git checkout --orphan foreign
		git rm -rf .
		echo legacy > legacy.txt
		git add .
		git commit -m'foreign root'
		git checkout master
		git merge --allow-unrelated-histories --no-ff -m'merge foreign' foreign
		echo more > more.txt
		git add .
		git commit -m'mainline child'
		git push
	`)
	repo, err := Open(filepath.Join(dir, "repo"), "", "master")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	base, err := repo.RevParse("master~2")
	if err != nil {
		t.Fatal(err)
	}
	foreignTip, err := repo.RevParse("master^^2")
	if err != nil {
		t.Fatal(err)
	}
	tip, err := repo.RevParse("master")
	if err != nil {
		t.Fatal(err)
	}

	shared, err := repo.HasCommonAncestor(base, tip)
	if err != nil || !shared {
		t.Fatalf("HasCommonAncestor(base, master) = %v, %v; want true", shared, err)
	}
	disjoint, err := repo.HasCommonAncestor(base, foreignTip)
	if err != nil || disjoint {
		t.Fatalf("HasCommonAncestor(base, foreign tip) = %v, %v; want false", disjoint, err)
	}
	self, err := repo.HasCommonAncestor(foreignTip, foreignTip)
	if err != nil || !self {
		t.Fatalf("HasCommonAncestor(foreign tip, itself) = %v, %v; want true", self, err)
	}

	lineage, err := repo.RevListExcluding(foreignTip, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(lineage) != 1 || lineage[0] != foreignTip {
		t.Fatalf("RevListExcluding(foreign tip, base) = %v; want [%s]", lineage, foreignTip)
	}
	none, err := repo.RevListExcluding(base, base)
	if err != nil || len(none) != 0 {
		t.Fatalf("RevListExcluding(base, base) = %v, %v; want empty", none, err)
	}
	mixed, err := repo.RevListExcluding(tip, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(mixed) != 3 {
		t.Fatalf("RevListExcluding(master, base) returned %d commits; want 3: %v", len(mixed), mixed)
	}
}
