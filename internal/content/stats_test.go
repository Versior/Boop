package content

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// The home page shows these numbers, so they must be the same population the
// feed lists: drafts, archived and soft-deleted posts are excluded, and moments
// are counted even though they have no filter of their own.
func TestContentCountsFollowTheFeedVisibilityRule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assetID := f.insertAsset(t, "photos/counts.jpg")

	seed := []Input{
		{Type: TypeMoment, Status: StatusPublished, Body: "公开动态"},
		{Type: TypeMoment, Status: StatusPublished, Body: "第二条公开动态"},
		{Type: TypeArticle, Status: StatusPublished, Title: "公开文章", Body: "正文"},
		{Type: TypeArticle, Status: StatusPublished, Title: "第二篇公开文章", Body: "正文"},
		{Type: TypeArticle, Status: StatusPublished, Title: "第三篇文章", Body: "正文"},
		{Type: TypePhoto, Status: StatusPublished, AssetIDs: []int64{assetID}},
		{Type: TypeMoment, Status: StatusDraft, Body: "草稿动态"},
		{Type: TypeArticle, Status: StatusArchived, Title: "归档文章", Body: "正文"},
	}
	for i, in := range seed {
		if _, err := Create(ctx, f.db, f.ownerID, in, testNow.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	removed, err := Create(ctx, f.db, f.ownerID, Input{Type: TypePhoto, Status: StatusPublished, AssetIDs: []int64{assetID}}, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("Create deleted: %v", err)
	}
	if err := Delete(ctx, f.db, removed.ID, testNow.Add(time.Hour)); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	counts, err := ContentCounts(ctx, f.db)
	if err != nil {
		t.Fatalf("ContentCounts: %v", err)
	}
	want := Counts{Total: 6, Article: 3, Photo: 1, Moment: 2}
	if counts != want {
		t.Errorf("ContentCounts = %+v, want %+v", counts, want)
	}

	// The counts agree with the lists they label, view by view.
	all, err := Feed(ctx, f.db, FeedOptions{})
	if err != nil {
		t.Fatalf("Feed all: %v", err)
	}
	if len(all.Posts) != counts.Total {
		t.Errorf("the unfiltered feed has %d posts, but the row says %d", len(all.Posts), counts.Total)
	}
	for _, view := range []struct {
		kind string
		want int
	}{
		{TypeArticle, counts.Article},
		{TypePhoto, counts.Photo},
	} {
		page, err := Feed(ctx, f.db, FeedOptions{Type: view.kind})
		if err != nil {
			t.Fatalf("Feed %s: %v", view.kind, err)
		}
		if len(page.Posts) != view.want {
			t.Errorf("the %s feed has %d posts, but the row says %d", view.kind, len(page.Posts), view.want)
		}
	}
}

// An empty site is the state every deployment starts in: the row still renders,
// and every entry reads zero rather than being missing.
func TestContentCountsOnAnEmptySite(t *testing.T) {
	f := newFixture(t)
	counts, err := ContentCounts(context.Background(), f.db)
	if err != nil {
		t.Fatalf("ContentCounts: %v", err)
	}
	if counts != (Counts{}) {
		t.Errorf("ContentCounts = %+v, want every field zero", counts)
	}
}

// One statement, however many posts there are: the counts are read on every home
// page render, right next to the feed query, so they must not add a query per
// type or per post.
func TestContentCountsIssueOneQuery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		if _, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: fmt.Sprintf("动态 %d", i)}, testNow); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	f.mirrorPosts(t)

	if got := f.measure(t, func() { mustContentCounts(t, ctx, f.counted) }); got != 1 {
		t.Errorf("ContentCounts issued %d statements, want 1", got)
	}
}

func mustContentCounts(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := ContentCounts(ctx, db); err != nil {
		t.Fatalf("ContentCounts: %v", err)
	}
}

// The author header dates the site from these two stamps, so they have to be
// the ends of the list the reader is looking at: the same visibility rule as
// Feed, and the same ordering, so "最近更新" cannot name a post the feed puts
// somewhere else.
func TestPublishedSpanMatchesTheFeedEnds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assetID := f.insertAsset(t, "photos/span.jpg")

	seed := []Input{
		{Type: TypeMoment, Status: StatusPublished, Body: "最早的一条"},
		{Type: TypeArticle, Status: StatusPublished, Title: "中间的文章", Body: "正文"},
		{Type: TypePhoto, Status: StatusPublished, AssetIDs: []int64{assetID}},
		{Type: TypeMoment, Status: StatusDraft, Body: "草稿不算"},
	}
	for i, in := range seed {
		if _, err := Create(ctx, f.db, f.ownerID, in, testNow.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	// A soft-deleted post is newer than everything above and still not an end:
	// the header dates the site by what the feed is willing to list.
	removed, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: "已删除"}, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("Create deleted: %v", err)
	}
	if err := Delete(ctx, f.db, removed.ID, testNow.Add(time.Hour)); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	span, err := PublishedSpan(ctx, f.db)
	if err != nil {
		t.Fatalf("PublishedSpan: %v", err)
	}
	page, err := Feed(ctx, f.db, FeedOptions{})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(page.Posts) == 0 {
		t.Fatal("the fixture feed is empty")
	}
	if want := page.Posts[len(page.Posts)-1].PublishedAt; span.First != want {
		t.Errorf("Span.First = %q, want the oldest listed post %q", span.First, want)
	}
	if want := page.Posts[0].PublishedAt; span.Last != want {
		t.Errorf("Span.Last = %q, want the newest listed post %q", span.Last, want)
	}
}

// A site with nothing published yet has no window to describe, and the header
// leaves the line out rather than dating it from a draft.
func TestPublishedSpanOnAnEmptySite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusDraft, Body: "只有草稿"}, testNow); err != nil {
		t.Fatalf("Create: %v", err)
	}
	span, err := PublishedSpan(ctx, f.db)
	if err != nil {
		t.Fatalf("PublishedSpan: %v", err)
	}
	if span != (Span{}) {
		t.Errorf("PublishedSpan = %+v, want both ends empty", span)
	}
}

// Both ends arrive in one statement: the header is rendered beside the feed
// query on every first page, so it must not add a query per end.
func TestPublishedSpanIssuesOneQuery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		if _, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: fmt.Sprintf("动态 %d", i)}, testNow); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	f.mirrorPosts(t)

	got := f.measure(t, func() {
		if _, err := PublishedSpan(ctx, f.counted); err != nil {
			t.Fatalf("PublishedSpan: %v", err)
		}
	})
	if got != 1 {
		t.Errorf("PublishedSpan issued %d statements, want 1", got)
	}
}
