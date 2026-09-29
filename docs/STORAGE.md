# 上传存储

Boop 的上传有两种存放位置，在后台的「存储」设置页里二选一：

- **本地目录**（默认）：`$BOOP_DATA_DIR/uploads/YYYY/MM/`。
- **兼容 S3 的对象存储**：Cloudflare R2、MinIO、AWS S3 等，配置一个 Endpoint、一个桶和一对凭据即可。

两种位置对页面、API 和数据库完全透明：`assets` 表里的 `storage_key` 形状不变，HTML 与 JSON 里的图片地址都由同一个函数生成，切换位置不需要改数据库、不需要改模板、不需要改客户端。

保存后**立刻生效，不需要重启**：进程在启动时读一次存储配置，每次保存设置后再读一次，其余时间只读内存里的快照。重启不会带来任何额外效果——它只会读到数据库里已经存好的值。

## 为什么默认是本地目录

`docs/PRODUCT.md` §2 的成功标准是「单个 Go 二进制 + SQLite + 本地上传目录即可部署」。默认值必须让一台 1 核 512MB 的机器无需任何外部服务就能跑起来，对象存储是可选增强，不是必需品。

## 为什么值得换成 R2

- **出网免费**。R2 不收取 egress 费用，而个人博客的流量几乎全是图片的读。这是它和 S3 相比最实际的区别。
- **免费额度够用**：10GB 存储、每月 100 万次写入（Class A）、1000 万次读取（Class B）、无出网费用。图片是读多写少，正好落在额度内。
- **主机磁盘不再是上限**。1 核小机器的磁盘和带宽通常都很紧。

代价是：多了一个外部依赖，凭证需要保管，备份面从「一个目录」变成「一个目录 + 一个桶」。

## 在哪里配置

**后台「存储」页（`/admin/settings/storage`）是权威来源。**页面上的字段：

| 字段 | 对应设置键 | 说明 |
|---|---|---|
| 存放位置 | `storage.mode` | `local` 或 `object` |
| Endpoint | `storage.endpoint` | S3 签名 API 地址 |
| 桶名 | `storage.bucket` | |
| 读取地址 | `storage.public_url` | 读取对象的主机名 |
| 区域 | `storage.region` | 留空即 `auto` |
| 键前缀 | `storage.prefix` | 可留空 |
| Access Key ID | `storage.access_key_id`（密钥） | 加密保存，不回显 |
| Secret Access Key | `storage.secret_access_key`（密钥） | 加密保存，不回显 |

两个凭据和 GitHub/AI 的密钥走同一条路：用 `BOOP_MASTER_KEY` 加密后存在 `secret_settings` 表里，任何 API 都不会返回，日志里也不会出现。没有配置 `BOOP_MASTER_KEY` 时它们无法保存，页面会直接说明。

半配置（选了对象存储却缺字段）在**保存时**被拒绝，而不是等到第一次上传才失败；错误里会列出缺哪几项，此时进程仍然用原来的存放位置。通过环境变量配置时同样的检查发生在**启动时**，报出缺哪个变量。

### 环境变量只播种一次

`BOOP_R2_*` 仍然可用，但它现在只在**站点还没有保存过存储分类**时生效：第一次启动时把这几个变量的值写进设置表，之后就以网页为准，改环境变量不再有影响。这样做有两个原因：

1. 已经用 `BOOP_R2_*` 部署好的站点升级后行为不变。
2. 「网页里可改」必须真的可改——如果环境变量永远压过页面，那这个页面就是摆设。

播种需要 `BOOP_MASTER_KEY`，因为凭据要加密入库。**没有 `BOOP_MASTER_KEY` 时进程什么都不写**，继续按环境变量把上传写进桶里。这一点是有意为之：退回到本地目录会让站点的新图片悄悄写到磁盘上，而已发布的图片都在桶里，那是最难排查的一种故障。

```dotenv
BOOP_R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
BOOP_R2_BUCKET=boop-uploads
BOOP_R2_ACCESS_KEY_ID=<token 的 Access Key ID>
BOOP_R2_SECRET_ACCESS_KEY=<token 的 Secret Access Key>
BOOP_R2_PUBLIC_URL=https://uploads.example.com
BOOP_R2_REGION=auto          # 可省略，默认 auto
BOOP_R2_PREFIX=uploads       # 可省略
```

`BOOP_R2_PUBLIC_URL` 是**读取**地址，不是签名 API 地址。对象通过它被读取：

- R2 自定义域名（推荐，可以从根路径提供，缓存策略由 Cloudflare 控制）；
- 或桶的 `r2.dev` 开发地址（Cloudflare 明确说明它不适合生产流量，会限速）。

签名用的 `BOOP_R2_ENDPOINT` **不是**公开读取面，浏览器不会访问它。因此桶不需要开启公开访问也能工作：只要 `BOOP_R2_PUBLIC_URL` 指向的那个域名能读到对象即可。

