# u-file

> **U**nified **File** —— 可私有化部署的统一文件管理服务平台：把对象存储（S3 / 阿里云 OSS / 腾讯云 COS / 华为云 OBS / 七牛 Kodo / 百度 BOS / Cloudflare R2 / MinIO）、FTP、SFTP、数据库（BLOB / GridFS）、本地磁盘等异构文件源统一接入，提供 Web 文件浏览、搜索、下载与基础管理能力。

**当前状态：M0 PoC 已完成**（驱动抽象 + local/s3 驱动 + 流式下载网关，已通过端到端冒烟与大文件内存验证）。规划文档见 [docs/](./docs/)。

## 文档导航

| 文档 | 内容 |
|---|---|
| [docs/01-调研报告.md](./docs/01-调研报告.md) | 同类开源产品调研与差异化定位（AList/OpenList、Cloudreve 等） |
| [docs/02-需求与产品规划.md](./docs/02-需求与产品规划.md) | 产品定位、目标用户、功能范围（P0~P3）、里程碑、验收标准 |
| [docs/03-技术可行性分析.md](./docs/03-技术可行性分析.md) | 技术栈选型、各类文件源逐项可行性、风险与缓解、结论 |
| [docs/04-总体架构设计.md](./docs/04-总体架构设计.md) | 分层架构、驱动接口设计、数据模型、API 草案、安全与部署 |
| [docs/research/01-技术调研笔记.md](./docs/research/01-技术调研笔记.md) | 调研原始笔记（一手来源、带引用链接） |
| [docs/m0-poc-验证记录.md](./docs/m0-poc-验证记录.md) | M0 PoC 冒烟与大文件内存验证记录 |

## 快速开始（M0 PoC）

环境要求：Go 1.22+（本机未装时，Windows 可解压官方 zip 到任意目录并把 `go/bin` 加入 PATH）。

```bash
# 1. 准备配置与样例文件
cp config.example.yaml config.yaml   # 按注释启用想接入的源
mkdir -p data/files/docs && echo hello > data/files/docs/readme.txt

# 2. 启动（内嵌 Web UI，无需单独前端构建）
go run ./cmd/u-file -config config.yaml
# → http://localhost:8080

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

> 注意：M0 无鉴权，仅限本地联调，勿暴露公网。MVP（M1）将加入登录、源管理 API、凭据加密与索引缓存。

## 已接入的源类型（M0）

| 类型 | 配置要点 | 能力 |
|---|---|---|
| `local` | `root: 本地目录` | 列表 / 下载 / Range |
| `s3` | `endpoint` `region` `bucket` `access_key` `secret_key` `use_path_style` `prefix`(可选) | 列表(游标分页) / 下载 / Range / 预签名直链(需凭据) |

预置 endpoint 参考（写在 `config.example.yaml` 注释里）：MinIO `http://host:9000` + path-style；R2 `https://<ACCOUNT_ID>.r2.cloudflarestorage.com` + region `auto`；OSS `https://s3.oss-{region}.aliyuncs.com`；COS `https://cos.<region>.myqcloud.com`（桶名 `<Bucket>-<APPID>`）。

## 文件源管理入口

**Web 界面**：右上角「＋ 新增文件源」→ 选类型、填参数 → **测试连接** → 保存（即时生效，无需重启）；鼠标悬停顶部的源标签可点 ✕ 删除（只删平台接入配置，不动存储里的文件）。

等价的 REST API（M0 无鉴权）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/sources` | 源列表（凭据打码为 `******`） |
| POST | `/api/v1/sources` | 新增：`{"id","name","type","config"}`，校验通过即时可用 |
| DELETE | `/api/v1/sources/{id}` | 删除 |
| GET | `/api/v1/sources/export?redact=1` | 导出全部源配置为 JSON 附件（`redact=1` 凭据打码，默认含明文凭据） |
| POST | `/api/v1/sources/import` | 导入源配置：`{"content": "<导出 JSON 或 JSON 字符串>", "overwrite": bool}`，逐条报告 added/updated/skipped/failed |
| POST | `/api/v1/sources/test` | 连接测试（不落盘）：`{"type","config"}` → `{"ok","latency_ms","error"}` |

**持久化规则**：源定义存于 `data/sources.json`；`config.yaml` 仅在首次启动（该文件不存在时）播种导入，之后以页面/API 管理为准，已删除的源不会被配置文件复活。凭据目前明文存于该文件（0600 权限），AES-GCM 加密落库在 M1 完成。

**配置迁移**：页面右上角「导出配置 / 导入配置」可在实例间迁移源定义。导出可选脱敏（凭据替换为 `******`，适合分享/存档）；未脱敏导出含明文凭据，请勿入库或外发。导入兼容完整导出文件与裸数组两种格式，逐条校验并给出 added/updated/skipped/failed 报告；含脱敏凭据的条目导入后需补填真实凭据才能连接。

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

- **统一驱动抽象**：`FSDriver` 接口 + 能力位（Capabilities），协议差异全部隔离在驱动层；写能力在 M1 以可选接口扩展
- **面向流**：下载全程 `io.Copy` 流式转发（1GB 文件实测服务端峰值内存 ~16MB），`http.ServeContent` 提供 Range/断点续传
- **直链模式**：`Presigner` 可选能力 + `?direct=1` 302 跳转，省网关带宽
- **凭据加密**（规划）：源配置 AES-256-GCM 加密落库，API 永不回显明文（M1 交付）

## 技术栈

Go 1.22+ / Gin · aws-sdk-go-v2（S3 协议族统一接入） · Vue 3 + TypeScript + Element Plus（M1 起） · SQLite（M1 起）
