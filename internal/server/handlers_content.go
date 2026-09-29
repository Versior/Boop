package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"boop/internal/content"
	"boop/internal/settings"
	"boop/internal/social"
)

const (
	contentJSONBytes = 512 << 10
	postPagePrefix   = "/p/"
	// summaryRunes bounds the description a detail page publishes in its head.
	// It matches the RSS description bound, so the two machine-readable copies
	// of a post summarise it the same way.
	summaryRunes = 300
)

// postPath is the relative address of a post page, with the slug encoded as a
// single path segment. Stored slugs are raw text - Slugify keeps CJK - so a
// Chinese headline becomes a Chinese path segment.
//
// The pages need no help from this: html/template already percent-encodes an
// href, so a view model keeps handing it the raw path and the template's
// escaping is the layer that owns that context. The outputs that no template
// escapes do need it, and they are the reason this function exists: the search
// JSON payload, and the sitemap and the feed, where encoding/xml only escapes
// the five XML entities and leaves the value otherwise untouched. Those two
// documents used to publish raw Chinese addresses that the sitemap protocol
// does not permit and that a reader's GUID could not open.
//
// PathEscape rather than a whole-path escape, because a slug is one segment: a
// slash inside one must not survive as a separator.
func postPath(slug string) string {
	return postPagePrefix + url.PathEscape(slug)
}

// postURL is postPath made absolute against BOOP_BASE_URL, for the documents
// that are read away from this site.
func (s *server) postURL(slug string) string {
	return s.cfg.BaseURL + postPath(slug)
}

// typeLabels maps a post type to its Chinese chip label.
var typeLabels = map[string]string{
	content.TypeMoment:  "动态",
	content.TypeArticle: "文章",
	content.TypePhoto:   "摄影",
}

// navBookmarksFilter marks the 收藏 section as current in the shell.
const navBookmarksFilter = "bookmarks"

type assetPayload struct {
	ID       int64  `json:"id"`
	URL      string `json:"url"`
	MimeType string `json:"mime_type"`
	Width    *int   `json:"width"`
	Height   *int   `json:"height"`
	Alt      string `json:"alt"`
}

type postPayload struct {
	ID           int64  `json:"id"`
	Slug         string `json:"slug"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	BodyHTML     string `json:"body_html"`
	Excerpt      string `json:"excerpt"`
	URL          string `json:"url"`
	Location     string `json:"location"`
	CapturedAt   string `json:"captured_at"`
	PublishedAt  string `json:"published_at"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	LikeCount    int    `json:"like_count"`
	CommentCount int    `json:"comment_count"`
	// Liked and Bookmarked describe the signed-in reader; they stay false for
	// guests, who have no state on the server.
	Liked      bool           `json:"liked"`
	Bookmarked bool           `json:"bookmarked"`
	Tags       []string       `json:"tags"`
	Assets     []assetPayload `json:"assets"`
}

// postPayloadOf projects a stored post into the JSON the API returns. It is a
// method because the asset URLs it publishes depend on where uploads live.
func (s *server) postPayloadOf(post content.Post) postPayload {
	tags := post.Tags
	if tags == nil {
		tags = []string{}
	}
	opts := s.mediaOpts()
	assets := make([]assetPayload, 0, len(post.Assets))
	for _, asset := range post.Assets {
		assets = append(assets, assetPayload{
			ID:       asset.ID,
			URL:      opts.URL(asset.StorageKey),
			MimeType: asset.MimeType,
			Width:    asset.Width,
			Height:   asset.Height,
			Alt:      asset.AltText,
		})
	}
	return postPayload{
		ID: post.ID, Slug: post.Slug, Type: post.Type, Status: post.Status,
		Title: post.Title, Body: post.BodyMarkdown, BodyHTML: post.BodyHTML, Excerpt: post.Excerpt,
		URL:      postPagePrefix + post.Slug,
		Location: post.Location, CapturedAt: post.CapturedAt, PublishedAt: post.PublishedAt,
		CreatedAt: post.CreatedAt, UpdatedAt: post.UpdatedAt,
		LikeCount: post.LikeCount, CommentCount: post.CommentCount,
		Tags: tags, Assets: assets,
	}
}

// postCard is the server-rendered feed item.
type postCard struct {
	ID            int64
	Type          string
	TypeLabel     string
	Title         string
	Excerpt       string
	Text          template.HTML
	URL           string
	Datetime      string
	TimeLabel     string
	LikeCount     int
	CommentCount  int
	ImageURL      string
	ImageAlt      string
	Location      string
	CapturedLabel string
	Tags          []string
	// Liked, Bookmarked and CanReact drive the interaction buttons: guests get a
	// sign-in link instead of a write control.
	Liked      bool
	Bookmarked bool
	CanReact   bool
	// OwnerName and OwnerAvatar are the single author shown on every card, so the
	// shared card template needs no page context.
	OwnerName   string
	OwnerAvatar string
}

