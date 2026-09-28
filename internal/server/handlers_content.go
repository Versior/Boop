package server

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"boop/internal/content"
	"boop/internal/settings"
)

// uploadsPrefix is the public path of stored assets. Task 5 writes files under
// BOOP_DATA_DIR/uploads and must serve them from exactly this prefix.
const uploadsPrefix = "/uploads/"

const (
	contentJSONBytes = 512 << 10
	postPagePrefix   = "/p/"
)

// typeLabels maps a post type to its Chinese chip label.
var typeLabels = map[string]string{
	content.TypeMoment:  "动态",
	content.TypeArticle: "文章",
	content.TypePhoto:   "摄影",
}

type assetPayload struct {
	ID       int64  `json:"id"`
	URL      string `json:"url"`
	MimeType string `json:"mime_type"`
	Width    *int   `json:"width"`
	Height   *int   `json:"height"`
	Alt      string `json:"alt"`
}

type postPayload struct {
	ID           int64          `json:"id"`
	Slug         string         `json:"slug"`
	Type         string         `json:"type"`
	Status       string         `json:"status"`
	Title        string         `json:"title"`
	Body         string         `json:"body"`
	BodyHTML     string         `json:"body_html"`
	Excerpt      string         `json:"excerpt"`
	URL          string         `json:"url"`
	Location     string         `json:"location"`
	CapturedAt   string         `json:"captured_at"`
	PublishedAt  string         `json:"published_at"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
	LikeCount    int            `json:"like_count"`
	CommentCount int            `json:"comment_count"`
	Tags         []string       `json:"tags"`
	Assets       []assetPayload `json:"assets"`
}

func postPayloadOf(post content.Post) postPayload {
	tags := post.Tags
	if tags == nil {
		tags = []string{}
	}
	assets := make([]assetPayload, 0, len(post.Assets))
	for _, asset := range post.Assets {
		assets = append(assets, assetPayload{
			ID:       asset.ID,
			URL:      uploadsPrefix + asset.StorageKey,
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
	values, err := settings.Load(r.Context(), s.db)
	if err != nil {
		// Display path: fall back to the documented defaults and record the
		// failure rather than hiding the feed behind a 500.
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "settings unavailable, using defaults",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		values = settings.Defaults()
	}
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
		pageView:     s.shellView(r, filter),
		Posts:        make([]postCard, 0, len(page.Posts)),
		Empty:        len(page.Posts) == 0,
		ComposerMode: composerMode(filter),
	}
	for _, post := range page.Posts {
		view.Posts = append(view.Posts, s.cardOf(post, location))
	}
	if page.NextCursor != "" {
		view.NextCursor = page.NextCursor
		view.LoadMoreURL = feedURL(filter, page.NextCursor)
	}
	view.OwnerName, view.OwnerAvatar = s.ownerIdentity(r.Context())
	if state, ok := authStateFrom(r.Context()); ok && state.authenticated && state.user.IsOwner() {
		view.Composer = true
	}
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
	values, err := settings.Load(r.Context(), s.db)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "settings unavailable, using defaults",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		values = settings.Defaults()
	}
	card := s.cardOf(*post, loadLocation(values.SiteTimezone))
	view := postPageView{
		pageView: s.shellView(r, filterOf(post.Type)),
		Card:     card,
		Title:    post.Title,
		Body:     template.HTML(post.BodyHTML), //nolint:gosec // body_html is sanitized at write time
		FullHTML: true,
		Tags:     post.Tags,
	}
	view.OwnerName, view.OwnerAvatar = s.ownerIdentity(r.Context())
	if view.Title == "" {
		view.Title = firstLine(post.BodyMarkdown)
	}
	view.Description = post.Excerpt
	if view.Description == "" {
		view.Description = firstLine(post.BodyMarkdown)
	}
	view.Location = post.Location
	view.CapturedLabel = card.CapturedLabel
	s.render(w, r, http.StatusOK, "post", view)
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
	for _, post := range page.Posts {
		payload = append(payload, postPayloadOf(post))
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
	writeJSON(w, http.StatusOK, map[string]any{"data": postPayloadOf(*post)})
}

// handlePostsFallback keeps every /api/v1/posts answer JSON.
func (s *server) handlePostsFallback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/posts/" || strings.HasSuffix(r.URL.Path, "/") {
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
		return
	}
	w.Header().Set("Allow", http.MethodGet)
	writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
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
		card.ImageURL = uploadsPrefix + asset.StorageKey
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
