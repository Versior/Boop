package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FeedOptions is the public list query: a type filter, an opaque cursor and a
// page size.
type FeedOptions struct {
	Type   string
	Cursor string
	Limit  int
}

// Page is one feed page plus the cursor that continues it.
type Page struct {
	Posts      []Post
	NextCursor string
}

// Feed returns published, undeleted posts newest first. The page and its
// counters come from a single statement; assets and tags are then fetched with
// two batched queries, so the statement count is constant per page instead of
// growing with the number of posts.
func Feed(ctx context.Context, db *sql.DB, opts FeedOptions) (*Page, error) {
	if db == nil {
		return nil, errors.New("content: feed: nil database")
	}
	switch opts.Type {
	case "", TypeArticle, TypePhoto:
	default:
		// docs/PRODUCT.md §5.1: moments only ever appear in the unfiltered feed.
		return nil, invalid("invalid_type", "该内容筛选类型不存在")
	}
	limit, err := pageLimit(opts.Limit)
	if err != nil {
		return nil, err
	}
	cursorTime, cursorID, err := parseCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}

	query := `SELECT ` + postColumns + ` FROM posts p
		WHERE p.status = 'published' AND p.deleted_at IS NULL`
	args := []any{}
	if opts.Type != "" {
		// Moments only exist in the unfiltered feed (docs/PRODUCT.md §5.1).
		query += ` AND p.type = ?`
		args = append(args, opts.Type)
	}
	if cursorID > 0 {
		query += ` AND (p.published_at < ? OR (p.published_at = ? AND p.id < ?))`
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	query += ` ORDER BY p.published_at DESC, p.id DESC LIMIT ?`
	// One extra row tells us whether another page exists without a count query.
	args = append(args, limit+1)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("content: feed query: %w", err)
	}
	defer rows.Close()

	posts := make([]Post, 0, limit)
	for rows.Next() {
		post, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		posts = append(posts, *post)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("content: feed rows: %w", err)
	}

	page := &Page{}
	if len(posts) > limit {
		posts = posts[:limit]
		page.NextCursor = formatCursor(posts[len(posts)-1])
	}
	page.Posts = posts
	if err := loadRelations(ctx, db, pointers(posts)); err != nil {
		return nil, err
	}
	return page, nil
}

// BySlug loads one published post for the public detail view. Drafts, archived
// and deleted posts are indistinguishable from a missing slug.
func BySlug(ctx context.Context, db *sql.DB, slug string) (*Post, error) {
	if db == nil {
		return nil, errors.New("content: by slug: nil database")
	}
	if strings.TrimSpace(slug) == "" {
		return nil, ErrNotFound
	}
	row := db.QueryRowContext(ctx,
		`SELECT `+postColumns+` FROM posts p
		 WHERE p.slug = ? AND p.status = 'published' AND p.deleted_at IS NULL`, slug)
	post, err := scanPost(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := loadRelations(ctx, db, []*Post{post}); err != nil {
		return nil, err
	}
	return post, nil
}

// loadRelations fills assets and tags of a whole page with two queries.
func loadRelations(ctx context.Context, db *sql.DB, posts []*Post) error {
	if len(posts) == 0 {
		return nil
	}
	byID := make(map[int64]*Post, len(posts))
	ids := make([]any, 0, len(posts))
	placeholders := make([]string, 0, len(posts))
	for _, post := range posts {
		byID[post.ID] = post
		post.Assets = nil
		post.Tags = nil
		ids = append(ids, post.ID)
		placeholders = append(placeholders, "?")
	}
	in := strings.Join(placeholders, ",")

	assetRows, err := db.QueryContext(ctx,
		`SELECT pa.post_id, a.id, a.storage_key, a.mime_type, COALESCE(pa.alt_text, ''), a.size_bytes, a.width, a.height
		 FROM post_assets pa JOIN assets a ON a.id = pa.asset_id
		 WHERE pa.post_id IN (`+in+`)
		 ORDER BY pa.post_id, pa.sort_order, a.id`, ids...)
	if err != nil {
		return fmt.Errorf("content: load assets: %w", err)
	}
	defer assetRows.Close()
	for assetRows.Next() {
		var postID int64
		var asset Asset
		var width, height sql.NullInt64
		if err := assetRows.Scan(&postID, &asset.ID, &asset.StorageKey, &asset.MimeType, &asset.AltText, &asset.SizeBytes, &width, &height); err != nil {
			return fmt.Errorf("content: scan asset: %w", err)
		}
		if width.Valid {
			value := int(width.Int64)
			asset.Width = &value
		}
		if height.Valid {
			value := int(height.Int64)
			asset.Height = &value
		}
		if post := byID[postID]; post != nil {
			post.Assets = append(post.Assets, asset)
		}
	}
	if err := assetRows.Err(); err != nil {
		return fmt.Errorf("content: asset rows: %w", err)
	}

	tagRows, err := db.QueryContext(ctx,
		`SELECT pt.post_id, t.name FROM post_tags pt JOIN tags t ON t.id = pt.tag_id
		 WHERE pt.post_id IN (`+in+`) ORDER BY pt.post_id, t.name`, ids...)
	if err != nil {
		return fmt.Errorf("content: load tags: %w", err)
	}
	defer tagRows.Close()
	for tagRows.Next() {
		var postID int64
		var name string
		if err := tagRows.Scan(&postID, &name); err != nil {
			return fmt.Errorf("content: scan tag: %w", err)
		}
		if post := byID[postID]; post != nil {
			post.Tags = append(post.Tags, name)
		}
	}
	return tagRows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanPost(row scanner) (*Post, error) {
	var post Post
	var coverID sql.NullInt64
	err := row.Scan(&post.ID, &post.Slug, &post.Type, &post.Status, &post.Title, &post.BodyMarkdown, &post.BodyHTML,
		&post.Excerpt, &coverID, &post.Location, &post.CapturedAt, &post.SEOTitle, &post.SEODescription,
		&post.PublishedAt, &post.CreatedAt, &post.UpdatedAt, &post.LikeCount, &post.CommentCount)
	if err != nil {
		return nil, err
	}
	if coverID.Valid {
		post.CoverAssetID = &coverID.Int64
	}
	return &post, nil
}

func pointers(posts []Post) []*Post {
	out := make([]*Post, len(posts))
	for i := range posts {
		out[i] = &posts[i]
	}
	return out
}

func pageLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return DefaultPageSize, nil
	case limit < 0 || limit > MaxPageSize:
		return 0, invalid("invalid_limit", fmt.Sprintf("每页数量需在 1 到 %d 之间", MaxPageSize))
	default:
		return limit, nil
	}
}

// formatCursor encodes the feed position as <published_at,id> (docs/API.md).
func formatCursor(post Post) string {
	return post.PublishedAt + "," + strconv.FormatInt(post.ID, 10)
}

func parseCursor(cursor string) (string, int64, error) {
	if cursor == "" {
		return "", 0, nil
	}
	at, idPart, found := strings.Cut(cursor, ",")
	if !found {
		return "", 0, invalid("invalid_cursor", "游标格式不正确")
	}
	if _, err := time.Parse(TimestampFormat, at); err != nil {
		return "", 0, invalid("invalid_cursor", "游标格式不正确")
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		return "", 0, invalid("invalid_cursor", "游标格式不正确")
	}
	return at, id, nil
}