// feedView drives the home page: the cards, the cursor that continues them and
// the owner-only quick publisher.
type feedView struct {
	pageView
	Posts        []postCard
	NextCursor   string
	LoadMoreURL  string
	Empty        bool
	Composer     bool
	OwnerName    string
	OwnerAvatar  string
	ComposerMode string
	// AIAssist renders the writing assistant inside the composer. It is true only
	// for the owner and only when the AI service is actually usable, so no owner
	// ever gets a control that can only fail.
	AIAssist bool
}

// postPageView drives the detail page.
type postPageView struct {
	pageView
	Card          postCard
	Title         string
	Description   string
	Body          template.HTML
	FullHTML      bool
	Tags          []string
	Location      string
	CapturedLabel string
	OwnerName     string
	OwnerAvatar   string
	// Comment thread and interaction state. The form and its reply controls are
	// rendered only for signed-in readers (docs/PRODUCT.md §3).
	Comments        []commentView
	CommentsEnabled bool
	CanComment      bool
}

// handleHome renders the public feed. A signed-in owner additionally gets the
// quick publisher; readers and guests never receive that DOM.
func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("type")
	switch filter {
	case "", content.TypeArticle, content.TypePhoto:
	default:
		writeFailure(w, r, http.StatusBadRequest, "invalid_type", "该内容筛选类型不存在")
		return
	}
	// One settings read serves both the site brand and the feed shape.
	values := s.displaySettings(r)
	location := loadLocation(values.SiteTimezone)

	page, err := content.Feed(r.Context(), s.db, content.FeedOptions{
		Type:   filter,
		Cursor: r.URL.Query().Get("cursor"),
		Limit:  values.PageSize,
	})
	if err != nil {
		s.writeContentFailure(w, r, "feed", err)
		return
	}

	view := feedView{
		pageView:     s.shellViewWithSettings(r, filter, values),
		Posts:        make([]postCard, 0, len(page.Posts)),
		Empty:        len(page.Posts) == 0,
		ComposerMode: composerMode(filter),
	}
	// One batched query resolves the reader's likes and bookmarks for the page.
	states := s.viewerStates(r.Context(), r, postIDs(page.Posts))
	viewer := s.socialViewer(r)
	view.OwnerName, view.OwnerAvatar = s.authorIdentity(r.Context(), values)
	for _, post := range page.Posts {
		card := s.cardOf(post, location)
		card.CanReact = viewer.signedIn
		card.OwnerName = view.OwnerName
		card.OwnerAvatar = view.OwnerAvatar
		if state, ok := states[post.ID]; ok {
			card.Liked = state.Liked
			card.Bookmarked = state.Bookmarked
		}
		view.Posts = append(view.Posts, card)
	}
	if page.NextCursor != "" {
		view.NextCursor = page.NextCursor
		view.LoadMoreURL = feedURL(filter, page.NextCursor)
	}
	if viewer.owner {
		view.Composer = true
		view.AIAssist = s.aiAvailable(r.Context(), values)
	}
	// The author status card belongs to the home page only: every other page
	// leaves the shared field zero, so no other render path touches the AI cache.
	view.AIStatus = authorStatusPayloadOf(s.authorStatus(r.Context(), values))
	s.indexMetadata(r, &view, values)
	s.render(w, r, http.StatusOK, "home", view)
}

// handlePostPage renders one published post, or the shared 404 page.
func (s *server) handlePostPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	post, err := content.BySlug(r.Context(), s.db, slug)
	if err != nil {
		s.writeContentFailure(w, r, "post page", err)
		return
	}
	values := s.displaySettings(r)
	location := loadLocation(values.SiteTimezone)
	card := s.cardOf(*post, location)
	viewer := s.socialViewer(r)
	card.CanReact = viewer.signedIn
	states := s.viewerStates(r.Context(), r, []int64{post.ID})
	if state, ok := states[post.ID]; ok {
		card.Liked = state.Liked
		card.Bookmarked = state.Bookmarked
	}
	thread, err := social.Comments(r.Context(), s.db, post.ID, viewer.userID)
	if err != nil {
		s.writeSocialFailure(w, r, "post page comments", err)
		return
	}
	ownerName, ownerAvatar := s.authorIdentity(r.Context(), values)
	card.OwnerName, card.OwnerAvatar = ownerName, ownerAvatar
	view := postPageView{
		pageView:        s.shellViewWithSettings(r, filterOf(post.Type), values),
		Card:            card,
		Title:           post.Title,
		Body:            template.HTML(post.BodyHTML), //nolint:gosec // body_html is sanitized at write time
		FullHTML:        true,
		Tags:            post.Tags,
		Comments:        s.commentViewsOf(thread, location, viewer),
		CommentsEnabled: values.CommentsEnabled,
		CanComment:      viewer.signedIn,
		OwnerName:       ownerName,
		OwnerAvatar:     ownerAvatar,
	}
	// The visible summary of the post is computed once and reused: the head, the
	// title fallback and the link preview all describe the same text.
	plain, err := postPlainText(*post)
	if err != nil {
		// Only the summary is missing: the row, the body and the counters are
		// already in hand, so a warning beats failing a page that renders fine.
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "post summary failed",
			slog.String("slug", post.Slug), slog.String("error", err.Error()),
			slog.String("request_id", requestIDFrom(r.Context())))
	}
	if view.Title == "" {
		view.Title = firstLine(plain)
	}
	view.Description = post.Excerpt
	if view.Description == "" {
		view.Description = clampRunes(oneLine(plain), summaryRunes)
	}
	view.Location = post.Location
	view.CapturedLabel = card.CapturedLabel
	s.postingMetadata(r, &view, post, ownerName)
	s.render(w, r, http.StatusOK, "post", view)
}

