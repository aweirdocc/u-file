# u-file

> **U**nified **File** —— 可私有化部署的统一文件管理服务平台：把对象存储（S3 / 阿里云 OSS / 腾讯云 COS / 华为云 OBS / 七牛 Kodo / 百度 BOS / Cloudflare R2 / MinIO）、FTP、SFTP、数据库（BLOB / GridFS）、本地磁盘等异构文件源统一接入，提供 Web 文件浏览、搜索、下载与基础管理能力。

**当前状态：M2 已完成**（写能力：上传/删除/重命名/建目录 + zip 打包下载 + 索引搜索 + FTP/FTPS/SFTP 驱动；M1：凭据加密落库 + 登录鉴权/角色/审计 + 索引缓存与后台同步；M0：驱动抽象 + local/s3 驱动 + 流式下载网关）。规划文档见 [docs/](./docs/)。

## 文档导航

| 文档 | 内容 |
|---|---|
| [docs/01-调研报告.md](./docs/01-调研报告.md) | 同类开源产品调研与差异化定位（AList/OpenList、Cloudreve 等） |
| [docs/02-需求与产品规划.md](./docs/02-需求与产品规划.md) | 产品定位、目标用户、功能范围（P0~P3）、里程碑、验收标准 |
| [docs/03-技术可行性分析.md](./docs/03-技术可行性分析.md) | 技术栈选型、各类文件源逐项可行性、风险与缓解、结论 |
| [docs/04-总体架构设计.md](./docs/04-总体架构设计.md) | 分层架构、驱动接口设计、数据模型、API 草案、安全与部署 |
| [docs/research/01-技术调研笔记.md](./docs/research/01-技术调研笔记.md) | 调研原始笔记（一手来源、带引用链接） |
| [docs/m0-poc-验证记录.md](./docs/m0-poc-验证记录.md) | M0 PoC 冒烟与大文件内存验证记录 |

## 快速开始（M1）

环境要求：Go 1.22+（本机未装时，Windows 可解压官方 zip 到任意目录并把 `go/bin` 加入 PATH）。

```bash
# 1. 准备配置与样例文件
cp config.example.yaml config.yaml   # 按注释启用想接入的源
mkdir -p data/files/docs && echo hello > data/files/docs/readme.txt

# 2. 启动（内嵌 Web UI，无需单独前端构建）
go run ./cmd/u-file -config config.yaml
# → http://localhost:8080
# 首次访问会引导创建管理员账号；数据（源定义/主密钥/用户/索引/审计）在 ./data，
# 可用 -data 参数另指定。

# 3.（可选）本地起一个 S3 兼容测试端点，验证 S3 驱动
go run ./cmd/fakes3 -addr 127.0.0.1:19000 -bucket demo
# config.yaml 里启用 minio-demo/fakes3 段（endpoint 指向 19000）

# 4. 测试
go test ./...
# S3 驱动对真实端点冒烟（MinIO/R2/OSS...）：
# TEST_S3_ENDPOINT=http://127.0.0.1:9000 TEST_S3_BUCKET=demo \
# TEST_S3_ACCESS_KEY=minioadmin TEST_S3_SECRET_KEY=minioadmin TEST_S3_PATH_STYLE=true \
#   go test ./internal/driver/s3/ -run TestS3Smoke -v
```

> **安全提示（M1）**：已启用登录鉴权（JWT）与凭据加密落库，但仍建议：内网/本机部署 + 反向代理提供 HTTPS；**务必备份 `data/master.key`**——丢失后已保存的源凭据无法解密（源列表还在，重新填一遍凭据即可）。

## 已接入的源类型

| 类型 | 配置要点 | 能力 |
|---|---|---|
| `local` | `root: 本地目录` | 列表 / 下载 / Range / 上传·删除·重命名·建目录 |
| `s3` | `endpoint` `region` `bucket` `access_key` `secret_key` `use_path_style` `prefix`(可选) | 列表(游标分页) / 下载 / Range / 预签名直链 / 写能力(需凭据) |
| `ftp` | `host` `port`(默认21) `username` `password` `tls`(FTPS) `insecure_tls` `root`(可选) | 列表 / 下载(断点) / 写能力 |
| `sftp` | `host` `port`(默认22) `username` + `password` 或 `private_key`(PEM 内容或路径，`private_key_passphrase` 可选) `root`(可选) | 列表 / 下载(Seek) / 写能力 |

