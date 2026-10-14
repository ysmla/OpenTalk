// Package markdown validates user-submitted Markdown before it is stored.
//
// The contract stores Markdown source and renders it in the browser, so the server's job is to refuse
// anything a renderer could turn into executable markup or a data-exfiltrating link. Parsing is
// required rather than scanning: a fenced code block may legitimately contain "<script>", and only a
// real parse tree distinguishes that from raw HTML in the document body.
//
// The extension set here MUST be at least as link-producing as the browser renderer's. If the browser
// autolinked bare URLs while this parser did not, a bare "javascript:…" could become a link the server
// never inspected.
package markdown

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

// Limits are counted in runes and stay below the storage capacity of the columns
// (topics.title VARCHAR(240), contents.body_md MEDIUMTEXT).
const (
	TitleMaxRunes = 120
	BodyMaxRunes  = 20000
)

var (
	ErrTitleRequired  = errors.New("title is required")
	ErrTitleTooLong   = errors.New("title is too long")
	ErrTitleMultiline = errors.New("title must be a single line")
	ErrBodyRequired   = errors.New("body is required")
	ErrBodyTooLong    = errors.New("body is too long")
	ErrRawHTML        = errors.New("raw HTML is not allowed")
	ErrUnsafeLink     = errors.New("link protocol is not allowed")
)

// parser mirrors the extensions the browser renderer enables. Linkify matters most: it is what turns a
// bare URL into a link node that must be protocol-checked.
var parser = goldmark.New(
	goldmark.WithExtensions(extension.Linkify, extension.Table, extension.Strikethrough),
).Parser()

// ValidateTitle checks a topic title and returns the value to store. A title is a single line of plain
// text: control characters are refused so a newline cannot forge a header in a notification email.
func ValidateTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return "", ErrTitleRequired
	}
	if utf8.RuneCountInString(title) > TitleMaxRunes {
		return "", ErrTitleTooLong
	}
	for _, r := range title {
		if r < 0x20 || r == 0x7f {
			return "", ErrTitleMultiline
		}
	}
	return title, nil
}

// ValidateBody checks Markdown source and returns the value to store. The stored value is the
// submission unchanged: trimming leading whitespace would silently turn an indented code block into a
// paragraph, so only the emptiness check looks at the trimmed form.
func ValidateBody(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", ErrBodyRequired
	}
	if utf8.RuneCountInString(raw) > BodyMaxRunes {
		return "", ErrBodyTooLong
	}
	source := []byte(raw)
	document := parser.Parse(text.NewReader(source))
	err := ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.Kind() {
		case ast.KindHTMLBlock, ast.KindRawHTML:
			// Rejected wherever it appears in the document, including inside a paragraph.
			return ast.WalkStop, ErrRawHTML
		}
		switch typed := node.(type) {
		case *ast.Link:
			if unsafeDestination(typed.Destination) {
				return ast.WalkStop, ErrUnsafeLink
			}
		case *ast.Image:
			if unsafeDestination(typed.Destination) {
				return ast.WalkStop, ErrUnsafeLink
			}
		case *ast.AutoLink:
			if unsafeDestination(typed.URL(source)) {
				return ast.WalkStop, ErrUnsafeLink
			}
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// allowedProtocols are the schemes a forum post actually needs. Everything else is refused rather than
// blocklisted, so a new scheme a browser starts executing is denied by default.
var allowedProtocols = map[string]bool{"http": true, "https": true, "mailto": true}

// unsafeDestination reports whether following a URL could execute code or exfiltrate data.
//
// It lower-cases the value and strips control characters before looking for the scheme, because
// browsers ignore embedded tabs and newlines: "java\tscript:" is executable, so a naive prefix check
// would miss it. A relative URL, a fragment and a protocol-relative URL carry no scheme and are safe.
func unsafeDestination(destination []byte) bool {
	value := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToLower(string(destination)))
	colon := strings.IndexByte(value, ':')
	if colon < 0 {
		return false
	}
	// A slash before the colon means the colon belongs to a path, not to a scheme.
	if slash := strings.IndexByte(value, '/'); slash >= 0 && slash < colon {
		return false
	}
	return !allowedProtocols[value[:colon]]
}
