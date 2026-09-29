# 上传存储

Boop 的上传有两种存放位置，由环境变量二选一：

- **本地目录**（默认）：`$BOOP_DATA_DIR/uploads/YYYY/MM/`。
- **兼容 S3 的对象存储**：Cloudflare R2、MinIO、AWS S3 等，只需设置 `BOOP_R2_*`。

两种位置对页面、API 和数据库完全透明：`assets` 表里的 `storage_key` 形状不变，HTML 与 JSON 里的图片地址都由同一个函数生成，切换位置不需要改数据库、不需要改模板、不需要改客户端。

## 为什么默认是本地目录

`docs/PRODUCT.md` §2 的成功标准是「单个 Go 二进制 + SQLite + 本地上传目录即可部署」。默认值必须让一台 1 核 512MB 的机器无需任何外部服务就能跑起来，对象存储是可选增强，不是必需品。

## 为什么值得换成 R2

- **出网免费**。R2 不收取 egress 费用，而个人博客的流量几乎全是图片的读。这是它和 S3 相比最实际的区别。
- **免费额度够用**：10GB 存储、每月 100 万次写入（Class A）、1000 万次读取（Class B）、无出网费用。图片是读多写少，正好落在额度内。
- **主机磁盘不再是上限**。1 核小机器的磁盘和带宽通常都很紧。

代价是：多了一个外部依赖，凭证需要保管，备份面从「一个目录」变成「一个目录 + 一个桶」。

## 配置

```dotenv
BOOP_R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
BOOP_R2_BUCKET=boop-uploads
BOOP_R2_ACCESS_KEY_ID=<token 的 Access Key ID>
BOOP_R2_SECRET_ACCESS_KEY=<token 的 Secret Access Key>
BOOP_R2_PUBLIC_URL=https://uploads.example.com
BOOP_R2_REGION=auto          # 可省略，默认 auto
BOOP_R2_PREFIX=uploads       # 可省略
```

前五个只要设置了任意一个，其余的就全部必填。半配置的对象存储会让进程在启动时为每个缺失的变量报错，而不是在当天第一次上传时才失败。

`BOOP_R2_PUBLIC_URL` 是**读取**地址，不是签名 API 地址。对象通过它被读取：

- R2 自定义域名（推荐，可以从根路径提供，缓存策略由 Cloudflare 控制）；
- 或桶的 `r2.dev` 开发地址（Cloudflare 明确说明它不适合生产流量，会限速）。

签名用的 `BOOP_R2_ENDPOINT` **不是**公开读取面，浏览器不会访问它。因此桶不需要开启公开访问也能工作：只要 `BOOP_R2_PUBLIC_URL` 指向的那个域名能读到对象即可。

### 凭证

在 Cloudflare 控制台创建 **R2 API Token**（不要用全局 API Key），权限只需要 **Object Read & Write**，并限定到这一个桶。R2 使用 `auto` 作为签名区域，用 path-style 寻址；这两点代码已经内置。

### Cloudflare 侧的设置

1. 建一个桶，例如 `boop-uploads`。
2. 在该桶的 Settings 里绑定一个自定义域名（或记下 `r2.dev` 地址），填入 `BOOP_R2_PUBLIC_URL`。
3. 创建一个限定到该桶的 R2 API Token，把 Key ID 与 Secret 填进 `BOOP_R2_ACCESS_KEY_ID` / `BOOP_R2_SECRET_ACCESS_KEY`。
4. 桶的 CORS 无需配置：页面直接引用图片，不做跨域请求。

## 运行时行为

### 写入

上传走 `PutObject`，并带上对象级别的 `Content-Type` 与 `Cache-Control: public, max-age=31536000, immutable`。对象名是 `assets.storage_key`（可加 `BOOP_R2_PREFIX`），形状固定为 `<YYYY>/<MM>/<32 位随机十六进制>.<ext>`，只写一次、永不改写。

数据库提交失败时，或者并发上传相同字节输了竞争时，会调用 `DeleteObject` 把刚写入的对象删掉，保证「没有行指向的对象」不会长期存在。这一步是尽力而为：对象名是随机的、没有任何东西指向它，所以即使删除失败也不会被读到。

### 读取

页面和 API 里的图片地址是 `BOOP_R2_PUBLIC_URL/<prefix>/<storage_key>`，浏览器直接从该域名取图，**不经过 Go 进程**。在 1 核机器上，把图片字节代理一遍等于把内存预算花在 CDN 做得更好的事情上。

`GET /uploads/<storage_key>` 在对象存储模式下返回 301，`Location` 指向同一个对象地址。旧页面、相对链接和已经发出去的地址因此继续可用，代价只是一次可缓存的重定向。键形状不合法时仍然返回 404，重定向不会成为发布任意地址的通道。

## 从本地目录迁移

对象存储和本地目录是互斥的：进程按配置只认一个位置。已经存在的键不会因为切换配置而移动，所以迁移必须在切换之前把对象放进桶里。

1. **先备份**：`tar` 整个 `$BOOP_DATA_DIR`（数据库和上传都在里面）。
2. **先上传，后切换**：把 `$BOOP_DATA_DIR/uploads/` 下的内容按原路径上传到桶里，即 `2026/09/<hex>.png` 传到 `<prefix>/2026/09/<hex>.png`。用 `aws s3 sync` 或 `rclone sync` 都可以，键路径必须原样保留——数据库里存的就是这个键。
3. **核对数量**：桶里的对象数应等于 `SELECT COUNT(*) FROM assets;`（去重的图片只有一份文件，两张表都按同一个规则算）。
4. **再改环境变量并重启**。此后新上传直接进桶。
5. **保留旧目录**一段时间再删。回滚就是把 `BOOP_R2_*` 去掉再重启，旧的键仍在本地目录里。

反向迁移（桶 → 本地目录）没有内置命令。需要时把对象按同样的键路径同步回 `$BOOP_DATA_DIR/uploads/`，再清掉 `BOOP_R2_*` 重启即可。

## 备份

- 本地模式：备份 `$BOOP_DATA_DIR` 就够了（`boop.db` 与 `uploads/`）。
- 对象存储模式：备份面变成两处——`boop.db`，以及桶本身。数据库里只有引用，对象在桶里，所以**两者必须一起备份**。启用桶的版本控制或生命周期规则是低成本的兜底。

## 实现边界

- 只用到 `PutObject` 与 `DeleteObject` 两个操作。对象只写一次、名字不可猜、永不改写，因此不需要 multipart 上传、不需要分片、不需要预签名 URL。
- 签名是标准库实现的 AWS Signature Version 4（`internal/media/sigv4.go`），并且逐字对照 AWS 官方测试向量。**不引入 AWS SDK**：`docs/PRODUCT.md` §7 明确把这列为非目标，`aws-sdk-go-v2` 会把 6 条直接依赖变成十几条。
- 依赖的两个事实必须保持：`storage_key` 的形状（`media.ValidStorageKey` 是目录穿越的唯一防线），以及写入后永不改写。