预置 endpoint 参考（写在 `config.example.yaml` 注释里）：MinIO `http://host:9000` + path-style；R2 `https://<ACCOUNT_ID>.r2.cloudflarestorage.com` + region `auto`；OSS `https://s3.oss-{region}.aliyuncs.com`；COS `https://cos.<region>.myqcloud.com`（桶名 `<Bucket>-<APPID>`）。

## 文件源管理入口

**Web 界面**：右上角「＋ 新增文件源」→ 选类型、填参数 → **测试连接** → 保存（即时生效，无需重启）；悬停源标签可 **✎ 编辑**（ID/类型不可变；凭据保持 `******` 表示不修改）与 **✕ 删除**（只删平台接入配置，不动存储里的文件）。

等价的 REST API（需登录，管理员可写 / 访客只读）：

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/api/v1/auth/bootstrap` | 无用户时 | 创建首个管理员（之后永久关闭） |
| POST | `/api/v1/auth/login` | 公开 | 登录，返回 JWT（`X-Auth-Token` + Cookie）；连续失败 5 次起指数退避限速 |
| GET | `/api/v1/auth/status` | 公开 | `{auth_enabled, needs_bootstrap}` |
| GET/POST/DELETE | `/api/v1/auth/users[...]` | admin | 访客账号管理（创建/列表/删除/重置密码） |
| GET | `/api/v1/sources` | 登录 | 源列表（凭据打码为 `******`） |
| POST | `/api/v1/sources` | admin | 新增：`{"id","name","type","config"}`，校验通过即时可用 |
| PUT | `/api/v1/sources/{id}` | admin | 编辑：`{"name","config"}`；config 中凭据为 `******` 时保留旧凭据 |
| DELETE | `/api/v1/sources/{id}` | admin | 删除 |
| GET | `/api/v1/sources/export?redact=1` | admin | 导出全部源配置为 JSON 附件（`redact=1` 凭据打码，默认含明文凭据） |
| POST | `/api/v1/sources/import` | admin | 导入源配置：`{"content": "<导出 JSON 或 JSON 字符串>", "overwrite": bool}`，逐条报告 added/updated/skipped/failed |
| POST | `/api/v1/sources/test` | admin | 连接测试（不落盘）：`{"type","config"}` → `{"ok","latency_ms","error"}` |
| GET | `/api/v1/sources/{id}/files?path=&cursor=&limit=&refresh=1` | 登录 | 目录列表（命中索引缓存；`refresh=1` 绕过缓存直读源） |
| GET | `/api/v1/sources/{id}/sync` | 登录 | 该源索引同步状态（上次成功/失败原因/进行中） |
| POST | `/api/v1/sources/{id}/sync` | admin | 立即触发该源同步 |
| GET | `/api/v1/search?q=关键词&source=源ID(可选)&limit=50` | 登录 | 跨源文件名搜索（基于索引/快照，大小写不敏感包含匹配） |
| POST | `/api/v1/sources/{id}/upload` | admin | multipart 上传（`file` 可多个 + `path` 目标目录），逐文件报告结果 |
| POST | `/api/v1/sources/{id}/mkdir` | admin | 新建目录：`{"path": "a/b/c"}`（逐级创建） |
| POST | `/api/v1/sources/{id}/delete` | admin | 批量删除：`{"paths": [...]}`（目录递归删） |
| POST | `/api/v1/sources/{id}/rename` | admin | 重命名/移动：`{"from", "to"}`（目录含内容整体移动） |
| GET | `/api/v1/sources/{id}/archive?paths=a&paths=docs/&name=包名` | 登录 | 多选/整目录 zip 流式打包下载（不落盘；上限 1 万文件/2GB） |

写操作仅对声明了 `write` 能力位的源开放（界面上自动显隐）。S3 匿名源（无凭据）为只读。

**持久化规则**：源定义存于 `data/sources.json`，凭据字段以 AES-256-GCM 加密（密文前缀 `enc:v1:`），主密钥为 `data/master.key`（首次启动自动生成，0600）；旧版明文 `sources.json` 首次启动自动加密迁移。`config.yaml` 仅在首次启动（该文件不存在时）播种导入，之后以页面/API 管理为准，已删除的源不会被配置文件复活。API 响应永不回显明文凭据（打码为 `******`）。

**配置迁移**：页面右上角「导出配置 / 导入配置」可在实例间迁移源定义。导出可选脱敏（凭据替换为 `******`，适合分享/存档）；未脱敏导出含明文凭据，请勿入库或外发。导入兼容完整导出文件与裸数组两种格式，逐条校验并给出 added/updated/skipped/failed 报告；含脱敏凭据的条目导入后需补填真实凭据才能连接。

## 鉴权与审计（M1）

- **角色**：`admin`（全权）与 `guest`（只读浏览/下载/预览）；首个账号通过 Web 引导创建（即管理员，之后引导入口永久关闭）
- **会话**：JWT（HS256，24h），前端走 `Authorization: Bearer`；浏览器预览/下载标签走 HttpOnly Cookie
- **限速**：登录按「用户名+来源 IP」限速，连续失败 5 次起指数退避（5s 起，封顶 15 分钟）
- **审计**：登录/登出/用户变更/源增删改/导入导出/手动同步 记录至 `data/audit.log`（JSONL，人可读）

## 索引缓存、同步与搜索（M1/M2）

- 目录列表默认走索引缓存（TTL 10 分钟），命中时响应 <10ms；点面包屑旁的 **⟳** 或带 `?refresh=1` 绕过缓存直读源
- 后台每小时自动全量同步各源目录树（逐目录刷新，单源单轮 10 分钟/1 万目录上限），同步状态在文件列表上方展示（进行中/上次成功/失败原因）
- 同步完成后快照落盘 `data/index.json`，重启后浏览秒开
- 下载/预览/Stat 永远直读源，不受缓存影响（保证字节级正确）
- **搜索**：顶栏搜索框回车即跨源搜文件名（命中索引/快照内容；只搜已同步过的目录，这是零配置方案的语义边界）
- 写操作（上传/删除/重命名/建目录）完成后自动失效该源缓存，浏览即时可见

## 文件预览

文件列表中可预览的类型会显示「预览」链接，弹窗内直接查看（视频/音频/PDF/图片由浏览器原生渲染，文本/代码高亮换行显示，Markdown 内置渲染器转 HTML）：

| 类别 | 格式 |
|---|---|
| 图片 | jpg / jpeg / png / gif / webp / bmp / ico / avif / svg |
| 视频 | mp4 / webm / mov / m4v（mkv 取决于浏览器编解码器） |
| 音频 | mp3 / wav / ogg / flac / m4a / aac |
| 文档 | pdf |
| 文本/代码 | txt / csv / log / json / yaml / toml / ini / xml / html / css / go / js / ts / py / java / c / cpp / rs / sh / sql 等 |
| Markdown | md（标题/列表/粗斜体/代码/引用/链接/图片） |

- 预览走 `GET /api/v1/sources/{id}/raw?path=...`，`Content-Disposition: inline` + 正确 `Content-Type` + CSP/nosniff 加固，支持 Range（视频拖动进度条可用）
- 安全策略：白名单外的类型返回 415；HTML 文件强制按 `text/plain` 直出（脚本不执行）；文本预览限 2MB
- 暂不支持：Office 文档（docx/xlsx/pptx，需前端重依赖，排在 M1+）、压缩包内部浏览

## 核心设计一览

- **统一驱动抽象**：`FSDriver` 接口 + 能力位（Capabilities），协议差异全部隔离在驱动层；写能力为可选 `Writer` 接口（local/s3/ftp/sftp 已实现）
- **面向流**：下载全程 `io.Copy` 流式转发（1GB 文件实测服务端峰值内存 ~16MB），`http.ServeContent` 提供 Range/断点续传；zip 打包同为流式（不落盘不整载）
- **直链模式**：`Presigner` 可选能力 + `?direct=1` 302 跳转，省网关带宽
- **凭据加密**：源配置敏感字段 AES-256-GCM 加密落库（内存中明文仅供驱动使用，API 永不回显明文）；主密钥与 JWT 密钥分离存放

## 技术栈

Go 1.22+ / Gin · aws-sdk-go-v2（S3 协议族统一接入） · jlaffaye/ftp（FTP/FTPS） · pkg/sftp + x/crypto/ssh（SFTP） · golang.org/x/crypto（bcrypt） · 内嵌单页 Web UI（原生 JS，无构建步骤） · 文件型存储（sources.json / users.json / index.json / audit.log，SQLite 视需求后置）
