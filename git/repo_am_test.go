package git

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthorEnvForCommitSuppressesSignatureOutput(t *testing.T) {
	sshKeygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is unavailable")
	}

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "signing-key")
	cmd := exec.Command(sshKeygen, "-q", "-t", "ed25519", "-N", "", "-f", keyPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot generate SSH signing key: %v\n%s", err, output)
	}
	publicKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(publicKey))
	if len(fields) < 2 {
		t.Fatalf("malformed SSH public key: %q", publicKey)
	}
	allowedSigners := filepath.Join(dir, "allowed-signers")
	if err := os.WriteFile(allowedSigners, []byte("author@example.com "+fields[0]+" "+fields[1]+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	runGit := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return output
	}
	runGit("init")
	runGit(
		"-c", "user.name=Actual Author",
		"-c", "user.email=author@example.com",
		"-c", "gpg.format=ssh",
		"-c", "user.signingkey="+keyPath,
		"-c", "commit.gpgsign=true",
		"commit", "--allow-empty", "-m", "signed source commit",
	)
	commitID := strings.TrimSpace(string(runGit("rev-parse", "HEAD")))
	repo := &Repo{
		root: dir,
		config: map[string]string{
			"log.showSignature":          "true",
			"gpg.format":                 "ssh",
			"gpg.ssh.allowedSignersFile": allowedSigners,
		},
	}

	legacyOutput, err := repo.git(nil, "show", "-s", "--format=%an%x00%ae%x00%aD", commitID)
	if err != nil {
		t.Fatalf("show signed source commit with signature display: %v", err)
	}
	legacyFields := bytes.Split(bytes.TrimSuffix(legacyOutput, []byte{'\n'}), []byte{0})
	if len(legacyFields) != 3 || !bytes.Contains(bytes.ToLower(legacyFields[0]), []byte("signature")) {
		t.Fatalf("expected signature display to corrupt the first author field, got %q", legacyOutput)
	}

	authorEnv, err := repo.authorEnvForCommit(commitID)
	if err != nil {
		t.Fatalf("read source author with signature display disabled: %v", err)
	}
	if len(authorEnv) != 3 ||
		authorEnv[0] != "GIT_AUTHOR_NAME=Actual Author" ||
		authorEnv[1] != "GIT_AUTHOR_EMAIL=author@example.com" {
		t.Fatalf("author metadata = %v", authorEnv)
	}
}

func TestWriteAMStateFileAtomicallyCleansUpFailedWrite(t *testing.T) {
	injectedErr := errors.New("injected write failure")
	tests := []struct {
		name  string
		write func(io.Writer, []byte) (int, error)
		want  error
	}{
		{
			name: "write error after partial data",
			write: func(writer io.Writer, contents []byte) (int, error) {
				n, err := writer.Write(contents[:len(contents)/2])
				if err != nil {
					return n, err
				}
				return n, injectedErr
			},
			want: injectedErr,
		},
		{
			name: "short write",
			write: func(writer io.Writer, contents []byte) (int, error) {
				return writer.Write(contents[:len(contents)/2])
			},
			want: io.ErrShortWrite,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, pausedAMContextFile)
			err := writeAMStateFileAtomicallyWith(path, []byte("complete context"), test.write)
			if !errors.Is(err, test.want) {
				t.Fatalf("writeAMStateFileAtomicallyWith() error = %v, want %v", err, test.want)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed write left final metadata file: stat error = %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("failed write left temporary files: %v", entries)
			}
		})
	}
}

func TestWriteAMStateFileAtomicallyReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, pausedAMContextFile)
	if err := os.WriteFile(path, []byte("stale context"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeAMStateFileAtomically(path, []byte("fresh context")); err != nil {
		t.Fatalf("re-record over existing state: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh context" {
		t.Fatalf("state = %q, want %q", got, "fresh context")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}
