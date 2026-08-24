// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.
package git

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParsePatch(t *testing.T) {
	patch := parsePatchRoundTrip(t, "testdata/0001-reflow-syntax-permit-file-and-dir-module-arguments-v.patch")
	if got, want := patch.ID.Hex(), "b969e1d8eb27e72eee131c1d31398fc3e6ef9c25"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, want := patch.Author, `"marius a. eriksen" <marius@grailbio.com>`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := patch.Time.Format(time.Kitchen), "11:44AM"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestParsePatchInvalidEmail verifies that we can parse patches with invalid
// email addresses, as these can be written by `git format-patch`.
func TestParsePatchInvalidEmail(t *testing.T) {
	// This patch has an email address with the '[' character, which is invalid.
	// See https://tools.ietf.org/html/rfc5322#section-3.2.3.
	patch := parsePatchRoundTrip(t, "testdata/0001-build-deps-bump-activesupport-from-6.0.2.1-to-6.0.3..patch")
	if got, want := patch.Author, `"dependabot[bot]" <49699333+dependabot[bot]@users.noreply.github.com>`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNewModeCRLFTrim(t *testing.T) {
	d := Diff{Meta: []byte("index abc123..def456 100644\r")}
	mode, ok := d.NewMode()
	if !ok || mode != "100644" {
		t.Fatalf("NewMode CRLF not trimmed: got %q ok=%v want %q", mode, ok, "100644")
	}
}

func TestNewBlobCRLFTrim(t *testing.T) {
	d := Diff{Meta: []byte("index abc123..def456\r")}
	blob, ok := d.NewBlob()
	if !ok || blob != "def456" {
		t.Fatalf("NewBlob CRLF not trimmed: got %q ok=%v want %q", blob, ok, "def456")
	}
}

func TestNewModeDeletedFile(t *testing.T) {
	d := Diff{Meta: []byte("deleted file mode 100644\nindex abc123..0000000 100644")}
	mode, ok := d.NewMode()
	if ok {
		t.Fatalf("NewMode for deleted file should return !ok, got %q", mode)
	}
}

func TestParseDiffHeaderCRLF(t *testing.T) {
	// Symmetric unquoted form, CRLF-terminated.
	got := parseDiffHeader([]byte("diff --git a/foo/bar b/foo/bar\r"))
	if string(got) != "foo/bar" {
		t.Fatalf("symmetric CRLF header parsed %q, want foo/bar", got)
	}
	// C-quoted form (tab escaped as git emits it), CRLF-terminated.
	quoted := []byte("diff --git \"a/sp\\tace\" \"b/sp\\tace\"\r")
	got = parseDiffHeader(quoted)
	if string(got) != "sp\tace" {
		t.Fatalf("quoted CRLF header parsed %q, want sp\\tace", got)
	}
}

func TestRewriteDiffMeta(t *testing.T) {
	fix := func(p string) string { return "d/" + strings.TrimPrefix(p, "s/") }

	t.Run("lf only", func(t *testing.T) {
		meta := []byte("index abc..def 100644\nold mode 100644\nnew mode 100755\n--- a/s/f.txt\n+++ b/s/f.txt")
		want := "index abc..def 100644\nold mode 100644\nnew mode 100755\n--- a/d/f.txt\n+++ b/d/f.txt"
		if got := rewriteDiffMeta(meta, fix); string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("crlf quoted paths rewritten with fidelity", func(t *testing.T) {
		// Production shape: next() yields meta ending on the final
		// line's carriage return, with hunks excluded.
		meta := []byte("index abc..def 100644\r\n--- \"a/s/t\\tab\"\r\n+++ \"b/s/t\\tab\"\r")
		want := "index abc..def 100644\r\n--- \"a/d/t\\tab\"\r\n+++ \"b/d/t\\tab\"\r"
		if got := rewriteDiffMeta(meta, fix); string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("crlf symmetric paths", func(t *testing.T) {
		meta := []byte("--- a/s/f.txt\r\n+++ b/s/f.txt\r")
		want := "--- a/d/f.txt\r\n+++ b/d/f.txt\r"
		if got := rewriteDiffMeta(meta, fix); string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestQuotePathRoundTrip(t *testing.T) {
	for _, p := range []string{
		"plain.txt",
		"sp ace.txt",
		"tab\tchar",
		`quo"te`,
		`back\slash`,
		"bell\aring\fform\nnew\rret\ttab\vvert",
		"del\x7fbyte",
		"caf\u00e9.txt",      // precomposed UTF-8, octal-encoded per byte
		string([]byte{0x00}), // NUL
		"\x01\x02\x1f control soup",
	} {
		q := quotePath(p)
		if !strings.HasPrefix(q, `"`) || !strings.HasSuffix(q, `"`) {
			t.Fatalf("quotePath(%q) = %q not wrapped in quotes", p, q)
		}
		back, err := gitCUnquote(q)
		if err != nil {
			t.Fatalf("gitCUnquote(%q): %v", q, err)
		}
		if back != p {
			t.Fatalf("round trip of %q: got %q via %q", p, back, q)
		}
	}
}

func TestGitCUnquoteOctalBytes(t *testing.T) {
	// Git emits one 3-digit octal escape per raw byte; Go's Unquote must
	// append each decoded value as a byte, not re-encode it as a rune,
	// or multibyte UTF-8 names would corrupt.
	got, err := gitCUnquote(`"caf\303\251.txt"`)
	if err != nil {
		t.Fatalf("unquote: %v", err)
	}
	want := "caf\u00e9.txt"
	if got != want {
		t.Fatalf("got %q (% x), want %q", got, got, want)
	}
	if len(got) != len(want) {
		t.Fatalf("decoded length %d, want %d (rune re-encoding suspected)", len(got), len(want))
	}
}

func TestHasControlChars(t *testing.T) {
	for _, tc := range []struct {
		p    string
		want bool
	}{
		{"plain.txt", false},
		{"tilde~and\xffhigh", false},
		{"tab\tinside", true},
		{"newline\ninside", true},
		{"carriage\rreturn", true},
		{"del\x7fchar", true},
		{"esc\x1bchar", true},
	} {
		if got := hasControlChars(tc.p); got != tc.want {
			t.Errorf("hasControlChars(%q) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestParseDiffHeaderForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"simple symmetric", "diff --git a/f.txt b/f.txt", "f.txt"},
		{"spaces symmetric", `diff --git a/sp ace b/sp ace`, "sp ace"},
		{"nested symmetric", "diff --git a/d/f b/d/f", "d/f"},
		{"quoted tab", "diff --git \"a/t\\tf\" \"b/t\\tf\"", "t\tf"},
		{"quoted unicode", "diff --git \"a/caf\\303\\251\" \"b/caf\\303\\251\"", "caf\u00e9"},
		{"quoted internal quote", `diff --git "a/q\"f" "b/q\"f"`, `q"f`},
		{"quoted space", `diff --git "a/sp ace" "b/sp ace"`, "sp ace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDiffHeader([]byte(tc.line)); string(got) != tc.want {
				t.Fatalf("parsed %q, want %q", got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		line string
	}{
		{"asymmetric halves", "diff --git a/left b/right"},
		{"missing separator", "diff --git a/onlyone"},
		{"not a header", "+++ b/something"},
		{"empty", ""},
		{"wrong prefix", "diff --git x/f y/f"},
	} {
		t.Run("reject "+tc.name, func(t *testing.T) {
			if got := parseDiffHeader([]byte(tc.line)); got != nil {
				t.Fatalf("expected rejection, got %q", got)
			}
		})
	}
}

// parsePatchRoundTrip parses and returns the patch at path, with a round trip
// through (Patch).Write.
func parsePatchRoundTrip(t *testing.T, path string) Patch {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %q: %v", path, err)
	}
	patch, err := parsePatchHeader(b)
	if err != nil {
		t.Fatalf("failed to parse patch: %v", err)
	}
	var buf bytes.Buffer
	if err := patch.Write(&buf); err != nil {
		t.Fatalf("failed to write to byte buffer: %v", err)
	}
	patch, err = parsePatchHeader(buf.Bytes())
	if err != nil {
		t.Fatalf("failed to parse written patch (roundtrip failed): %v", err)
	}
	return patch
}
