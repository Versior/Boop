package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// StatusCacheKey is the single ai_cache row the author status owns.
const StatusCacheKey = "author_status"

// failureBackoff is how long a failure that left no renderable value keeps the
// model out of reach before a later visit may try again. It is deliberately
// short and fixed: the status is decorative, so retrying is cheap to want, but
// the deadline is what stops a broken endpoint from being called once per page
// view.
const failureBackoff = 5 * time.Minute

const (
	// maxStatusRunes bounds the rendered status: the card is a short
	// introduction, not an article.
	maxStatusRunes = 280
	// maxStatusTopics and maxTopicRunes bound the topic chips.
	maxStatusTopics = 5
	maxTopicRunes   = 24
	// maxSnapshotPosts and maxSnapshotBytes bound what the model may see: the
	// newest published posts, and about 24 KiB of plain text in total.
	maxSnapshotPosts = 20
	maxSnapshotBytes = 24 << 10
	// maxPostRunes bounds one post's contribution to the snapshot, so one long
	// article cannot use the whole budget.
	maxPostRunes = 1200
	// maxStatusTokens leaves room for the text plus the topics.
	maxStatusTokens = 400
)

// fallbackText is the handwritten status shown before anything has been
// generated and whenever generation is impossible. It is plain text, it never
// pretends to come from the model, and it is what the card renders with
// Default set.
const fallbackText = "站长在这里写文章、发动态、拍照片。AI 作者状态会依据最近的公开内容生成。"

// statusSystemPrompt keeps the reply machine-readable: strict JSON is decoded
// with unknown fields and trailing content rejected.
const statusSystemPrompt = "你是个人博客 Boop 的作者状态助手。只输出一个 JSON 对象，不要输出解释、Markdown 代码块或多余文字。"

// Status is the author status Boop shows: the text, its optional topics and the
// metadata the card needs.
type Status struct {
	Text        string
	Topics      []string
	GeneratedAt string
	// Stale reports that the stored value is past its TTL; it may still be
	// accurate when no published content changed since it was generated.
	Stale bool
	// Default reports the handwritten fallback, not generated content.
	Default bool
}

// FallbackStatus returns the handwritten status: what a first visit shows and
// what a failed or impossible generation leaves in place.
func FallbackStatus() Status {
	return Status{Text: fallbackText, Default: true}
}

// storedStatus is the JSON value of the ai_cache row: exactly the two fields the
// card renders.
type storedStatus struct {
	Text   string   `json:"text"`
	Topics []string `json:"topics,omitempty"`
}

// stored is one decoded cache row plus the metadata that decides freshness.
type stored struct {
	Status
	// usable reports whether the row carries a generated value the card can
	// render.
	usable bool
	// placeholder marks the minimal failure row: no generated value and no
	// generation timestamp, with expiresAt holding the moment a later visit may
	// try the model again.
	placeholder     bool
	sourceUpdatedAt string
	expiresAt       string
}

// Decide answers the read side of the author status without ever calling the
// model: what to show now, and whether an upstream refresh is worth starting.
// A value is reused while it is fresh, when the newest published revision is the
// one it was generated from, and when that revision went backwards because
// content was deleted or archived; only a strictly newer published revision asks
// for a refresh.
func Decide(ctx context.Context, db *sql.DB, now time.Time) (Status, bool, error) {
	if db == nil {
		return FallbackStatus(), false, errors.New("ai: author status: nil database")
	}
	cached, found, err := readCache(ctx, db)
	if err != nil {
		return FallbackStatus(), false, err
	}
	if found && !cached.usable {
		// Nothing renderable is stored, so the card shows the handwritten
		// fallback. A failure placeholder carries the deadline of its own
		// failure: while it holds, the model is left alone instead of being
		// called on every visit. A row that is merely broken has no such
		// deadline and is regenerated as soon as there is content to summarize.
		if cached.placeholder && fresh(cached.expiresAt, now) {
			return FallbackStatus(), false, nil
		}
		newest, err := newestPublished(ctx, db)
		if err != nil {
			return FallbackStatus(), false, err
		}
		return FallbackStatus(), newest != "", nil
	}
	if found && fresh(cached.expiresAt, now) {
		// The common path: one read of the cache row decides everything.
		return cached.Status, false, nil
	}
	newest, err := newestPublished(ctx, db)
	if err != nil {
		return FallbackStatus(), false, err
	}
	switch {
	case found && newest == "":
		// Nothing is published (or everything was deleted): there is nothing to
		// summarize, so the stored value stays and no call is made.
		cached.Stale = true
		return cached.Status, false, nil
	case found && !newerContent(newest, cached.sourceUpdatedAt):
		// The stored revision is the live one, or is newer than it because a post
		// was deleted or archived. Either way the stored value still describes
		// every published post, and there is nothing new to summarize.
		cached.Stale = true
		return cached.Status, false, nil
	case found:
		cached.Stale = true
		return cached.Status, true, nil
	case newest == "":
		return FallbackStatus(), false, nil
	default:
		return FallbackStatus(), true, nil
	}
}

