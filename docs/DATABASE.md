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
CREATE INDEX idx_likes_post ON likes(post_id);

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
- 发布状态带一条时间不变量：`status='published'` 的行一定有非空 `published_at`（`content.Create` 在首次以发布状态写入时落库，`content.Update` 在首次转为发布时补写，之后冻结，编辑不会移动内容在信息流中的位置）。因此读取已发布行的查询可以直接读该列，不需要 `COALESCE` 兜底；该列是应用层不变量，schema 仍允许 NULL（草稿与归档行就是 NULL）。
- `cover_asset_id` 在迁移 002 中通过触发器或应用事务校验属于当前文章，不建立会造成建表顺序循环的外键。
- 点赞、收藏使用 `INSERT ... ON CONFLICT DO NOTHING` 与 `DELETE`，响应返回最终状态和计数。收藏是私有的：返回的计数是当前用户自己的收藏总数。
- 每个内容读取都要解析点赞数，走的是 `SELECT COUNT(*) FROM likes WHERE post_id = ?`（`internal/content/postColumns`）。`likes` 的主键是 `(user_id, post_id)`，`post_id` 没有前导索引，因此该计数原本是全表扫描；连接池又被钉在 `maxOpenConns = 1`，所以一页 N 条内容就是在唯一连接上串行扫 N 次，代价随点赞量线性增长。`idx_likes_post`（迁移 004）把它变成覆盖索引查找，`internal/store/store_test.go` 用 `EXPLAIN QUERY PLAN` 钉住计划（并用 `NOT INDEXED` 证明该断言非空转）。反方向（按 `user_id` 查自己的点赞）已由主键覆盖，不再单独建索引。
- 删除或拒绝父评论后，其回复仍是 `approved` 数据，但公开列表不会展示孤儿回复（写作时判定的可见性以父评论为准）。
- FTS 索引由 posts 的 insert/update/delete 触发器同步；软删除内容不得进入搜索结果。
- 搜索只**读** `post_search` 与 `posts`，不新增表、不新增迁移：`status='published' AND deleted_at IS NULL` 的过滤在 SQL 里完成（索引里保留草稿与软删除行，以便恢复后重新可见），`snippet()` 的列参数为 `-1`（取匹配最佳的那一列：标题、正文或摘要，因此只在标题/摘要命中的结果也能看到高亮），`bm25()` 只作为结果里的内部字段，排序恒为 `published_at DESC, id DESC`，游标沿用公共信息流的 `<published_at,id>`，命中的行与索引列一一对应，不会因为评分造成漏行或重复。
- RSS 用一条窄查询走 `idx_posts_feed`，只读 `slug`、`type`、`title`、`body_markdown`、`excerpt` 与 `published_at`（每条一个标记，`LIMIT 50`），**不读** `body_html`、`post_assets`/`assets`、`tags`/`post_tags` 与计数子查询；文章正文在应用层经 goldmark + bluemonday 渲染后只取可见文本（复用 `internal/content` 的同一套流水线，未新增模块依赖），因此订阅读者拿到的永远是纯文本描述。该查询直接读、直接按 `published_at DESC, id DESC` 排序（依据上面的发布时间不变量，不套 `COALESCE`）：一旦把列包进表达式，`idx_posts_feed` 的 `(status, published_at DESC, id DESC)` 顺序就用不上，SQLite 会对全部已发布行做临时 B-tree 排序；`internal/server/handlers_feed_test.go` 里有 `EXPLAIN QUERY PLAN` 测试断言计划读 `idx_posts_feed` 且不出现临时排序。
- `ai_cache` 是生成结果的单行缓存：作者状态使用 `cache_key='author_status'`，`value_json` 形如 `{"text":"...","topics":["..."]}`（纯文本，最多 280 字与 5 个主题词）。`source_updated_at` 原样保存生成时最新已发布内容的 `updated_at`，读取时与当前 `MAX(updated_at)` 按**时间**（RFC3339，同一套存储格式）比较：只有**严格更新**的已发布内容才值得刷新，删除或归档导致这个最大值回退时不调用模型；时间戳无法解析时不能证明内容没变，按“可能更新”保守刷新。`expires_at` 由 `ai.author_status_ttl_hours` 计算；`last_error` 只保存稳定的失败短码（如 `timeout`、`upstream`、`invalid_reply`），**不保存提示词、密钥、模型输出或上游响应体**。
- 刷新失败分两种情况落库：已有可渲染值时只更新 `last_error`，**不覆盖 `value_json`、`source_updated_at`、`generated_at` 与 `expires_at`**；尚无任何可渲染值时才写入一行最小失败占位行（`value_json=''`、`generated_at=''`、`expires_at` 为失败时刻加固定的 5 分钟退避）。占位行只承载失败短码与“下次可重试时刻”：读取端据此渲染手写兑底文案（`default=true`、`generated_at` 为空），并在退避期内直接复用而不调用模型；仅仅值损坏但仍带 `generated_at` 的行不算占位行，有已发布内容时立即重新生成、不继承退避。
- 迁移文件一经发布不可修改，只能追加新版本。

## 默认设置

```json
{
  "site.name": "Boop",
  "site.description": "遇事开心的个人博客",
  "site.avatar_url": "",
  "site.icon_url": "",
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

