package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/grailbio/grit/git"
)

func TestIsProcessed(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef01234567"
	processed := map[string]bool{
		full:      true, // full-digest tag (current format)
		"0fedcba": true, // legacy abbreviated tag
	}
	for _, tc := range []struct {
		hex  string
		want bool
	}{
		{full, true},      // exact full-digest match
		{"0fedcba", true}, // exact legacy match
		// A full digest whose leading characters are a recorded legacy
		// id is excluded by prefix.
		{"0fedcba" + strings.Repeat("a", 33), true},
		// Unrelated digests are not excluded, including one that merely
		// contains a recorded id after its leading characters.
		{"1111111111111111111111111111111111111111", false},
		{"999999" + "0fedcba" + strings.Repeat("0", 27), false},
	} {
		if got := isProcessed(tc.hex, processed); got != tc.want {
			t.Errorf("isProcessed(%q) = %v, want %v", tc.hex, got, tc.want)
		}
	}
}

func TestRewriteDiffGuards(t *testing.T) {
	r := rules{rewrite: []rewriteRule{
		{pathRe: regexp.MustCompile(`.*`), oldRe: regexp.MustCompile(`x`), new: []byte("y")},
	}}
	// Binary sections are never rewritten.
	binary := git.Diff{Path: "b.dat", Meta: []byte("index 00..11\ndata GIT binary patch literal 12\n"), Body: nil}
	r.rewriteDiff(&binary)
	if len(binary.Body) != 0 {
		t.Fatalf("empty body of a binary diff gained content: %q", binary.Body)
	}
	// Empty textual bodies gain no content.
	empty := git.Diff{Path: "e.txt"}
	r.rewriteDiff(&empty)
	if len(empty.Body) != 0 {
		t.Fatalf("empty body gained content: %q", empty.Body)
	}
	// Textual payloads are rewritten as before (upstream terminates
	// every rewritten line).
	text := git.Diff{Path: "t.txt", Body: []byte("+xx\n")}
	r.rewriteDiff(&text)
	if string(text.Body) != "+yy\n\n" {
		t.Fatalf("textual rewrite broken: %q", text.Body)
	}
}
