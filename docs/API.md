# Boop HTTP API v0.1

所有 JSON 接口位于 `/api/v1`。成功统一返回 `{"data":...}`；失败返回 `{"error":{"code":"...","message":"..."}}`。写接口要求 Session Cookie 和 `X-CSRF-Token`。列表使用游标 `cursor=<published_at,id>`，默认 20，最大 50。

## 公共与健康检查

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 进程存活，不访问外部 AI |
| GET | `/readyz` | SQLite 可读写迁移状态正常 |
| GET | `/api/v1/posts?type=&cursor=&limit=` | 已发布信息流；`type` 为 article/photo，空值含动态 |
| GET | `/api/v1/posts/{slug}` | 内容详情、点赞数、评论数和当前用户状态 |
| GET | `/api/v1/posts/{slug}/comments` | 仅已批准评论，按时间正序嵌套一级回复 |
| GET | `/api/v1/search?q=&cursor=` | SQLite FTS 搜索 |
| GET | `/api/v1/ai/author-status` | 返回缓存状态、generated_at、stale 标志 |
| GET | `/feed.xml` | RSS 2.0 |

## 认证

| 方法 | 路径 | 输入 | 结果 |
|---|---|---|---|
| POST | `/api/v1/auth/register` | email,password,display_name | 创建 reader 并登录；注册关闭返回 403 |
| POST | `/api/v1/auth/login` | email,password | 创建 Session，返回用户和 CSRF token |
| POST | `/api/v1/auth/logout` | 无 | 删除当前 Session 并清 Cookie |
| GET | `/api/v1/auth/me` | 无 | 当前用户和 CSRF token；游客返回 401 |
| GET | `/auth/github/start` | 可选 return_to | 保存一次性 state 后跳转 GitHub |
| GET | `/auth/github/callback` | code,state | 登录/绑定后回到同源白名单路径 |

密码 10–72 字节；邮箱最大 254 字符；登录与注册按 IP 和邮箱限速。所有认证失败使用相同外部错误信息。

## 互动

| 方法 | 路径 | 输入 | 权限 |
|---|---|---|---|
| POST | `/api/v1/posts/{id}/comments` | body,parent_id? | reader/owner |
| DELETE | `/api/v1/comments/{id}` | 无 | 作者或 owner |
| PUT | `/api/v1/posts/{id}/like` | 无 | reader/owner；幂等设为已点赞 |
| DELETE | `/api/v1/posts/{id}/like` | 无 | reader/owner；幂等取消 |
| PUT | `/api/v1/posts/{id}/bookmark` | 无 | reader/owner |
| DELETE | `/api/v1/posts/{id}/bookmark` | 无 | reader/owner |
| GET | `/api/v1/me/bookmarks?cursor=` | 无 | reader/owner |

## 站长内容管理

| 方法 | 路径 | 输入/说明 |
|---|---|---|
| POST | `/api/v1/admin/posts` | type,status,title,body,excerpt,asset_ids,tags,location,captured_at |
| PATCH | `/api/v1/admin/posts/{id}` | 上述字段的部分更新，带 `updated_at` 乐观锁 |
| DELETE | `/api/v1/admin/posts/{id}` | 软删除 |
| POST | `/api/v1/admin/uploads` | multipart 单文件；jpg/png/webp/gif，默认最大 10MB |
| GET | `/api/v1/admin/comments?status=pending` | 审核队列 |
| POST | `/api/v1/admin/comments/{id}/approve` | 批准 |
| POST | `/api/v1/admin/comments/{id}/reject` | 拒绝 |
| DELETE | `/api/v1/admin/comments/{id}` | 管理删除 |

## 设置与 AI

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/admin/settings` | 返回非密钥设置及密钥是否已配置 |
| PATCH | `/api/v1/admin/settings` | 白名单字段更新；空密钥表示保持不变，显式 `clear_secret=true` 才删除 |
| POST | `/api/v1/admin/ai/test` | 最小请求验证配置，不回显密钥 |
| POST | `/api/v1/admin/ai/author-status/regenerate` | 手动刷新作者状态，单飞 |
| POST | `/api/v1/admin/ai/assist` | action=summary/tags/seo，输入草稿内容 |
| POST | `/api/v1/admin/ai/chat` | v0.2 站内问答，SSE 输出 |

## HTML 页面

- `GET /` 首页 SSR。
- `GET /p/{slug}` 内容详情 SSR。
- `GET /search?q=` 搜索结果 SSR。
- `GET /login`、`GET /register`。
- `GET /bookmarks` 登录用户收藏。
- `GET /admin`、`GET /admin/comments`、`GET /admin/settings` 站长页面。
- 未知页面返回带 request_id 的统一 404；API 永远不返回 HTML 错误页。

