package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/joeycumines/grit/git"
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

func TestSrcRelativeDiffPaths(t *testing.T) {
	diffs := []git.Diff{
		{Path: "prefixed/deep/file.txt"},
		{Path: "prefixed/other.bin"},
		{Path: "unrelated"},
	}
	got := srcRelativeDiffPaths(diffs, "prefixed/")
	for _, want := range []string{"deep/file.txt", "other.bin", "unrelated"} {
		if !got[want] {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	if got["prefixed/deep/file.txt"] {
		t.Error("destination prefix was not stripped")
	}
	// Unprefixed destinations leave paths untouched.
	got = srcRelativeDiffPaths(diffs, "")
	if !got["prefixed/deep/file.txt"] || len(got) != 3 {
		t.Fatalf("empty destination prefix mishandled: %v", got)
	}
}

func TestParseSpec(t *testing.T) {
	for _, tc := range []struct {
		name           string
		spec           string
		url            string
		prefix, branch string
		wantErr        bool
	}{
		{"url only", "https://example.com/r.git", "https://example.com/r.git", "", "master", false},
		{"url and prefix", "u,p/", "u", "p/", "master", false},
		{"all three fields", "u,p/,main", "u", "p/", "main", false},
		{"empty fields preserved", ",,", "", "", "", false},
		{"too many fields", "a,b,c,d", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, prefix, branch, err := parseSpec(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseSpec(%q) succeeded: %q %q %q", tc.spec, url, prefix, branch)
				}
				if !strings.Contains(err.Error(), "too many comma-separated fields") {
					t.Fatalf("error %v does not identify the field overflow", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSpec(%q): %v", tc.spec, err)
			}
			if url != tc.url || prefix != tc.prefix || branch != tc.branch {
				t.Fatalf("parseSpec(%q) = %q,%q,%q want %q,%q,%q", tc.spec, url, prefix, branch, tc.url, tc.prefix, tc.branch)
			}
		})
	}
}

func TestParseRewriteRule(t *testing.T) {
	valid := []struct {
		name     string
		rule     string
		pathRe   string
		oldRe    string
		repl     string
		sampleIn string
		sample   string
	}{
		{
			name: "slash separators", rule: "go.mod$:/replace .* => .*//",
			pathRe: "go.mod$", oldRe: "replace .* => .*", repl: "",
			sampleIn: "replace old => new\n", sample: "\n\n",
		},
		{
			name: "alternate separator", rule: `x\.txt$:!-old-!-new-!`,
			pathRe: `x\.txt$`, oldRe: `-old-`, repl: "-new-",
			sampleIn: "a-old-b\n", sample: "a-new-b\n\n",
		},
		{
			name: "regex from", rule: `f$:/o+/<X>/`,
			pathRe: "f$", oldRe: "o+", repl: "<X>",
			sampleIn: "fooo\n", sample: "f<X>\n\n",
		},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parseRewriteRule(tc.rule)
			if err != nil {
				t.Fatalf("parseRewriteRule(%q): %v", tc.rule, err)
			}
			if r.pathRe.String() != tc.pathRe || r.oldRe.String() != tc.oldRe || string(r.new) != tc.repl {
				t.Fatalf("rule %+v, want path=%q old=%q new=%q", r, tc.pathRe, tc.oldRe, tc.repl)
			}
			if got := string(r.rewrite([]byte(tc.sampleIn))); got != tc.sample {
				t.Fatalf("rewrite(%q) = %q, want %q", tc.sampleIn, got, tc.sample)
			}
		})
	}
	invalid := []struct {
		name  string
		rule  string
		inErr string
	}{
		{"missing separator colon", "no-separator-here", "invalid rewrite rule"},
		{"uncompilable path regexp", "(unclosed,:/a/b/", "invalid path regexp"},
		{"suffix too short", "f$:/a", "must be of form"},
		{"empty rewrite body", "f$:", "must be of form"},
		{"wrong field count", "f$:/a/b/c/", "must be of form"},
		{"unterminated trailing separator", "f$:/a/b/x", "must be of form"},
		{"uncompilable from regexp", "f$:/(unclosed/b/", "invalid 'from' regexp"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRewriteRule(tc.rule)
			if err == nil {
				t.Fatalf("parseRewriteRule(%q) succeeded", tc.rule)
			}
			if !strings.Contains(err.Error(), tc.inErr) {
				t.Fatalf("error %v lacks %q", err, tc.inErr)
			}
		})
	}
}

func digestCommit(t *testing.T, hex string) *git.Commit {
	t.Helper()
	d, err := git.SHA1.Parse(hex)
	if err != nil {
		t.Fatal(err)
	}
	return &git.Commit{Digest: d}
}

