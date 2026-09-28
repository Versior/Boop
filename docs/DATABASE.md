# Boop 数据库结构 v0.1

SQLite 时间统一存 UTC RFC3339 字符串；布尔值存 `INTEGER NOT NULL CHECK(value IN (0,1))`；应用启动时依序执行嵌入迁移。

```sql
CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);

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
CREATE UNIQUE INDEX idx_assets_owner_hash ON assets(owner_user_id, sha256);

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
  key TEXT NOT NULL PRIMARY KEY,
  value_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE secret_settings (
  key TEXT NOT NULL PRIMARY KEY,
  nonce BLOB NOT NULL,
  ciphertext BLOB NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE ai_cache (
  cache_key TEXT NOT NULL PRIMARY KEY,
  value_json TEXT NOT NULL,
  source_updated_at TEXT NOT NULL,
  generated_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
);

CREATE VIRTUAL TABLE post_search USING fts5(
  title,
  body_markdown,
  excerpt,
  content='posts',
  content_rowid='id',
  tokenize='unicode61'
);
```

> 外部内容表按 **列名** 映射内容表：`post_search` 的列名必须与 `posts` 的列名一致。列为
> `body` 时 `MATCH` 仍可命中，但 `SELECT title, body FROM post_search` 与 `snippet()` 会报
> `no such column: T.body`，故此处使用 `body_markdown`（已在 modernc.org/sqlite 上实测确认）。

## 约束与事务规则

- 创建评论时在同一个事务里校验：内容已发布且未删除，父评论属于同一文章、本身没有父级、且当前为 `approved`（回复隐藏的父评论会让附件评论不可达）。
- 评论的 `comments.enabled` 与 `comments.moderation_enabled` 也在这个事务内读取：状态由事务内的值决定，调用方不能传入先前读到的值，写入与判断之间不会被并发设置变更插入，读取失败（值无法解码）则回滚且不写行。
- 设置写入（`settings` 与 `secret_settings`）是同一事务：类型与范围校验在事务之前完成，任何一个字段被拒都不会留下部分写入。密钥用 `BOOP_MASTER_KEY` 的 AES-256-GCM 加密后存入 `secret_settings`，每次写入都生成新的 nonce（同一明文两次加密结果不同），并以该密钥自己的 settings key 作为 GCM additional data：密文只能解回同一个 key，把一条密文换到另一个 key 上同样认证失败，不会返回明文。没有 `BOOP_MASTER_KEY` 时不得写入任何密钥行，读取密钥也直接失败；**删除密钥只是 `DELETE` 一行，不需要解密，因此没有主密钥时仍可幂等清除**（同一请求里既有新密钥又有删除时按写入处理，整单失败）。
- GitHub 登录在一个事务内完成：先查 `oauth_accounts(provider, provider_user_id)`，否则仅在 GitHub 标记邮箱已验证时按邮箱绑定已有用户或创建新用户（`password_hash` 为 NULL）；`auth.registration_enabled` 在读它的同一事务内决定是否允许创建新用户，未验证邮箱一律不写行。绑定插入遇到 `UNIQUE(provider, provider_user_id)` 冲突时在同一事务内重查实际 `user_id`：只有它等于本次解析出的用户时才幂等成功，否则报冲突并回滚，绝不把该身份当作另一个用户的登录凭据。
- 发布 `photo` 必须在同一事务中确认至少一个属于当前 owner 且为图片 MIME 的 `assets` 行。
- 本地上传按 `(owner_user_id, sha256)` 去重：相同字节只保留一行 asset 和一个文件，由 `idx_assets_owner_hash` 唯一索引保证（迁移 003），不依赖应用层的查询时序。
- 发布 `article` 必须有标题和正文；发布 `moment` 必须有正文。
- `cover_asset_id` 在迁移 002 中通过触发器或应用事务校验属于当前文章，不建立会造成建表顺序循环的外键。
- 点赞、收藏使用 `INSERT ... ON CONFLICT DO NOTHING` 与 `DELETE`，响应返回最终状态和计数。收藏是私有的：返回的计数是当前用户自己的收藏总数。
- 删除或拒绝父评论后，其回复仍是 `approved` 数据，但公开列表不会展示孤儿回复（写作时判定的可见性以父评论为准）。
- FTS 索引由 posts 的 insert/update/delete 触发器同步；软删除内容不得进入搜索结果。
- `ai_cache` 是生成结果的单行缓存：作者状态使用 `cache_key='author_status'`，`value_json` 形如 `{"text":"...","topics":["..."]}`（纯文本，最多 280 字与 5 个主题词）。`source_updated_at` 原样保存生成时最新已发布内容的 `updated_at`（与 `posts.updated_at` 做字符串比较，与公开信息流游标使用的是同一套存储格式），`expires_at` 由 `ai.author_status_ttl_hours` 计算；`last_error` 只保存稳定的失败短码（如 `timeout`、`upstream`、`invalid_reply`），**不保存提示词、密钥、模型输出或上游响应体**。刷新失败时不新增、不覆盖已有行，只更新 `last_error`；没有缓存行时前端渲染手写兑底文案，不为此写入一行。
- 迁移文件一经发布不可修改，只能追加新版本。

## 默认设置

```json
{
  "site.name": "Boop",
  "site.description": "遇事开心的个人博客",
  "site.avatar_url": "",
  "site.timezone": "Asia/Shanghai",
  "content.page_size": 20,
  "auth.registration_enabled": true,
  "comments.enabled": true,
  "comments.moderation_enabled": false,
  "ai.enabled": false,
  "ai.base_url": "",
  "ai.chat_model": "",
  "ai.embedding_model": "",
  "ai.author_status_ttl_hours": 168
}
```

