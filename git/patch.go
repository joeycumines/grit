// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/grailbio/base/digest"
)

const zeroWidthSpace = "\u200b"

// A Diff represents a set of changes to a single file.
type Diff struct {
	// Path holds the path of the file to be changed.
	Path string
	// Meta holds the diff's metadata, treated opaquely.
	Meta []byte
	// Body is the actual diff contents. It is interpreted by
	// git when applying a patch.
	Body []byte
}

var zeroBlob = strings.Repeat("0", 40)

// NewBlob returns the post-image blob digest recorded in the diff's
// "index <old>..<new>[ <mode>]" metadata line. Deleted files record the
// zero digest. The second return value is false when the diff carries no
// usable index line.
func (d Diff) NewBlob() (string, bool) {
	for _, line := range bytes.Split(d.Meta, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("index ")) {
			continue
		}
		rest := string(line[len("index "):])
		parts := strings.SplitN(rest, "..", 2)
		if len(parts) != 2 {
			return "", false
		}
		newBlob := parts[1]
		if i := strings.IndexByte(newBlob, ' '); i >= 0 {
			newBlob = newBlob[:i]
		}
		newBlob = strings.TrimSpace(newBlob)
		if newBlob == "" {
			return "", false
		}
		return newBlob, true
	}
	return "", false
}

// NewMode returns the post-image file mode declared by the diff, when
// one is recorded ("new file mode", a mode-change pair, or a trailing
// token on the index line). Pure content modifications usually declare
// none; the second return value is false in that case and in the
// absence of any recognizable mode metadata.
func (d Diff) NewMode() (string, bool) {
	var fromIndex string
	isDeleted := false
	for _, line := range bytes.Split(d.Meta, []byte("\n")) {
		line := string(line)
		switch {
		case strings.HasPrefix(line, "deleted file mode "):
			isDeleted = true
		case strings.HasPrefix(line, "new file mode "):
			return strings.TrimSpace(line[len("new file mode "):]), true
		case strings.HasPrefix(line, "new mode "):
			return strings.TrimSpace(line[len("new mode "):]), true
		case strings.HasPrefix(line, "index "):
			rest := line[len("index "):]
			if i := strings.Index(rest, ".."); i >= 0 {
				rest = rest[i+2:]
				if j := strings.IndexByte(rest, ' '); j >= 0 {
					fromIndex = strings.TrimSpace(rest[j+1:])
				}
			}
		}
	}
	if isDeleted {
		return "", false
	}
	if fromIndex != "" {
		return fromIndex, true
	}
	return "", false
}

// A Patch is a single, atomic change, originating in a Repo. Patches
// comprise one or more diffs, representing file changes in a
// repository. Patches may be derived from commits and applied to a
// repo in order to recreate that commit elsewhere, possibly by way
// of rewriting.
type Patch struct {
	// ID is the commit ID from which the patch was derived.
	ID digest.Digest

	// Author is the patch's author.
	Author string
	// Time is the commit time of the patch's underlying commit.
	Time time.Time
	// Subject is the patch's subject line.
	Subject string
	// Body is the patch's description.
	Body string
	// Diffs contains a set of diffs that represent the patch's
	// change.
	Diffs []Diff
}

func (p Patch) String() string {
	return fmt.Sprintf("patch %s %s %s %s (%d diffs)",
		p.ID.Hex()[:7], p.Author, p.Time, p.Subject, len(p.Diffs))
}

// Patch returns the serialized patch as a string.
func (p Patch) Patch() string {
	var b strings.Builder
	_ = p.Write(&b)
	return b.String()
}

// Write serializes the patch to the standard git patch format and
// writes it to the provided writer. Write escapes diff-like content
// in the patch body. Specifically, lines beginning with "diff",
// "---", and "+++" are prefixed with a unicode zero width space.
// This is to avoid ambiguity in git's patch parsing. This appears to
// be an issue with git itself: patches that contain other patches
// embedded in the patch description fail to apply properly using
// standard git tooling.
func (p Patch) Write(w io.Writer) error {
	ew := &errWriter{Writer: w}
	fmt.Fprintf(ew, "From %s Mon Sep 17 00:00:00 2001\n", p.ID.Hex())
	fmt.Fprintf(ew, "From: %s\n", p.Author)
	fmt.Fprintf(ew, "Date: %s\n", p.Time.Format(gitTimeLayout))
	fmt.Fprintf(ew, "Subject: %s\n", p.Subject)
	body := strings.Replace(p.Body, "\ndiff", "\n"+zeroWidthSpace+"diff", -1)
	body = strings.Replace(body, "\n---", "\n"+zeroWidthSpace+"---", -1)
	body = strings.Replace(body, "\n+++", "\n"+zeroWidthSpace+"+++", -1)
	fmt.Fprintf(ew, "\n%s\n---\n\n\n", body)
	for _, diff := range p.Diffs {
		if strings.ContainsAny(diff.Path, "\"\t\n\r") || hasControlChars(diff.Path) {
			fmt.Fprintf(ew, "diff --git %s %s\n", quotePath("a/"+diff.Path), quotePath("b/"+diff.Path))
		} else {
			fmt.Fprintf(ew, "diff --git a/%s b/%s\n", diff.Path, diff.Path)
		}
		ew.Write(diff.Meta)
		ew.Write([]byte{'\n'})
		ew.Write(diff.Body)
		ew.Write([]byte{'\n'})
	}
	return ew.Err()
}

var oid = []byte("oid")