func TestRulesIsStripped(t *testing.T) {
	full := strings.Repeat("0123456789abcdef", 4)[:40]
	prefix7 := full[:7]
	r := rules{stripCommits: []string{prefix7}}
	if !r.isStripped(digestCommit(t, full)) {
		t.Error("seven-digit prefix must strip a commit whose digest extends it")
	}
	if !r.isStripped(digestCommit(t, prefix7+strings.Repeat("0", 33))) {
		t.Error("prefix rule did not strip an extending digest")
	}
	other := strings.Repeat("fedcba9876543210", 4)[:40]
	if r.isStripped(digestCommit(t, other)) {
		t.Error("unrelated digest stripped by prefix")
	}
	fullRule := rules{stripCommits: []string{full}}
	if !fullRule.isStripped(digestCommit(t, full)) {
		t.Error("full-digest rule did not match its own commit")
	}
	if fullRule.isStripped(digestCommit(t, full[:39]+"e")) {
		t.Error("full-digest rule matched a differing digest")
	}
	// A ruleset holding several prefixes must consult every entry: a
	// commit matching only the second prefix is still stripped.
	multi := rules{stripCommits: []string{other[:7], prefix7}}
	if !multi.isStripped(digestCommit(t, full)) {
		t.Error("second-entry prefix was not consulted")
	}
	if !multi.isStripped(digestCommit(t, other)) {
		t.Error("first-entry prefix stopped matching its own digest")
	}
	unrelated := strings.Repeat("9876543210abcdef", 4)[:40]
	if multi.isStripped(digestCommit(t, unrelated)) {
		t.Error("digest matching neither entry was stripped")
	}
}

func TestRulesPathAndMessageStripping(t *testing.T) {
	a := regexp.MustCompile(`^BUILD$`)
	b := regexp.MustCompile(`^vendor/`)
	msg := regexp.MustCompile(`go\.(mod|sum)$`)
	r := rules{strip: []*regexp.Regexp{a, b}, stripMessagePaths: []*regexp.Regexp{msg}}

	for _, p := range []string{"BUILD", "vendor/x/y"} {
		match, re := r.isPathStripped(p)
		if !match {
			t.Errorf("isPathStripped(%q) did not match", p)
		}
		if re != a && re != b {
			t.Errorf("isPathStripped(%q) returned an unknown regexp", p)
		}
	}
	if match, _ := r.isPathStripped("builder"); match {
		t.Error("anchored strip rule matched a sibling name")
	}
	if _, re := r.isPathStripped("BUILD"); re != a {
		t.Error("multi-rule matching did not preserve rule order")
	}
	if match, _ := r.isMessagePathStripped("go.mod"); !match {
		t.Error("isMessagePathStripped missed go.mod")
	}
	if match, _ := r.isMessagePathStripped("go.sum"); !match {
		t.Error("isMessagePathStripped missed go.sum")
	}
	if match, _ := r.isMessagePathStripped("go.mod.bak"); match {
		t.Error("isMessagePathStripped matched a non-source file")
	}
}

func TestRulesMayRewrite(t *testing.T) {
	r := rules{rewrite: []rewriteRule{
		{pathRe: regexp.MustCompile(`^go\.mod$`)},
	}}
	if !r.mayRewrite("go.mod") {
		t.Error("mayRewrite missed its own target")
	}
	if r.mayRewrite("go.modx") {
		t.Error("mayRewrite matched beyond its anchor")
	}
	if (rules{}).mayRewrite("anything") {
		t.Error("empty ruleset rewrote")
	}
}

// fixtureRepo builds a real repository whose history supports
// isCommitApplicable: one commit touching proj/f.txt only, one touching
// only top.txt outside proj/, and one adding proj/BUILD.
func fixtureRepo(t *testing.T) (*git.Repo, map[string]*git.Commit) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=init.defaultBranch",
			"GIT_CONFIG_VALUE_0=master")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "--bare", filepath.Join(dir, "origin.git"))
	run("clone", "-q", filepath.Join(dir, "origin.git"), filepath.Join(dir, "work"))

	w := filepath.Join(dir, "work")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(w, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) *git.Commit {
		t.Helper()
		run("-C", w, "add", "-A")
		run("-C", w, "commit", "-m", msg)
		out, err := exec.Command("git", "-C", w, "rev-parse", "HEAD").CombinedOutput()
		if err != nil {
			t.Fatalf("rev-parse: %v\n%s", err, out)
		}
		return digestCommit(t, strings.TrimSpace(string(out)))
	}

	write("proj/f.txt", "in prefix")
	inPrefix := commit("in prefix")
	write("top.txt", "outside")
	outside := commit("outside only")
	write("proj/BUILD", "stripped target")
	stripped := commit("stripped target")

	run("-C", w, "push", "-q", "origin", "master")

	prevDir := git.Dir
	git.Dir = filepath.Join(t.TempDir(), "grit")
	t.Cleanup(func() { git.Dir = prevDir })

	src, err := git.Open(filepath.Join(dir, "origin.git"), "proj/", "master")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	return src, map[string]*git.Commit{
		"inPrefix": inPrefix, "outside": outside, "stripped": stripped,
	}
}

func TestIsCommitApplicable(t *testing.T) {
	src, commits := fixtureRepo(t)

	applicable := rules{}
	ok, err := applicable.isCommitApplicable(commits["inPrefix"], src)
	if err != nil || !ok {
		t.Fatalf("in-prefix change: ok=%v err=%v, want applicable", ok, err)
	}

	ok, err = applicable.isCommitApplicable(commits["outside"], src)
	if err != nil || ok {
		t.Fatalf("out-of-prefix-only change: ok=%v err=%v, want empty in prefix", ok, err)
	}

	stripping := rules{strip: []*regexp.Regexp{regexp.MustCompile(`^BUILD$`)}}
	ok, err = stripping.isCommitApplicable(commits["stripped"], src)
	if err != nil || ok {
		t.Fatalf("all-paths-stripped change: ok=%v err=%v, want not applicable", ok, err)
	}
	ok, err = stripping.isCommitApplicable(commits["inPrefix"], src)
	if err != nil || !ok {
		t.Fatalf("strip rule leaked into unrelated commit: ok=%v err=%v", ok, err)
	}
}