// postPlainText is the text a reader of this post actually sees. An article body
// goes through the same Markdown pipeline the RSS description uses, because both
// are machine-readable copies of the post and neither may put back what the
// sanitizer removed from the body: without this, an article whose first line is
// a raw tag would publish that tag as its summary, and a heading would publish
// its "#" markers. A moment or a photo caption is stored as plain text already.
func postPlainText(post content.Post) (string, error) {
	if post.Type != content.TypeArticle {
		return post.BodyMarkdown, nil
	}
	text, err := content.MarkdownPlainText(post.BodyMarkdown)
	if err != nil {
		return "", fmt.Errorf("post plain text: %w", err)
	}
	return text, nil
}

// handlePostsAPI serves the public feed as JSON.
func (s *server) handlePostsAPI(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := 0
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeFailure(w, r, http.StatusBadRequest, "invalid_limit", "每页数量需在 1 到 50 之间")
			return
		}
		limit = parsed
	}
	page, err := content.Feed(r.Context(), s.db, content.FeedOptions{
		Type:   query.Get("type"),
		Cursor: query.Get("cursor"),
		Limit:  limit,
	})
	if err != nil {
		s.writeContentFailure(w, r, "feed api", err)
		return
	}
	payload := make([]postPayload, 0, len(page.Posts))
	states := s.viewerStates(r.Context(), r, postIDs(page.Posts))
	for _, post := range page.Posts {
		item := s.postPayloadOf(post)
		applyViewerState(&item, states)
		payload = append(payload, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": payload, "next_cursor": page.NextCursor})
}

// handlePostAPI serves one published post as JSON.
func (s *server) handlePostAPI(w http.ResponseWriter, r *http.Request) {
	post, err := content.BySlug(r.Context(), s.db, r.PathValue("slug"))
	if err != nil {
		s.writeContentFailure(w, r, "post api", err)
		return
	}
	payload := s.postPayloadOf(*post)
	applyViewerState(&payload, s.viewerStates(r.Context(), r, []int64{post.ID}))
	writeJSON(w, http.StatusOK, map[string]any{"data": payload})
}

// handlePostsFallback keeps every /api/v1/posts answer JSON. A sub-path that no
// handler matched is answered with the method it does support, and an unknown
// sub-path is a plain 404, so ServeMux never gets to print plain text.
func (s *server) handlePostsFallback(w http.ResponseWriter, r *http.Request) {
	segments := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/posts/"), "/"), "/")
	allow := ""
	switch {
	case r.URL.Path == "/api/v1/posts/" || strings.HasSuffix(r.URL.Path, "/"):
	case len(segments) == 1 && segments[0] != "":
		// One slug or id: only the read route exists for it.
		allow = http.MethodGet
	case len(segments) == 2:
		allow, _ = postSubRouteAllow(segments[1])
	}
	if allow == "" {
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
		return
	}
	w.Header().Set("Allow", allow)
	writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
}

// postSubRouteAllow maps the interaction sub-route of one post to the methods
// it accepts.
func postSubRouteAllow(verb string) (string, bool) {
	switch verb {
	case "comments":
		return http.MethodGet + ", " + http.MethodPost, true
	case "like", "bookmark":
		return http.MethodPut + ", " + http.MethodDelete, true
	default:
		return "", false
	}
}

// handleCommentsFallback answers method mismatches under /api/v1/comments.
func (s *server) handleCommentsFallback(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/comments/")
	if rest == "" || strings.Contains(rest, "/") {
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
		return
	}
	w.Header().Set("Allow", http.MethodDelete)
	writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
}

// handleMeFallback answers method mismatches under /api/v1/me.
func (s *server) handleMeFallback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/me/bookmarks" {
		w.Header().Set("Allow", http.MethodGet)
		writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
		return
	}
	writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
}

