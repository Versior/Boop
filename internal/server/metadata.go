package server

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"boop/internal/content"
	"boop/internal/settings"
)

// pageMeta is the document-level metadata of one rendered page: the address a
// crawler should index it under, the preview a chat client shows when the URL is
// pasted, and the structured data a search engine reads.
//
// Every URL in here is absolute and built from BOOP_BASE_URL. The request Host
// and the X-Forwarded-* headers are never consulted, so a spoofed header cannot
// make one deployment advertise another origin.
type pageMeta struct {
	// Canonical is the absolute address of this page with no query string.
	// Comments, cursors and the feed type filter are all dropped, so every view
	// of the same page collapses onto one address: /?type=article and a cursor
	// page render the same document title and the same cards in a different
	// order, and claiming eight addresses for one document is what splits a
	// crawler's signal.
	Canonical string
	// Type is the Open Graph object type: "website" for an index surface,
	// "article" for a single post.
	Type string
	// Title is the page-specific part of the document title. Empty means the
	// page has no title of its own and the site name stands alone.
	Title string
	// Description is the link-preview and search-result summary. Empty falls
	// back to the configured site description.
	Description string
	// Image is the absolute preview image of this page, empty when it has none.
	// A post uses its own cover and gets nothing when it has no cover, rather
	// than borrowing the site avatar and claiming it in structured data.
	Image string
	// PublishedAt and ModifiedAt are the stored RFC3339 stamps of a post. They
	// stay empty on every other page.
	PublishedAt string
	ModifiedAt  string
	// NoIndex keeps a page out of a search index. It is set on the surfaces that
	// exist for one session rather than for the public.
	NoIndex bool
	// JSONLD is the rendered structured-data document, empty when this page
	// carries none.
	JSONLD template.JS
}

// DocumentTitle is the full contents of the <title> element: the page's own
// title with the site name appended, or the site name alone when the page has
// none. It is a method rather than a field so the title block and og:title
// cannot drift apart, and it is only meaningful for a page that set a Title.
func (m pageMeta) DocumentTitle(siteName string) string {
	if m.Title == "" {
		return siteName
	}
	return m.Title + " · " + siteName
}

// pageMetaOf is the metadata every page starts from: its own path turned into an
// absolute address, and the private-surface flags.
func (s *server) pageMetaOf(r *http.Request) pageMeta {
	return pageMeta{
		// EscapedPath is the percent-encoded form, which is what belongs in a
		// URL: a slug may hold non-ASCII bytes, and a raw one would be emitted
		// into the attribute unencoded.
		Canonical: s.cfg.BaseURL + r.URL.EscapedPath(),
		Type:      "website",
		// Two signals on purpose. The robots.txt rule spares a compliant crawler
		// the request; this covers the ones that ignore it, and a page that
		// exists for one session must not be indexed by either kind.
		NoIndex: isPrivatePath(r.URL.Path),
	}
}

// isPrivatePath reports whether a path is a session-scoped surface rather than
// public content. It mirrors robotsDisallow, which is the list a crawler is
// asked to honour.
func isPrivatePath(path string) bool {
	switch path {
	case "/bookmarks", "/login", "/register":
		return true
	default:
		return strings.HasPrefix(path, "/admin")
	}
}

// brandImage is the configured image that represents the whole site, used as the
// link preview of the index surfaces. The cover image wins: it is the one image
// drawn to be seen at full width, so a preview built from it is a banner rather
// than a square crop. The avatar comes next and the tab icon last, because an
// icon is drawn for sixteen pixels.
func brandImage(values settings.Values) string {
	if values.SiteCoverURL != "" {
		return values.SiteCoverURL
	}
	if values.SiteAvatarURL != "" {
		return values.SiteAvatarURL
	}
	return values.SiteIconURL
}

// absoluteImage turns a stored image URL into an absolute one. Media URLs are
// site-relative (/uploads/...) and configured brand images are already absolute,
// so both shapes have to be accepted.
func absoluteImage(baseURL, image string) string {
	if image == "" {
		return ""
	}
	if strings.HasPrefix(image, "http://") || strings.HasPrefix(image, "https://") {
		return image
	}
	return baseURL + image
}

// structuredDataType names what a post is in schema.org terms. An article is a
// BlogPosting; a moment or a photograph is a post on a feed rather than an
// article, and schema.org has the subtype for exactly that. Google's Article
// guidance is explicit that a page which is not an article should not be marked
// up as one.
func structuredDataType(postType string) string {
	if postType == content.TypeArticle {
		return "BlogPosting"
	}
	return "SocialMediaPosting"
}

