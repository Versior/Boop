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