// newerContent reports whether the live published revision is strictly newer
// than the one the stored value was generated from. Both are RFC3339 timestamps
// written by the content path, so they are compared as instants rather than as
// strings: deleting or archiving the newest post lowers MAX(updated_at), and a
// lower revision means there is nothing new to summarize. A timestamp that does
// not parse cannot prove freshness, so it asks for a refresh instead of pinning
// the stored value forever.
func newerContent(newest, stored string) bool {
	newestAt, err := time.Parse(time.RFC3339, newest)
	if err != nil {
		return true
	}
	storedAt, err := time.Parse(time.RFC3339, stored)
	if err != nil {
		return true
	}
	return newestAt.After(storedAt)
}

// Refresh generates one new author status from the current published content and
// stores it. It is the only path that calls the model for the status; a failure
// keeps the previous value (or the fallback) and records only its code.
func Refresh(ctx context.Context, db *sql.DB, cfg Config, logger *slog.Logger, now time.Time) (Status, error) {
	if db == nil {
		return Status{}, errors.New("ai: refresh author status: nil database")
	}
	if !cfg.Configured() {
		return Status{}, ErrUnconfigured
	}
	source, err := loadSnapshot(ctx, db)
	if err != nil {
		return Status{}, err
	}
	if source.Newest == "" {
		return Status{}, ErrNoContent
	}
	content, err := complete(ctx, cfg, []message{
		{Role: "system", Content: statusSystemPrompt},
		{Role: "user", Content: statusPrompt(source)},
	}, maxStatusTokens)
	if err != nil {
		recordFailure(ctx, db, logger, err, now)
		return Status{}, err
	}
	generated, err := parseStatus(content)
	if err != nil {
		recordFailure(ctx, db, logger, err, now)
		return Status{}, err
	}
	status, err := writeCache(ctx, db, generated, source.Newest, cfg.ttl(), now)
	if err != nil {
		return Status{}, err
	}
	return status, nil
}

// Guard is the process-local single-flight guard of the author status refresh.
// It has no queue and no waiting: a caller that does not get the slot keeps the
// value it already showed, which is exactly the stale-while-revalidate contract.
type Guard struct {
	mu      sync.Mutex
	running bool
}

// TryStart claims the single refresh slot and reports whether it got it.
func (g *Guard) TryStart() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return false
	}
	g.running = true
	return true
}

// Finish releases the slot.
func (g *Guard) Finish() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = false
}

// RefreshInBackground runs one refresh away from the caller's request, so no
// visitor ever waits for the model loop. Concurrent callers do not duplicate
// generation: only the caller that claims the slot starts the work, and it
// reports false when a refresh was already in flight.
func (g *Guard) RefreshInBackground(db *sql.DB, cfg Config, logger *slog.Logger) bool {
	if !g.TryStart() {
		return false
	}
	go func() {
		defer g.Finish()
		// The request context is deliberately not used: the refresh outlives the
		// response that triggered it, but it still gets a deadline of its own.
		ctx, cancel := context.WithTimeout(context.Background(), BackgroundTimeout)
		defer cancel()
		status, err := Refresh(ctx, db, cfg, logger, time.Now())
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "author status refresh failed",
				slog.String("code", Code(err)), slog.String("error", err.Error()))
			return
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "author status refreshed",
			slog.String("generated_at", status.GeneratedAt))
	}()
	return true
}