### 凭证

在 Cloudflare 控制台创建 **R2 API Token**（不要用全局 API Key），权限只需要 **Object Read & Write**，并限定到这一个桶。R2 使用 `auto` 作为签名区域，用 path-style 寻址；这两点代码已经内置。

### Cloudflare 侧的设置

1. 建一个桶，例如 `boop-uploads`。
2. 在该桶的 Settings 里绑定一个自定义域名（或记下 `r2.dev` 地址），填进「读取地址」。
3. 创建一个限定到该桶的 R2 API Token，把 Key ID 与 Secret 填进「Access Key ID」与「Secret Access Key」。
4. 桶的 CORS 无需配置：页面直接引用图片，不做跨域请求。

## 运行时行为

### 写入

上传走 `PutObject`，并带上对象级别的 `Content-Type` 与 `Cache-Control: public, max-age=31536000, immutable`。对象名是 `assets.storage_key`（可加 `BOOP_R2_PREFIX`），形状固定为 `<YYYY>/<MM>/<32 位随机十六进制>.<ext>`，只写一次、永不改写。

数据库提交失败时，或者并发上传相同字节输了竞争时，会调用 `DeleteObject` 把刚写入的对象删掉，保证「没有行指向的对象」不会长期存在。这一步是尽力而为：对象名是随机的、没有任何东西指向它，所以即使删除失败也不会被读到。

### 读取

页面和 API 里的图片地址是 `<读取地址>/<prefix>/<storage_key>`，浏览器直接从该域名取图，**不经过 Go 进程**。在 1 核机器上，把图片字节代理一遍等于把内存预算花在 CDN 做得更好的事情上。

`GET /uploads/<storage_key>` 在对象存储模式下返回 301，`Location` 指向同一个对象地址。旧页面、相对链接和已经发出去的地址因此继续可用，代价只是一次可缓存的重定向。键形状不合法时仍然返回 404，重定向不会成为发布任意地址的通道。

## 从本地目录迁移

对象存储和本地目录是互斥的：进程按配置只认一个位置。已经存在的键不会因为切换配置而移动，所以迁移必须在切换之前把对象放进桶里。后台「存储」页常驻一条红色警告交代这件事，但它拦不住你——顺序仍然是你的责任。

1. **先备份**：`tar` 整个 `$BOOP_DATA_DIR`（数据库和上传都在里面）。
2. **先上传，后切换**：把 `$BOOP_DATA_DIR/uploads/` 下的内容按原路径上传到桶里，即 `2026/09/<hex>.png` 传到 `<prefix>/2026/09/<hex>.png`。用 `aws s3 sync` 或 `rclone sync` 都可以，键路径必须原样保留——数据库里存的就是这个键。
3. **核对数量**：桶里的对象数应等于 `SELECT COUNT(*) FROM assets;`（去重的图片只有一份文件，两张表都按同一个规则算）。
4. **再在「存储」页把存放位置改成对象存储并保存**。保存即生效，此后新上传直接进桶。
5. **保留旧目录**一段时间再删。回滚就是在同一页改回本地目录并保存，旧的键仍在本地目录里——桶里的值和两个凭据都还在，不必重输。

反向迁移（桶 → 本地目录）没有内置命令。需要时把对象按同样的键路径同步回 `$BOOP_DATA_DIR/uploads/`，再把存放位置改回本地目录即可。

## 备份

- 本地模式：备份 `$BOOP_DATA_DIR` 就够了（`boop.db` 与 `uploads/`）。
- 对象存储模式：备份面变成两处——`boop.db`，以及桶本身。数据库里只有引用，对象在桶里，所以**两者必须一起备份**。启用桶的版本控制或生命周期规则是低成本的兜底。
- 存储配置本身（含两个凭据的密文）就在 `boop.db` 里，跟着数据库一起备份即可；解密的 `BOOP_MASTER_KEY` 不在数据库里，必须单独保管——丢了它，库里那对凭据就再也读不出来，需要在 Cloudflare 重新签发一个 Token。

## 实现边界

- 只用到 `PutObject` 与 `DeleteObject` 两个操作。对象只写一次、名字不可猜、永不改写，因此不需要 multipart 上传、不需要分片、不需要预签名 URL。
- 签名是标准库实现的 AWS Signature Version 4（`internal/media/sigv4.go`），并且逐字对照 AWS 官方测试向量。**不引入 AWS SDK**：`docs/PRODUCT.md` §7 明确把这列为非目标，`aws-sdk-go-v2` 会把 6 条直接依赖变成十几条。
- 依赖的两个事实必须保持：`storage_key` 的形状（`media.ValidStorageKey` 是目录穿越的唯一防线），以及写入后永不改写。