// schemaPost is the posting document of a detail page.
type schemaPost struct {
	Context       string       `json:"@context"`
	Type          string       `json:"@type"`
	MainEntity    *schemaID    `json:"mainEntityOfPage,omitempty"`
	Headline      string       `json:"headline"`
	Description   string       `json:"description,omitempty"`
	Image         []string     `json:"image,omitempty"`
	DatePublished string       `json:"datePublished,omitempty"`
	DateModified  string       `json:"dateModified,omitempty"`
	Author        *schemaNamed `json:"author,omitempty"`
	Publisher     *schemaNamed `json:"publisher,omitempty"`
	InLanguage    string       `json:"inLanguage"`
}

// schemaSite is the document of an index surface. The name is what Google reads
// for the site name it prints above a result, so it is the configured one rather
// than a hardcoded string.
type schemaSite struct {
	Context    string `json:"@context"`
	Type       string `json:"@type"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	InLanguage string `json:"inLanguage"`
}

type schemaID struct {
	Type string `json:"@type"`
	ID   string `json:"@id"`
}

type schemaNamed struct {
	Type string `json:"@type"`
	Name string `json:"name"`
}

// imageList renders the optional single image as the array the schema allows.
func imageList(image string) []string {
	if image == "" {
		return nil
	}
	return []string{image}
}

// jsonLD renders a structured-data document for a
// <script type="application/ld+json"> data block.
//
// json.Marshal escapes <, > and & as \u003c, \u003e and \u0026, so no title or
// description can close the script element however it is written, and it escapes
// U+2028 and U+2029, which are literal line terminators inside a JavaScript
// string. The block is therefore safe to inject as raw template.JS, and it has
// to be raw: any further escaping would corrupt the JSON. The type attribute is
// also what keeps the element out of script-src, because a script block whose
// type is not a JavaScript MIME type is discarded before the CSP check runs.
//
// A failure here is a programming error rather than an input error, since every
// document is built from plain strings. The page stays correct without it, and
// the log is what makes the omission visible.
func (s *server) jsonLD(r *http.Request, document any) template.JS {
	encoded, err := json.Marshal(document)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "structured data failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		return ""
	}
	return template.JS(encoded)
}

// postingMetadata fills the document metadata of a detail page from the row
// itself, so the head, the structured data and the visible content cannot
// disagree.
func (s *server) postingMetadata(r *http.Request, view *postPageView, post *content.Post, author string) {
	meta := &view.Meta
	meta.Type = "article"

	// The author's SEO override wins in the head only. The visible heading keeps
	// the real title: a wording chosen for a search result is not always the
	// wording the page should show.
	meta.Title = strings.TrimSpace(post.SEOTitle)
	if meta.Title == "" {
		meta.Title = view.Title
	}
	meta.Description = strings.TrimSpace(post.SEODescription)
	if meta.Description == "" {
		meta.Description = view.Description
	}
	meta.PublishedAt = post.PublishedAt
	meta.ModifiedAt = post.UpdatedAt
	meta.Image = absoluteImage(s.cfg.BaseURL, view.Card.ImageURL)

	meta.JSONLD = s.jsonLD(r, schemaPost{
		Context:       "https://schema.org",
		Type:          structuredDataType(post.Type),
		MainEntity:    &schemaID{Type: "WebPage", ID: meta.Canonical},
		Headline:      meta.Title,
		Description:   meta.Description,
		Image:         imageList(meta.Image),
		DatePublished: meta.PublishedAt,
		DateModified:  meta.ModifiedAt,
		Author:        &schemaNamed{Type: "Person", Name: author},
		Publisher:     &schemaNamed{Type: "Organization", Name: view.SiteName},
		InLanguage:    "zh-CN",
	})
}

// indexMetadata fills the document metadata of the home page: the brand preview
// image and the WebSite document, which is what tells a search engine the name
// of the site as a whole.
func (s *server) indexMetadata(r *http.Request, view *feedView, values settings.Values) {
	view.Meta.Image = absoluteImage(s.cfg.BaseURL, brandImage(values))
	view.Meta.JSONLD = s.jsonLD(r, schemaSite{
		Context:    "https://schema.org",
		Type:       "WebSite",
		Name:       values.SiteName,
		URL:        s.cfg.BaseURL + "/",
		InLanguage: "zh-CN",
	})
}
