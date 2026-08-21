package main

import (
	"strings"
	"testing"
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