// readCache reads and decodes the stored row. A row without a renderable value
// is reported as found but unusable, so one broken value degrades to the
// fallback instead of hiding the home page. Only the failure placeholder, which
// has no generation timestamp either, also carries the retry deadline.
func readCache(ctx context.Context, db *sql.DB) (stored, bool, error) {
	var valueJSON, sourceUpdatedAt, generatedAt, expiresAt string
	err := db.QueryRowContext(ctx,
		`SELECT value_json, source_updated_at, generated_at, expires_at FROM ai_cache WHERE cache_key = ?`,
		StatusCacheKey).Scan(&valueJSON, &sourceUpdatedAt, &generatedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return stored{}, false, nil
	}
	if err != nil {
		return stored{}, false, fmt.Errorf("ai: read author status cache: %w", err)
	}
	broken := stored{expiresAt: expiresAt}
	// A row without a generation timestamp never carries generated content: that
	// is the failure placeholder, and the handwritten fallback is recovered from
	// it.
	if generatedAt == "" {
		broken.placeholder = true
		return broken, true, nil
	}
	var value storedStatus
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return broken, true, nil
	}
	text := strings.TrimSpace(value.Text)
	if text == "" {
		return broken, true, nil
	}
	return stored{
		Status:          Status{Text: text, Topics: boundList(value.Topics, maxStatusTopics, maxTopicRunes), GeneratedAt: generatedAt},
		usable:          true,
		sourceUpdatedAt: sourceUpdatedAt,
		expiresAt:       expiresAt,
	}, true, nil
}

// writeCache stores one generated value with the content revision it came from
// and the next expiry, clearing any earlier failure.
func writeCache(ctx context.Context, db *sql.DB, value storedStatus, sourceUpdatedAt string, ttl time.Duration, now time.Time) (Status, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return Status{}, fmt.Errorf("ai: encode author status: %w", err)
	}
	generatedAt := stamp(now)
	_, err = db.ExecContext(ctx,
		`INSERT INTO ai_cache(cache_key, value_json, source_updated_at, generated_at, expires_at, last_error)
		 VALUES(?,?,?,?,?,'')
		 ON CONFLICT(cache_key) DO UPDATE SET
			value_json = excluded.value_json,
			source_updated_at = excluded.source_updated_at,
			generated_at = excluded.generated_at,
			expires_at = excluded.expires_at,
			last_error = ''`,
		StatusCacheKey, string(raw), sourceUpdatedAt, generatedAt, stamp(now.Add(ttl)))
	if err != nil {
		return Status{}, fmt.Errorf("ai: store author status: %w", err)
	}
	return Status{Text: value.Text, Topics: value.Topics, GeneratedAt: generatedAt}, nil
}

// recordFailure keeps whatever the card can render and stores only the stable
// code, so an operator can see why the status stopped updating without a prompt,
// a key or a model reply landing in the database. A stored value is never
// touched: only its code changes. When nothing renderable is stored yet, a
// minimal placeholder row carries the code and the retry deadline instead, so
// the handwritten fallback keeps rendering while the next visit backs off
// rather than calling the model again immediately.
func recordFailure(ctx context.Context, db *sql.DB, logger *slog.Logger, err error, now time.Time) {
	code := Code(err)
	if code == "" {
		code = CodeUpstream
	}
	cached, found, readErr := readCache(ctx, db)
	if readErr != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "author status failure could not be recorded",
			slog.String("error", readErr.Error()))
		return
	}
	var execErr error
	if found && cached.usable {
		// The stored value is what visitors keep seeing: only the code is
		// written, so a failed retry cannot overwrite the text, the source
		// revision or the expiry it is served with.
		_, execErr = db.ExecContext(ctx,
			`UPDATE ai_cache SET last_error = ? WHERE cache_key = ?`, code, StatusCacheKey)
	} else {
		// value_json and generated_at stay empty, so the read side recognises the
		// placeholder, renders the fallback and waits for the deadline.
		_, execErr = db.ExecContext(ctx,
			`INSERT INTO ai_cache(cache_key, value_json, source_updated_at, generated_at, expires_at, last_error)
			 VALUES(?, '', '', '', ?, ?)
			 ON CONFLICT(cache_key) DO UPDATE SET
				value_json = excluded.value_json,
				source_updated_at = excluded.source_updated_at,
				generated_at = excluded.generated_at,
				expires_at = excluded.expires_at,
				last_error = excluded.last_error`,
			StatusCacheKey, stamp(now.Add(failureBackoff)), code)
	}
	if execErr != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "author status failure could not be recorded",
			slog.String("error", execErr.Error()))
	}
}

