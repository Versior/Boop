package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"boop/internal/media"
	"boop/internal/store"
)

// postColumns is the shared projection for feed and detail reads: the row plus
// the two counters are resolved in the same statement, so a page of posts never
// costs a query per post. The comment count matches the public thread: a reply
// whose parent is hidden is not counted either.
const postColumns = `p.id, p.slug, p.type, p.status, p.title, p.body_markdown, p.body_html,
	p.excerpt, p.cover_asset_id, p.location, COALESCE(p.captured_at, ''), p.seo_title, p.seo_description,
	COALESCE(p.published_at, ''), p.created_at, p.updated_at,
	(SELECT COUNT(*) FROM likes l WHERE l.post_id = p.id),
	(SELECT COUNT(*) FROM comments c WHERE c.post_id = p.id AND c.status = 'approved' AND c.deleted_at IS NULL
		AND (c.parent_id IS NULL OR EXISTS(
			SELECT 1 FROM comments parent
			WHERE parent.id = c.parent_id AND parent.status = 'approved' AND parent.deleted_at IS NULL)))`

// Create stores a new post owned by ownerID: slug allocation, the row itself,
// its asset links and its tags all happen in one transaction, so a rejected
// relationship write leaves nothing behind. ownerID is required because a
// referenced asset must belong to the writer and be an image.
func Create(ctx context.Context, db *sql.DB, ownerID int64, in Input, now time.Time) (*Post, error) {
	if db == nil {
		return nil, errors.New("content: create: nil database")
	}
	if ownerID <= 0 {
		return nil, errors.New("content: create: owner id is required")
	}
	prepared, err := validateInput(in)
	if err != nil {
		return nil, err
	}
	bodyHTML, err := RenderBody(prepared.Type, prepared.Body)
	if err != nil {
		return nil, fmt.Errorf("content: render body: %w", err)
	}

	stamp := timestamp(now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("content: begin: %w", err)
	}
	defer tx.Rollback()

	if err := checkAssets(ctx, tx, ownerID, prepared.AssetIDs); err != nil {
		return nil, err
	}

	slug, err := allocateSlug(ctx, tx, prepared, now)
	if err != nil {
		return nil, err
	}

	publishedAt := any(nil)
	if prepared.publishing {
		publishedAt = stamp
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO posts(slug, type, status, title, body_markdown, body_html, excerpt, cover_asset_id,
			location, captured_at, published_at, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		slug, prepared.Type, prepared.Status, prepared.Title, prepared.Body, bodyHTML, prepared.Excerpt,
		coverAssetID(prepared.AssetIDs), prepared.Location, nullString(prepared.CapturedAt), publishedAt, stamp, stamp)
	if err != nil {
		return nil, fmt.Errorf("content: insert post: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("content: post id: %w", err)
	}
	if err := replaceAssets(ctx, tx, id, prepared.AssetIDs); err != nil {
		return nil, err
	}
	if err := replaceTags(ctx, tx, id, prepared.Tags); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("content: commit: %w", err)
	}
	return ByID(ctx, db, id)
}

// Update applies a partial change under an optimistic lock: the caller must send
// the updated_at it read, and a row that moved on is reported as a conflict.
func Update(ctx context.Context, db *sql.DB, ownerID int64, id int64, patch Patch, now time.Time) (*Post, error) {
	if db == nil {
		return nil, errors.New("content: update: nil database")
	}
	if ownerID <= 0 {
		return nil, errors.New("content: update: owner id is required")
	}
	if _, err := time.Parse(TimestampFormat, patch.UpdatedAt); err != nil {
		return nil, invalid("invalid_updated_at", "缺少或错误的 updated_at")
	}

	current, err := ByID(ctx, db, id)
	if err != nil {
		return nil, err
	}
	merged := Input{
		Type:       current.Type,
		Status:     current.Status,
		Title:      current.Title,
		Body:       current.BodyMarkdown,
		Excerpt:    current.Excerpt,
		Location:   current.Location,
		CapturedAt: current.CapturedAt,
		Tags:       current.Tags,
	}
	for _, asset := range current.Assets {
		merged.AssetIDs = append(merged.AssetIDs, asset.ID)
	}
	if patch.Status != nil {
		merged.Status = *patch.Status
	}
	if patch.Title != nil {
		merged.Title = *patch.Title
	}
	if patch.Body != nil {
		merged.Body = *patch.Body
	}
	if patch.Excerpt != nil {
		merged.Excerpt = *patch.Excerpt
	}
	if patch.Location != nil {
		merged.Location = *patch.Location
	}
	if patch.CapturedAt != nil {
		merged.CapturedAt = *patch.CapturedAt
	}
	if patch.AssetIDs != nil {
		merged.AssetIDs = *patch.AssetIDs
	}
	if patch.Tags != nil {
		merged.Tags = *patch.Tags
	}

	prepared, err := validateInput(merged)
	if err != nil {
		return nil, err
	}
	bodyHTML, err := RenderBody(prepared.Type, prepared.Body)
	if err != nil {
		return nil, fmt.Errorf("content: render body: %w", err)
	}

	stamp := nextStamp(now, current.UpdatedAt)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("content: begin: %w", err)
	}
	defer tx.Rollback()

	if err := checkAssets(ctx, tx, ownerID, prepared.AssetIDs); err != nil {
		return nil, err
	}

	// published_at is set on the first transition to published and then frozen so
	// the feed position of a post never moves because of an edit.
	publishedAt := current.PublishedAt
	if prepared.publishing && publishedAt == "" {
		publishedAt = stamp
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE posts SET status = ?, title = ?, body_markdown = ?, body_html = ?, excerpt = ?, cover_asset_id = ?,
			location = ?, captured_at = ?, published_at = ?, updated_at = ?
		 WHERE id = ? AND updated_at = ? AND deleted_at IS NULL`,
		prepared.Status, prepared.Title, prepared.Body, bodyHTML, prepared.Excerpt, coverAssetID(prepared.AssetIDs),
		prepared.Location, nullString(prepared.CapturedAt), nullString(publishedAt), stamp, id, patch.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("content: update post: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("content: update post: %w", err)
	}
	if affected == 0 {
		return nil, ErrConflict
	}

	if patch.AssetIDs != nil {
		if err := replaceAssets(ctx, tx, id, prepared.AssetIDs); err != nil {
			return nil, err
		}
	}
	if patch.Tags != nil {
		if err := replaceTags(ctx, tx, id, prepared.Tags); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("content: commit: %w", err)
	}
	return ByID(ctx, db, id)
}

// Delete soft deletes a post: the row and its assets survive for the cleanup
// command, but every public query stops seeing it.
func Delete(ctx context.Context, db *sql.DB, id int64, now time.Time) error {
	if db == nil {
		return errors.New("content: delete: nil database")
	}
	result, err := db.ExecContext(ctx,
		`UPDATE posts SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		timestamp(now), timestamp(now), id)
	if err != nil {
		return fmt.Errorf("content: delete post: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("content: delete post: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ByID loads one post regardless of status; owner-facing updates use it.
func ByID(ctx context.Context, db *sql.DB, id int64) (*Post, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+postColumns+` FROM posts p WHERE p.id = ? AND p.deleted_at IS NULL`, id)
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

// ByIDs loads published, undeleted posts by id in one statement, preserving the
// caller's id order and skipping ids that are no longer publicly readable. The
// bookmark list uses it, so a bookmarked post that was unpublished or deleted in
// the meantime simply disappears instead of leaking.
func ByIDs(ctx context.Context, db *sql.DB, ids []int64) ([]Post, error) {
	if db == nil {
		return nil, errors.New("content: by ids: nil database")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+postColumns+` FROM posts p
		 WHERE p.status = 'published' AND p.deleted_at IS NULL AND p.id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("content: by ids query: %w", err)
	}
	defer rows.Close()

	found := make(map[int64]Post, len(ids))
	for rows.Next() {
		post, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		found[post.ID] = *post
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("content: by ids rows: %w", err)
	}

	posts := make([]Post, 0, len(found))
	for _, id := range ids {
		if post, ok := found[id]; ok {
			posts = append(posts, post)
		}
	}
	if err := loadRelations(ctx, db, pointers(posts)); err != nil {
		return nil, err
	}
	return posts, nil
}

// checkAssets verifies every referenced asset in one statement: each id must
// exist, belong to ownerID and be an image this deployment produced. Looking at
// the id alone would let a post reference an asset the writer does not own.
func checkAssets(ctx context.Context, tx *sql.Tx, ownerID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, ownerID)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, mime_type FROM assets WHERE owner_user_id = ? AND id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return fmt.Errorf("content: check assets: %w", err)
	}
	defer rows.Close()

	found := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var mime string
		if err := rows.Scan(&id, &mime); err != nil {
			return fmt.Errorf("content: scan asset: %w", err)
		}
		found[id] = mime
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("content: asset rows: %w", err)
	}

	for _, id := range ids {
		mime, ok := found[id]
		if !ok {
			// Unknown and not-yours are the same answer on purpose: a writer must
			// not be able to probe for other people's assets.
			return invalid("invalid_asset", "关联的图片不存在")
		}
		if !media.IsImageMIME(mime) {
			return invalid("invalid_asset", "只能关联图片资源")
		}
	}
	return nil
}

// allocateSlug derives a slug and resolves collisions with a numeric suffix,
// retrying while a concurrent writer holds the same prefix.
func allocateSlug(ctx context.Context, tx *sql.Tx, in validated, now time.Time) (string, error) {
	base := Slugify(slugBase(in.Input))
	if base == "" {
		base = fallbackSlug(in.Type, now)
	}
	for attempt := 1; attempt <= maxSlugAttempts; attempt++ {
		candidate := base
		if attempt > 1 {
			candidate = fmt.Sprintf("%s-%d", base, attempt)
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM posts WHERE slug = ?`, candidate).Scan(&taken); err != nil {
			return "", fmt.Errorf("content: check slug: %w", err)
		}
		if taken == 0 {
			return candidate, nil
		}
	}
	return "", invalid("invalid_slug", "无法生成唯一链接，请修改标题后重试")
}

func replaceAssets(ctx context.Context, tx *sql.Tx, postID int64, ids []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM post_assets WHERE post_id = ?`, postID); err != nil {
		return fmt.Errorf("content: clear post assets: %w", err)
	}
	for order, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO post_assets(post_id, asset_id, sort_order, alt_text) VALUES(?,?,?,?)`,
			postID, id, order, ""); err != nil {
			return fmt.Errorf("content: link asset %d: %w", id, err)
		}
	}
	return nil
}