// MaybeContainsLFSPointer uses (coarse) heuristics to determine
// whether the patch could possibly contain an LFS pointer. If it
// returns false, then there is definitely not an LFS pointer in the
// patch.
func (p Patch) MaybeContainsLFSPointer() bool {
	for _, diff := range p.Diffs {
		// This is definitely hacky, but works well enough. These are
		// required fields in any LFS pointer file, and any change
		// involving a new LFS object must declare an oid.
		if bytes.Contains(diff.Body, oid) {
			return true
		}
	}
	return false
}

// quotePath renders a path in git's C-style quoted form using only the
// escapes git's unquoting grammar accepts.
func quotePath(p string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\v':
			b.WriteString(`\v`)
		default:
			if c < 0x20 || c >= 0x7f {
				fmt.Fprintf(&b, "\\%03o", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// gitCUnquote decodes git's C-style quoted form, accepting the escapes
// git emits plus common extensions.
func gitCUnquote(s string) (string, error) {
	return strconv.Unquote(s)
}

// hasControlChars reports whether the path contains characters that
// force git to quote it in diff headers.
func hasControlChars(p string) bool {
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

var errMalformedPatch = errors.New("malformed patch")

// ParsePatchHead parses a patch header from the provided buffer.
func parsePatchHeader(b []byte) (Patch, error) {
	from := scanLine(&b)
	fields := bytes.Fields(from)
	if len(fields) < 2 {
		return Patch{}, errMalformedPatch
	}
	var (
		p   Patch
		err error
	)
	p.ID, err = SHA1.Parse(string(fields[1]))
	if err != nil {
		return Patch{}, err
	}
	m, err := mail.ReadMessage(bytes.NewReader(b))
	if err != nil {
		return Patch{}, err
	}
	p.Author = m.Header.Get("From")
	if p.Author == "" {
		return Patch{}, errors.New("patch is missing author")
	}
	p.Time, err = m.Header.Date()
	if err != nil {
		return Patch{}, err
	}
	p.Subject = m.Header.Get("Subject")
	if p.Subject == "" {
		return Patch{}, errors.New("patch is missing subject")
	}
	b, err = io.ReadAll(m.Body)
	if err != nil {
		return Patch{}, err
	}
	p.Body = string(b)
	return p, nil
}

func scan(b *[]byte, prefix string) (body []byte) {
	body = next(b, prefix)
	if len(*b) >= len(prefix) {
		*b = (*b)[len(prefix):]
	}
	return body
}

func scanLine(b *[]byte) (line []byte) {
	i := bytes.Index(*b, []byte{'\n'})
	if i < 0 {
		line = *b
		*b = nil
		return
	}
	line = (*b)[:i]
	*b = (*b)[i+1:]
	return
}

func next(b *[]byte, prefix string) (body []byte) {
	if bytes.HasPrefix(*b, []byte(prefix)) {
		return nil
	}
	i := bytes.Index(*b, []byte("\n"+prefix))
	if i < 0 {
		body = *b
		*b = nil
		return
	}
	body = (*b)[:i]
	*b = (*b)[i+1:]
	return
}

func foreach(b []byte, prefix string, do func(section []byte) error) error {
	if !bytes.HasPrefix(b, []byte(prefix)) {
		i := bytes.Index(b, []byte("\n"+prefix))
		if i < 0 {
			return nil
		}
		b = b[i+1:]
	}
	for {
		i := bytes.Index(b, []byte("\n"+prefix))
		if i < 0 {
			return do(b)
		}
		if err := do(b[:i]); err != nil {
			return err
		}
		b = b[i+1:]
	}
}

var quotedDiffHeaderRe = regexp.MustCompile(`^diff --git ("a/.*") ("b/.*")$`)

// parseDiffHeader extracts the repository-relative path from a
// "diff --git" header line. Git emits two forms: symmetric unquoted
// headers ("diff --git a/P b/P"), including for paths containing
// spaces, and C-quoted headers ("diff --git \"a/P\" \"b/P\"") for paths
// containing tabs, quotes or control characters. Both are parsed
// exactly; the symmetric form is resolved by locating the " b/"
// separator whose two halves agree.
func parseDiffHeader(line []byte) (path []byte) {
	// CRLF-contaminated patch text (e.g. mboxes round-tripped through
	// Windows tooling) leaves a trailing carriage return on every line;
	// it participates in neither the quoted regex's $ anchor nor the
	// symmetric form's left/right equality, so strip it up front.
	line = bytes.TrimSuffix(line, []byte{'\r'})
	// The C-quoted form must be tried first: it begins with a quote, so
	// the symmetric branch's "a/" prefix test can never reach it.
	if m := quotedDiffHeaderRe.FindSubmatch(line); m != nil {
		unquoted, err := gitCUnquote(string(m[1]))
		if err != nil {
			return nil
		}
		return []byte(strings.TrimPrefix(unquoted, "a/"))
	}
	rest := bytes.TrimPrefix(line, []byte("diff --git "))
	if !bytes.HasPrefix(rest, []byte("a/")) {
		return nil
	}
	rest = rest[2:]
	for i := 0; ; {
		j := bytes.Index(rest[i:], []byte(" b/"))
		if j < 0 {
			return nil
		}
		j += i
		left, right := rest[:j], rest[j+3:]
		if bytes.Equal(left, right) {
			return left
		}
		i = j + 1
	}
}

type errWriter struct {
	io.Writer
	err error
}

func (e *errWriter) Err() error {
	return e.err
}

func (e *errWriter) Write(p []byte) (n int, err error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err = e.Writer.Write(p)
	e.err = err
	return
}
