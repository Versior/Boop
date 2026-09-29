-- Boop migration 005: remove the FTS5 index that 002 created.
--
-- The site has no search any more: the /search page and the /api/v1/search
-- endpoint are gone, so nothing reads post_search. An external-content FTS5
-- table is pure derived data -- it can be rebuilt from posts at any time -- but
-- its three triggers are not: they fire on every insert, update and delete of
-- posts. Leaving them in place would mean every publish on a 1 vCPU machine
-- maintains an index that nothing ever queries, which is the cost this
-- migration exists to stop paying.
--
-- 002_search.sql itself is deliberately left untouched. A migration that has
-- already run is never edited, only superseded; the history of how this schema
-- got here stays readable in the files.
--
-- Dropping is safe in the way that matters: post_search holds no information
-- that is not in posts, so this is a schema change, not a data loss. Rebuilding
-- it (should search ever come back) means a new migration that recreates the
-- table and a command that repopulates it from posts.
DROP TRIGGER IF EXISTS posts_search_insert;
DROP TRIGGER IF EXISTS posts_search_update;
DROP TRIGGER IF EXISTS posts_search_delete;
DROP TABLE IF EXISTS post_search;
