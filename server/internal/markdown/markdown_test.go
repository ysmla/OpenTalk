package markdown

import (
	"errors"
	"strings"
	"testing"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

func TestValidateTitle(t *testing.T) {
	cases := map[string]struct {
		input string
		want  string
		err   error
	}{
		"plain":                {"发布一个帖子", "发布一个帖子", nil},
		"trims surrounding":    {"  spaced  ", "spaced", nil},
		"exactly at the limit": {strings.Repeat("字", TitleMaxRunes), strings.Repeat("字", TitleMaxRunes), nil},
		"empty":                {"", "", ErrTitleRequired},
		"whitespace only":      {"   \t ", "", ErrTitleRequired},
		"over the limit":       {strings.Repeat("字", TitleMaxRunes+1), "", ErrTitleTooLong},
		"newline":              {"two\nlines", "", ErrTitleMultiline},
		"carriage return":      {"two\rlines", "", ErrTitleMultiline},
		"tab":                  {"two\tparts", "", ErrTitleMultiline},
		"nul byte":             {"a\x00b", "", ErrTitleMultiline},
		"delete character":     {"a\x7fb", "", ErrTitleMultiline},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ValidateTitle(tc.input)
			if !errors.Is(err, tc.err) {
				t.Fatalf("error=%v want=%v", err, tc.err)
			}
			if err == nil && got != tc.want {
				t.Fatalf("value=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestValidateBody(t *testing.T) {
	codeWithScript := "示例：\n\n```html\n<script>alert(1)</script>\n```\n"
	indentedCode := "示例：\n\n    <script>alert(1)</script>\n"
	inlineCode := "写作 `<script>` 会被当作文本。"
	cases := map[string]struct {
		input string
		err   error
	}{
		"plain paragraph":             {"一段普通正文。", nil},
		"common markdown":             {"# 标题\n\n- 一\n- 二\n\n**粗体**与[链接](https://ok.test)", nil},
		"fenced code may hold script": {codeWithScript, nil},
		"indented code may hold html": {indentedCode, nil},
		"inline code may hold html":   {inlineCode, nil},
		"escaped entity":              {"&lt;script&gt; 只是文字", nil},
		"relative link":               {"[相对](/boards/1)", nil},
		"fragment link":               {"[锚点](#section)", nil},
		"mailto link":                 {"[写信](mailto:a@b.test)", nil},
		"colon later in path":         {"[路径](https://ok.test/a:b)", nil},
		"protocol relative":           {"[外站](//ok.test/x)", nil},
		"bare url is linkified":       {"见 https://ok.test/x", nil},
		"empty":                       {"", ErrBodyRequired},
		"whitespace only":             {"  \n\t\n ", ErrBodyRequired},
		"over the limit":              {strings.Repeat("字", BodyMaxRunes+1), ErrBodyTooLong},
		"exactly at the limit":        {strings.Repeat("字", BodyMaxRunes), nil},
		"raw script":                  {"正文<script>alert(1)</script>", ErrRawHTML},
		"raw html block":              {"<div onclick=\"x\">hi</div>", ErrRawHTML},
		"inline html tag":             {"这是<b>粗体</b>标签", ErrRawHTML},
		"html comment":                {"<!-- 注释 -->", ErrRawHTML},
		"iframe":                      {"<iframe src=\"https://evil.test\"></iframe>", ErrRawHTML},
		"javascript link":             {"[点我](javascript:alert(1))", ErrUnsafeLink},
		"mixed case scheme":           {"[点我](JaVaScRiPt:alert(1))", ErrUnsafeLink},
		"data url image":              {"![图](data:text/html;base64,PHNjcmlwdD4=)", ErrUnsafeLink},
		"vbscript link":               {"[点我](vbscript:msgbox(1))", ErrUnsafeLink},
		"file link":                   {"[本地](file:///etc/passwd)", ErrUnsafeLink},
		// A bare "javascript:" is not linkified by the shared extension set, so it stays literal text
		// and there is no link to reject. TestLinkifyCoverage pins that down; if a parser upgrade ever
		// began linkifying bare schemes, that test fails and this case must become ErrUnsafeLink.
		"bare javascript stays text": {"见 javascript:alert(1) 这段", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ValidateBody(tc.input)
			if !errors.Is(err, tc.err) {
				t.Fatalf("error=%v want=%v", err, tc.err)
			}
			// The stored value must be the submission unchanged: trimming leading whitespace would
			// silently convert an indented code block into a paragraph.
			if err == nil && got != tc.input {
				t.Fatalf("body was modified:\n got %q\nwant %q", got, tc.input)
			}
		})
	}
}

// TestLinkifyCoverage pins down exactly which destinations the parser turns into links, because the
// protocol check can only reject links that exist. If a parser upgrade started linkifying a scheme
// this set does not expect, the count changes and the "bare javascript" case above must be revisited.
func TestLinkifyCoverage(t *testing.T) {
	cases := map[string]struct {
		input     string
		wantLinks int
	}{
		"bare https":      {"见 https://ok.test/x", 1},
		"bare www":        {"见 www.ok.test", 1},
		"bare email":      {"联系 a@b.test", 1},
		"bare javascript": {"见 javascript:alert(1)", 0},
		"bare data":       {"见 data:text/html,x", 0},
		"markdown link":   {"[x](javascript:alert(1))", 1},
		"code fence":      {"```\n[x](javascript:alert(1))\n```", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			source := []byte(tc.input)
			document := parser.Parse(text.NewReader(source))
			links := 0
			_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
				if !entering {
					return ast.WalkContinue, nil
				}
				switch node.(type) {
				case *ast.Link, *ast.Image, *ast.AutoLink:
					links++
				}
				return ast.WalkContinue, nil
			})
			if links != tc.wantLinks {
				t.Fatalf("links=%d want=%d for %q", links, tc.wantLinks, tc.input)
			}
		})
	}
}

func TestUnsafeDestination(t *testing.T) {
	cases := map[string]bool{
		"https://ok.test":           false,
		"http://ok.test/a":          false,
		"mailto:a@b.test":           false,
		"/relative":                 false,
		"#fragment":                 false,
		"//protocol.relative":       false,
		"":                          false,
		"https://ok.test/a:b":       false,
		"java\tscript:alert(1)":     true,
		"java\nscript:alert(1)":     true,
		"  javascript:alert(1)  ":   true,
		"JavaScript:alert(1)":       true,
		"JAVASCRIPT:alert(1)":       true,
		"data:text/html;base64,AAA": true,
		"vbscript:msgbox(1)":        true,
		"file:///etc/passwd":        true,
		"blob:https://ok.test/uuid": true,
		"about:blank":               true,
		"chrome://settings":         true,
		"j\u0000avascript:alert(1)": true,
		"ftp://ok.test/file":        true,
	}
	for destination, wantUnsafe := range cases {
		t.Run(destination, func(t *testing.T) {
			if got := unsafeDestination([]byte(destination)); got != wantUnsafe {
				t.Fatalf("unsafeDestination(%q)=%v want=%v", destination, got, wantUnsafe)
			}
		})
	}
}
