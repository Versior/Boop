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
	xhtml "golang.org/x/net/html"
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

// textBoundaryTags are the elements whose text must not run into the next one.
// Each boundary contributes a newline, so a caller that needs a single line
// collapses the whitespace afterwards.
var textBoundaryTags = map[string]bool{
	"p": true, "div": true, "br": true, "hr": true, "li": true, "ul": true, "ol": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "pre": true, "table": true, "tr": true,
}

// MarkdownPlainText renders Markdown through the same goldmark + bluemonday
// pipeline as stored bodies and returns only the text a reader sees: heading and
// emphasis markers, link targets, code fences and raw HTML are gone. The result
// carries no markup at all, so the caller owns the one remaining encoding step
// (the RSS description is XML-escaped by the encoder). The parser runs on an
// already sanitized fragment, so nothing has to be re-parsed as HTML by hand.
func MarkdownPlainText(source string) (string, error) {
	rendered, err := RenderMarkdown(source)
	if err != nil {
		return "", err
	}
	return htmlText(rendered)
}

// htmlText collects the text nodes of sanitized HTML, inserting a line break at
// every block boundary. Comments, tags and attributes are dropped, and the
// parser resolves entities, so the output is real text ("a&b") rather than
// escaped markup.
func htmlText(markup string) (string, error) {
	doc, err := xhtml.Parse(strings.NewReader(markup))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	var walk func(node *xhtml.Node)
	walk = func(node *xhtml.Node) {
		if node.Type == xhtml.TextNode {
			out.WriteString(node.Data)
			return
		}
		block := node.Type == xhtml.ElementNode && textBoundaryTags[node.Data]
		if block {
			out.WriteByte('\n')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if block {
			out.WriteByte('\n')
		}
	}
	walk(doc)
	return out.String(), nil
}

// RenderBody picks the representation for a post type.
func RenderBody(postType, body string) (string, error) {
	if postType == TypeArticle {
		return RenderMarkdown(body)
	}
	return RenderPlainText(body), nil
}