// writeContentFailure maps a content error onto the documented envelope.
func (s *server) writeContentFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	var invalid *content.ValidationError
	switch {
	case errors.As(err, &invalid):
		writeFailure(w, r, http.StatusBadRequest, invalid.Code, invalid.Message)
	case errors.Is(err, content.ErrNotFound):
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
	case errors.Is(err, content.ErrConflict):
		writeFailure(w, r, http.StatusConflict, "conflict", "内容已被其他操作修改，请刷新后重试")
	default:
		s.logger.LogAttrs(r.Context(), slog.LevelError, operation+" failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
	}
}

// cardOf projects a stored post into the feed item the templates render.
func (s *server) cardOf(post content.Post, location *time.Location) postCard {
	card := postCard{
		ID:           post.ID,
		Type:         post.Type,
		TypeLabel:    typeLabels[post.Type],
		Title:        post.Title,
		Excerpt:      post.Excerpt,
		URL:          postPagePrefix + post.Slug,
		LikeCount:    post.LikeCount,
		CommentCount: post.CommentCount,
		Location:     post.Location,
		Tags:         post.Tags,
	}
	card.Datetime, card.TimeLabel = displayTime(post.PublishedAt, location)
	if asset, ok := post.Cover(); ok {
		card.ImageURL = s.mediaOpts().URL(asset.StorageKey)
		card.ImageAlt = asset.AltText
		if card.ImageAlt == "" {
			card.ImageAlt = firstLine(post.BodyMarkdown)
		}
	}
	if post.CapturedAt != "" {
		_, card.CapturedLabel = displayTime(post.CapturedAt, location)
	}
	if post.Type != content.TypeArticle {
		// Moments and photo captions are stored as escaped plain text.
		card.Text = template.HTML(post.BodyHTML) //nolint:gosec // sanitized at write time
	}
	return card
}

// displayTime formats a stored RFC3339 timestamp in the site timezone, keeping
// the machine-readable value for the datetime attribute.
func displayTime(stamp string, location *time.Location) (string, string) {
	if stamp == "" {
		return "", ""
	}
	parsed, err := time.Parse(content.TimestampFormat, stamp)
	if err != nil {
		return "", stamp
	}
	local := parsed.In(location)
	now := time.Now().In(location)
	switch {
	case local.Year() == now.Year() && local.YearDay() == now.YearDay():
		return stamp, local.Format("15:04")
	case local.Year() == now.Year():
		return stamp, local.Format("1月2日")
	default:
		return stamp, local.Format("2006年1月2日")
	}
}

func loadLocation(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return location
}

func firstLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if index := strings.IndexAny(trimmed, "\r\n"); index >= 0 {
		trimmed = strings.TrimSpace(trimmed[:index])
	}
	runes := []rune(trimmed)
	if len(runes) > 120 {
		return string(runes[:120])
	}
	return trimmed
}

// filterOf maps a post type onto the navigation filter it belongs to.
func filterOf(postType string) string {
	if postType == content.TypeArticle || postType == content.TypePhoto {
		return postType
	}
	return ""
}

func composerMode(filter string) string {
	if filter == content.TypeArticle || filter == content.TypePhoto {
		return filter
	}
	return content.TypeMoment
}

// feedURL builds a same-origin feed link carrying the filter and cursor.
func feedURL(filter, cursor string) string {
	values := make([]string, 0, 2)
	if filter != "" {
		values = append(values, "type="+filter)
	}
	if cursor != "" {
		values = append(values, "cursor="+cursor)
	}
	if len(values) == 0 {
		return "/"
	}
	return "/?" + strings.Join(values, "&")
}

// ownerIdentity is the display name shown on every card: Boop has one author.
func (s *server) ownerIdentity(ctx context.Context) (name, avatar string) {
	var storedAvatar sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT display_name, avatar_url FROM users WHERE role = 'owner' ORDER BY id LIMIT 1`).Scan(&name, &storedAvatar)
	if err != nil || name == "" {
		return "站长", ""
	}
	return name, storedAvatar.String
}

// authorIdentity resolves the identity published content is attributed to: the
// owner account plus the avatar the design shows. A configured site avatar wins
// over the account avatar, and an empty result keeps the built-in SVG.
func (s *server) authorIdentity(ctx context.Context, values settings.Values) (name, avatar string) {
	name, avatar = s.ownerIdentity(ctx)
	if values.SiteAvatarURL != "" {
		avatar = values.SiteAvatarURL
	}
	return name, avatar
}