// replaceTags links the post to existing tags, creating the missing ones.
func replaceTags(ctx context.Context, tx *sql.Tx, postID int64, names []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM post_tags WHERE post_id = ?`, postID); err != nil {
		return fmt.Errorf("content: clear post tags: %w", err)
	}
	for _, name := range names {
		id, err := upsertTag(ctx, tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO post_tags(post_id, tag_id) VALUES(?,?) ON CONFLICT DO NOTHING`, postID, id); err != nil {
			return fmt.Errorf("content: link tag %q: %w", name, err)
		}
	}
	return nil
}

// upsertTag resolves a tag case-insensitively (tags.name is COLLATE NOCASE
// UNIQUE) and derives a unique slug for new rows.
func upsertTag(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE name = ? COLLATE NOCASE`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("content: find tag %q: %w", name, err)
	}

	base := Slugify(name)
	if base == "" {
		base = "tag"
	}
	for attempt := 1; attempt <= maxSlugAttempts; attempt++ {
		slug := base
		if attempt > 1 {
			slug = fmt.Sprintf("%s-%d", base, attempt)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO tags(name, slug) VALUES(?,?)`, name, slug)
		if err != nil {
			if store.IsUniqueViolation(err) {
				// Either the name appeared concurrently (then reuse it) or the
				// slug is held by a different name (then try the next suffix).
				var existing int64
				if lookupErr := tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE name = ? COLLATE NOCASE`, name).Scan(&existing); lookupErr == nil {
					return existing, nil
				} else if !errors.Is(lookupErr, sql.ErrNoRows) {
					return 0, fmt.Errorf("content: resolve tag %q: %w", name, lookupErr)
				}
				continue
			}
			return 0, fmt.Errorf("content: insert tag %q: %w", name, err)
		}
		inserted, err := result.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("content: tag id: %w", err)
		}
		return inserted, nil
	}
	return 0, invalid("invalid_tags", "标签冲突，请更换标签")
}

func coverAssetID(ids []int64) any {
	if len(ids) == 0 {
		return nil
	}
	return ids[0]
}

func timestamp(at time.Time) string {
	return at.UTC().Format(TimestampFormat)
}

// nextStamp keeps updated_at strictly increasing for one post. Two writes inside
// the same clock tick (Windows clocks can be coarse) must not produce the same
// revision string, otherwise the optimistic lock cannot tell them apart.
func nextStamp(at time.Time, previous string) string {
	if previous == "" {
		return timestamp(at)
	}
	prior, err := time.Parse(TimestampFormat, previous)
	if err != nil || at.After(prior) {
		return timestamp(at)
	}
	return timestamp(prior.Add(time.Microsecond))
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
