package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Counts is how much published content the site has, by type. The home page
// shows it in the content-type row, and that number is the whole reason the row
// exists: without it the row would only repeat the navigation entries that
// already reach the same three views.
type Counts struct {
	Total   int
	Article int
	Photo   int
	Moment  int
}

// Span is the window the feed covers: the moment the first post went up and the
// moment the most recent one did, as stored timestamps (RFC3339Nano, UTC). The
// home page's author header shows both, and the query below orders exactly the
// way Feed does, so "最近更新" can never name a post other than the one the feed
// puts on top.
type Span struct {
	First string
	Last  string
}

// PublishedSpan reads both ends of that window in one statement. An empty site
// returns the zero value: the ordering subqueries match no row, so both ends
// come back NULL and the header simply leaves the line out.
func PublishedSpan(ctx context.Context, db *sql.DB) (Span, error) {
	if db == nil {
		return Span{}, errors.New("content: published span: nil database")
	}
	var first, last sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT published_at FROM posts
			  WHERE status = 'published' AND deleted_at IS NULL
			  ORDER BY published_at ASC, id ASC LIMIT 1),
			(SELECT published_at FROM posts
			  WHERE status = 'published' AND deleted_at IS NULL
			  ORDER BY published_at DESC, id DESC LIMIT 1)`).Scan(&first, &last)
	if err != nil {
		return Span{}, fmt.Errorf("content: published span: %w", err)
	}
	return Span{First: first.String, Last: last.String}, nil
}

// ContentCounts counts published, undeleted posts in one statement. The
// visibility rule is exactly the one Feed uses, so a count can never include a
// post the feed would refuse to list.
func ContentCounts(ctx context.Context, db *sql.DB) (Counts, error) {
	if db == nil {
		return Counts{}, errors.New("content: counts: nil database")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT type, COUNT(*) FROM posts
		 WHERE status = 'published' AND deleted_at IS NULL
		 GROUP BY type`)
	if err != nil {
		return Counts{}, fmt.Errorf("content: counts query: %w", err)
	}
	defer rows.Close()

	var counts Counts
	for rows.Next() {
		var postType string
		var total int
		if err := rows.Scan(&postType, &total); err != nil {
			return Counts{}, fmt.Errorf("content: scan count: %w", err)
		}
		switch postType {
		case TypeArticle:
			counts.Article = total
		case TypePhoto:
			counts.Photo = total
		case TypeMoment:
			counts.Moment = total
		}
		counts.Total += total
	}
	if err := rows.Err(); err != nil {
		return Counts{}, fmt.Errorf("content: count rows: %w", err)
	}
	return counts, nil
}
