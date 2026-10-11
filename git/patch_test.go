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

	"github.com/grailbio/base/digest"
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

func mustDigest(t *testing.T, hex string) digest.Digest {
	t.Helper()
	d, err := SHA1.Parse(hex)
	if err != nil {
		t.Fatalf("parse digest: %v", err)
	}
	return d
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

func TestWriteEscapesBodyInitialTokens(t *testing.T) {
	for _, body := range []string{
		"diff --git a/x b/x\npayload",
		"--- separator-lookalike\npayload",
		"+++ plus-lookalike\npayload",
	} {
		p := Patch{
			ID:      mustDigest(t, "0123456789abcdef0123456789abcdef01234567"),
			Author:  "t <t@t>",
			Subject: "s",
			Body:    body,
		}
		var b strings.Builder
		if err := p.Write(&b); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		// The body's first line must not survive verbatim as a patch
		// control line; only the envelope's own separator may appear bare.
		first := strings.SplitN(body, "\n", 2)[0]
		if strings.Contains(out, "\n"+first+"\n") && !strings.Contains(first, zeroWidthSpace) {
			t.Fatalf("body-initial %q unescaped in:\n%s", first, out)
		}
	}
}

func TestWriteQuotesBackslashPath(t *testing.T) {
	p := Patch{
		ID:      mustDigest(t, "0123456789abcdef0123456789abcdef01234567"),
		Author:  "t <t@t>",
		Subject: "s",
		Diffs:   []Diff{{Path: `back\slash.txt`, Meta: []byte("index 00..11\n"), Body: []byte("@@\n")}},
	}
	var b strings.Builder
	if err := p.Write(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `diff --git "a/back\\slash.txt" "b/back\\slash.txt"`) {
		t.Fatalf("backslash path not C-quoted:\n%s", b.String())
	}
	if got := parseDiffHeader([]byte(`diff --git "a/back\\slash.txt" "b/back\\slash.txt"`)); string(got) != `back\slash.txt` {
		t.Fatalf("quoted backslash header parsed as %q", got)
	}
}

func TestRewriteDiffMetaPreservesQuotedSuffix(t *testing.T) {
	meta := []byte("--- \"a/sp ace\"\t1200000000 +0000\n+++ \"b/sp ace\"\t1300000000 +0000")
	out := rewriteDiffMeta(meta, func(p string) string { return "d/" + p })
	if !strings.Contains(string(out), "\t1200000000 +0000") || !strings.Contains(string(out), "\t1300000000 +0000") {
		t.Fatalf("quoted-line suffix dropped:\n%s", out)
	}
	if !strings.Contains(string(out), `"a/d/sp ace"`) {
		t.Fatalf("quoted path not relocated:\n%s", out)
	}
}

func TestRewriteDiffMetaQuotePromotesControlChars(t *testing.T) {
	// When the destination prefix injects quoting triggers (tab,
	// newline, quote, backslash, or other control bytes) into a path that
	// arrived unquoted, the rewritten ---/+++ line must be C-quoted;
	// otherwise git rejects the header. Plain rewrites stay unquoted.
	meta := []byte("--- a/f.txt\n+++ b/f.txt")
	out := rewriteDiffMeta(meta, func(p string) string { return "ta\tb/" + p })
	if string(out) != "--- \"a/ta\\tb/f.txt\"\n+++ \"b/ta\\tb/f.txt\"" {
		t.Fatalf("control-char rewrite not quote-promoted:\n%s", out)
	}
	plain := rewriteDiffMeta(meta, func(p string) string { return "d/" + p })
	if string(plain) != "--- a/d/f.txt\n+++ b/d/f.txt" {
		t.Fatalf("plain rewrite changed quoting:\n%s", plain)
	}
}

func TestRewriteDiffMetaPreservesUnquotedSuffix(t *testing.T) {
	// The C-quoted branch preserves tab timestamps; the unquoted
	// branch must too. Current git (2.56.0) emits no such suffix on plain
	// lines, so this pins hardening rather than observed output. A
	// stripping source prefix makes the latent mangling visible: pre-fix,
	// the tab and timestamp ride through fixPath into the path string
	// (verified: fixPath("proj/f.txt\\t<ts>") yields "f.txt\\t<ts>"), so
	// the suffix is not preserved as separate bytes.
	meta := []byte("--- a/proj/f.txt\t1200000000 +0000\n+++ b/proj/f.txt\t1300000000 +0000")
	strip := func(p string) string { return strings.TrimPrefix(p, "proj/") }
	out := rewriteDiffMeta(meta, strip)
	if string(out) != "--- a/f.txt\t1200000000 +0000\n+++ b/f.txt\t1300000000 +0000" {
		t.Fatalf("unquoted-line suffix mangled:\n%s", out)
	}
}

func TestParseDiffHeaderQuotedInternalQuote(t *testing.T) {
	// Greedy (".*") would consume past the escaped quote to the last quote;
	// backslash-aware pairs must stop at the true terminator.
	line := `diff --git "a/q\"f\"g" "b/q\"f\"g"`
	if got := parseDiffHeader([]byte(line)); string(got) != `q"f"g` {
		t.Fatalf("parsed %q, want %q", got, `q"f"g`)
	}
}

func TestLfsPointersSpacedPath(t *testing.T) {
	listing := []byte("abc123 * my doc.pdf\ndef456 * plain.bin\n")
	got, err := lfsPointers(listing, "")
	if err != nil {
		t.Fatalf("lfsPointers: %v", err)
	}
	if len(got) != 2 || got[0] != "my doc.pdf" || got[1] != "plain.bin" {
		t.Fatalf("pointers = %q, want spaced + plain paths", got)
	}
}

// TestForeachDiffGitBoundary pins the split hardening: sections must
// break only on the full "diff --git " header, never on a bare payload line
// that merely starts with "diff". Unified-diff content lines carry a
// leading "+" so they can never collide; the realistic collision is a bare
// base85 binary-payload line ("diffXYZ...") at line start with no "+"
// prefix. Git's base85 payload alphabet excludes the space, so the longer
// prefix cannot occur inside payloads.
func TestForeachDiffGitBoundary(t *testing.T) {
	raw := []byte("diff --git a/one b/one\nindex 00..11\n@@\n+A\n+B\n" +
		"diffXYZ-bare-payload-line\n+tail\n" +
		"diff --git a/two b/two\nindex 22..33\n@@\n+C\n")
	var sections [][]byte
	if err := foreach(raw, "diff --git ", func(s []byte) error {
		sections = append(sections, s)
		return nil
	}); err != nil {
		t.Fatalf("foreach: %v", err)
	}
	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2", len(sections))
	}
	// The legacy short prefix over-splits on the payload line; the
	// hardened boundary must not.
	legacy := 0
	_ = foreach(raw, "diff", func(s []byte) error {
		legacy++
		return nil
	})
	if legacy != 3 {
		t.Fatalf("fixture does not demonstrate the legacy over-split: %d sections", legacy)
	}
	// The hardened split must keep the payload line inside its section.
	if !bytes.Contains(sections[0], []byte("diffXYZ-bare-payload-line")) {
		t.Fatalf("payload line lost from its section:\n%s", sections[0])
	}
}