// newestPublished returns the most recently updated published post, or "" when
// nothing is published. Every timestamp is written by the content path in one
// RFC3339 format, so MAX(updated_at) is the newest revision; Decide compares it
// with the stored one as an instant rather than as a string.
func newestPublished(ctx context.Context, db *sql.DB) (string, error) {
	var newest string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(updated_at), '') FROM posts WHERE status = 'published' AND deleted_at IS NULL`).Scan(&newest)
	if err != nil {
		return "", fmt.Errorf("ai: newest published content: %w", err)
	}
	return newest, nil
}

// snapshot is the bounded view of published content the model is allowed to see.
type snapshot struct {
	Text string
	// Newest is the newest published updated_at when the snapshot was taken; it
	// is stored verbatim so the next visit can compare it with the live value.
	Newest string
}

// loadSnapshot reads the newest published posts and renders them as plain text
// within the documented bounds: at most maxSnapshotPosts posts and about
// maxSnapshotBytes in total.
func loadSnapshot(ctx context.Context, db *sql.DB) (snapshot, error) {
	newest, err := newestPublished(ctx, db)
	if err != nil {
		return snapshot{}, err
	}
	if newest == "" {
		return snapshot{}, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT type, title, excerpt, body_markdown FROM posts
		 WHERE status = 'published' AND deleted_at IS NULL
		 ORDER BY published_at DESC, id DESC LIMIT ?`, maxSnapshotPosts)
	if err != nil {
		return snapshot{}, fmt.Errorf("ai: read published content: %w", err)
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var kind, title, excerpt, body string
		if err := rows.Scan(&kind, &title, &excerpt, &body); err != nil {
			return snapshot{}, fmt.Errorf("ai: scan published content: %w", err)
		}
		entry := renderEntry(kind, title, excerpt, body)
		if b.Len()+len(entry) > maxSnapshotBytes {
			break
		}
		b.WriteString(entry)
	}
	if err := rows.Err(); err != nil {
		return snapshot{}, fmt.Errorf("ai: published content rows: %w", err)
	}
	return snapshot{Text: b.String(), Newest: newest}, nil
}

// renderEntry renders one post as plain text for the prompt.
func renderEntry(kind, title, excerpt, body string) string {
	var b strings.Builder
	b.WriteString("- [" + kindLabel(kind) + "] ")
	if headline := truncateRunes(title, 120); headline != "" {
		b.WriteString(headline)
	} else {
		b.WriteString("（无标题）")
	}
	if summary := truncateRunes(excerpt, 200); summary != "" {
		b.WriteString("\n  摘要：" + summary)
	}
	if text := truncateRunes(strings.TrimSpace(body), maxPostRunes); text != "" {
		b.WriteString("\n  正文：" + text)
	}
	b.WriteString("\n")
	return b.String()
}

func kindLabel(kind string) string {
	switch kind {
	case "article":
		return "文章"
	case "photo":
		return "摄影"
	default:
		return "动态"
	}
}

// statusPrompt asks for exactly the two documented fields.
func statusPrompt(source snapshot) string {
	return "以下是站长最近发布的公开内容节选：\n\n" + source.Text +
		"\n请用不超过 280 字的中文写一段第一人称的作者状态，并给出不超过 5 个不超过 24 字的主题词，" +
		"只输出 {\"text\":\"...\",\"topics\":[\"...\"]}。"
}

// parseStatus decodes one strict JSON object and bounds it to what the card can
// render.
func parseStatus(content string) (storedStatus, error) {
	var reply storedStatus
	if err := decodeStrict(content, &reply); err != nil {
		return storedStatus{}, err
	}
	text := truncateRunes(strings.TrimSpace(reply.Text), maxStatusRunes)
	if text == "" {
		return storedStatus{}, ErrInvalidReply
	}
	return storedStatus{Text: text, Topics: boundList(reply.Topics, maxStatusTopics, maxTopicRunes)}, nil
}

// boundList trims, drops blanks, de-duplicates case-insensitively and caps a list
// of short values, so anything a model sends still fits the UI.
func boundList(values []string, maxItems, maxRunes int) []string {
	out := make([]string, 0, maxItems)
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		item := truncateRunes(strings.TrimSpace(value), maxRunes)
		if item == "" {
			continue
		}
		key := strings.ToLower(item)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
		if len(out) == maxItems {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// truncateRunes cuts text to limit characters without splitting one.
func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return strings.TrimSpace(string([]rune(text)[:limit]))
}

// fresh reports whether the stored expiry is still in the future. An unreadable
// expiry counts as expired, so a broken row is regenerated instead of pinned.
func fresh(expiresAt string, now time.Time) bool {
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return false
	}
	return now.Before(expiry)
}

// stamp is the format every ai_cache timestamp is written in.
func stamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}
