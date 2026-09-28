// Markdown rendering for Boop content: goldmark parses, bluemonday sanitizes.
// Rendering happens before any database work so the single write connection is
// never held while parsing.
package content

import (
	"bytes"
	"html"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// markdown is the shared parser. GFM tables, strikethrough, task lists and
// autolinks are part of the authoring experience; raw HTML is passed through so
// the sanitizer, not the parser, owns what survives.
var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

// sanitizer is the single allow-list policy for stored HTML: it keeps the
// formatting goldmark produces and strips scripts, event handlers, inline
// styles and dangerous URL schemes.
func newSanitizer() *bluemonday.Policy {
	policy := bluemonday.UGCPolicy()
	policy.AllowRelativeURLs(true)
	policy.RequireNoFollowOnLinks(false)
	policy.AddTargetBlankToFullyQualifiedLinks(true)
	return policy
}

var sanitizer = newSanitizer()

// RenderMarkdown converts Markdown to sanitized HTML. The result is stored in
// posts.body_html and rendered as trusted markup; it is only ever produced
// here, which is the trust boundary for that column.
func RenderMarkdown(source string) (string, error) {
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(source), &buf); err != nil {
		return "", err
	}
	return strings.TrimSpace(sanitizer.SanitizeReader(&buf).String()), nil
}

// RenderPlainText escapes short-form bodies (moments and photo captions) and
// keeps their line breaks.
func RenderPlainText(source string) string {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(html.EscapeString(trimmed), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return "<p>" + strings.Join(lines, "<br>") + "</p>"
}

// RenderBody picks the representation for a post type.
func RenderBody(postType, body string) (string, error) {
	if postType == TypeArticle {
		return RenderMarkdown(body)
	}
	return RenderPlainText(body), nil
}
