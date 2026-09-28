-- Boop migration 001: content, identity and interaction schema.
-- Source of truth: docs/DATABASE.md (v0.1). Published migrations are immutable;
-- schema changes must be added as a new numbered file.

CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  email TEXT NOT NULL COLLATE NOCASE UNIQUE,
  password_hash TEXT,
  display_name TEXT NOT NULL,
  avatar_url TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL CHECK (role IN ('owner','reader')),
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_login_at TEXT
);

CREATE TABLE oauth_accounts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL CHECK (provider IN ('github')),
  provider_user_id TEXT NOT NULL,
  provider_login TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(provider, provider_user_id)
);

CREATE TABLE sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash BLOB NOT NULL UNIQUE,
  csrf_token_hash BLOB NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '',
  ip_prefix TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE posts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  slug TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL CHECK (type IN ('moment','article','photo')),
  status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','published','archived')),
  title TEXT NOT NULL DEFAULT '',
  body_markdown TEXT NOT NULL DEFAULT '',
  body_html TEXT NOT NULL DEFAULT '',
  excerpt TEXT NOT NULL DEFAULT '',
  cover_asset_id INTEGER,
  location TEXT NOT NULL DEFAULT '',
  captured_at TEXT,
  seo_title TEXT NOT NULL DEFAULT '',
  seo_description TEXT NOT NULL DEFAULT '',
  published_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  deleted_at TEXT
);
CREATE INDEX idx_posts_feed ON posts(status, published_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_posts_type_feed ON posts(type, status, published_at DESC) WHERE deleted_at IS NULL;

CREATE TABLE assets (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_user_id INTEGER NOT NULL REFERENCES users(id),
  storage_key TEXT NOT NULL UNIQUE,
  original_name TEXT NOT NULL,
  mime_type TEXT NOT NULL,
  size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
  width INTEGER,
  height INTEGER,
  sha256 BLOB NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE post_assets (
  post_id INTEGER NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE RESTRICT,
  sort_order INTEGER NOT NULL DEFAULT 0,
  alt_text TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(post_id, asset_id)
);
CREATE INDEX idx_post_assets_order ON post_assets(post_id, sort_order);

CREATE TABLE tags (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL COLLATE NOCASE UNIQUE,
  slug TEXT NOT NULL UNIQUE
);

CREATE TABLE post_tags (
  post_id INTEGER NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY(post_id, tag_id)
);

CREATE TABLE comments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  post_id INTEGER NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  parent_id INTEGER REFERENCES comments(id) ON DELETE CASCADE,
  body TEXT NOT NULL CHECK(length(body) BETWEEN 1 AND 2000),
  status TEXT NOT NULL CHECK(status IN ('pending','approved','rejected')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  deleted_at TEXT
);
CREATE INDEX idx_comments_post ON comments(post_id, status, created_at) WHERE deleted_at IS NULL;
CREATE INDEX idx_comments_queue ON comments(status, created_at) WHERE status='pending' AND deleted_at IS NULL;

CREATE TABLE likes (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  post_id INTEGER NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  PRIMARY KEY(user_id, post_id)
);

CREATE TABLE bookmarks (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  post_id INTEGER NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  PRIMARY KEY(user_id, post_id)
);
CREATE INDEX idx_bookmarks_user ON bookmarks(user_id, created_at DESC);

CREATE TABLE settings (
  key TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE secret_settings (
  key TEXT PRIMARY KEY,
  nonce BLOB NOT NULL,
  ciphertext BLOB NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE ai_cache (
  cache_key TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  source_updated_at TEXT NOT NULL,
  generated_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
);
