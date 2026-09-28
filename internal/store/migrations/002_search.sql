-- Boop migration 002: FTS5 full-text index for posts, kept in sync by triggers.
--
-- Deviation from docs/DATABASE.md, verified experimentally against
-- modernc.org/sqlite: an external content FTS5 table maps its columns to the
-- content table *by name*. Declaring the second column as `body` while posts
-- stores `body_markdown` makes MATCH work but breaks every query that reads a
-- column value or a snippet:
--
--   SELECT title, body FROM post_search WHERE rowid = 1;
--   -> SQL logic error: no such column: T.body
--
-- The column is therefore named body_markdown so it maps onto posts.body_markdown.
-- Search still covers the title, the Markdown body and the excerpt as documented.
--
-- The trigger index stays in sync with posts. Soft-deleted and unpublished rows
-- remain in the index on purpose; every search query must filter them in SQL
-- (status = 'published' AND deleted_at IS NULL) so a restore stays possible.

CREATE VIRTUAL TABLE post_search USING fts5(
  title,
  body_markdown,
  excerpt,
  content='posts',
  content_rowid='id',
  tokenize='unicode61'
);

CREATE TRIGGER posts_search_insert AFTER INSERT ON posts BEGIN
  INSERT INTO post_search(rowid, title, body_markdown, excerpt)
  VALUES (new.id, new.title, new.body_markdown, new.excerpt);
END;

CREATE TRIGGER posts_search_update AFTER UPDATE ON posts BEGIN
  INSERT INTO post_search(post_search, rowid, title, body_markdown, excerpt)
  VALUES ('delete', old.id, old.title, old.body_markdown, old.excerpt);
  INSERT INTO post_search(rowid, title, body_markdown, excerpt)
  VALUES (new.id, new.title, new.body_markdown, new.excerpt);
END;

CREATE TRIGGER posts_search_delete AFTER DELETE ON posts BEGIN
  INSERT INTO post_search(post_search, rowid, title, body_markdown, excerpt)
  VALUES ('delete', old.id, old.title, old.body_markdown, old.excerpt);
END;
