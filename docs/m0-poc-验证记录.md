# M0 PoC 验证记录

> 日期：2026-10-27 · 环境：Windows 10 x64 / Go 1.27.1 · 目标 S3 端点：gofakes3 v1.2.0（本地）

## 范围

M0 验证两件事（对应 docs/03-技术可行性分析 第 6 节的建议）：

1. 一套 S3 驱动跑通多家 S3 兼容端点的**列取与流式下载**；
2. **1GB 文件下载的内存曲线达标**（目标 < 200MB）。

## 环境说明

- 本机 Docker Hub / quay.io / 各国内镜像源拉取 MinIO 镜像均失败（`denied`/`not found`），改用纯 Go 的 gofakes3（v1.2.0，2026-07 发布，维护活跃）作为本地 S3 端点，封装于 `cmd/fakes3`。
- 局限：gofakes3 **不校验签名**，因此 SigV4 签名正确性需待真实云（OSS/R2/MinIO）联调覆盖；本机 9000 端口被既有 Java 进程占用，fakes3 使用 19000。
- gofakes3 禁用了 versioning 路由（`WithoutVersioning`），避免 SDK 探测桶版本状态时收到 NotImplemented。

## 验证结果

### 1. 单元测试（`go test ./...` 全绿）

| 包 | 用例 | 结果 |
|---|---|---|
| internal/driver/local | List / Stat / Open 全文与 Range / 非法路径（`..`、`\`）/ Seeker 断言 | PASS |
| internal/api | reopenSeeker 实现 io.ReadSeeker；Seek(0, SeekEnd) 返回 Stat size | PASS |
| internal/driver/s3 | TestS3Smoke（环境变量门控，可对任意真实 S3 端点运行） | PASS（对 fakes3） |

### 2. S3 驱动冒烟（对 fakes3，path-style）

- List 根目录（ListObjectsV2 + Delimiter）：正确返回 3 个目录前缀，对象不泄漏到顶层 ✅
- 逐级浏览 `docs/`：CommonPrefixes + Contents 混合正确，ETag/大小/时间正确 ✅
- Stat：HeadObject 命中对象；目录走前缀探测 ✅
- Open 全文读取：16 字节头部与写入内容逐字节一致 ✅
- PresignGet：生成 SigV4 预签名 URL，匿名 GET 200 且内容一致 ✅

### 3. 网关端到端（u-file → fakes3）

| 用例 | 期望 | 结果 |
|---|---|---|
| `GET /api/v1/sources/fakes3-demo/files` | 目录树 JSON | ✅ 3 目录 |
| `GET .../download?path=docs/readme.txt` | 全文 | ✅ `hello from fakes3` |
| `Range: bytes=6-9` | 206 + `from` | ✅ 206 |
| `?direct=1` | 302 → 预签名 URL | ✅ 302 |
| 本地源 `Range: bytes=0-4` | 206 + `hello` | ✅ 206（互不影响） |
| `?path=../escape` | 400 INVALID_PATH | ✅ |
| 不存在路径 | 404 NOT_FOUND | ✅ |
| local 源请求 presign | 400 NOT_SUPPORTED | ✅ |
| `GET /` | 内嵌 Web UI | ✅ |

### 4. 1GB 大文件内存曲线（网关流式代理，curl → u-file → fakes3）

下载 `big/1gb.bin`（1,073,741,824 字节，本地环回，实测吞吐 ~1.4 GB/s）：

| 场景 | 服务端内存（观测期间最大值） | 结论 |
|---|---|---|
| 单连接下载全程 | WS ≤ 13.7MB，Private ≤ 48.2MB | ✅ |
| **3 路并发**下载全程 | WS ≤ 16.4MB，Private ≤ 51.5MB | ✅ 远低于 200MB 目标 |
| 下载完成后 | Private 51.7MB（无增长滞留） | ✅ 无泄漏迹象 |

补充：从 512MB 偏移处 Range 下载后半段（536,870,912 字节）→ 206，说明大文件断点/分段可用。

内存恒定符合预期：下载链路为「S3 响应体 → io.Copy → HTTP 响应」，缓冲区为固定大小，与文件体积无关。

### 5. 过程中发现并修复的缺陷

| 缺陷 | 影响 | 修复 |
|---|---|---|
| local.Open 在 Length>0 时用 io.NopCloser 包装 LimitReader，Close 变空操作 | 文件句柄泄漏（Windows 上句柄不释放） | 引入 limitedCloser 组合 Closer（`internal/driver/local/local.go`） |
| reopenSeeker 拒绝 Seek(0, SeekEnd)，而 http.ServeContent 的 sizeFunc 依赖该调用求文件大小 | 所有不可 Seek 的流（S3 网络流）网关下载 500 “seeker can't seek” | reopenSeeker 支持基于 Stat size 的 SeekEnd（`internal/api/stream.go`） |
| 无凭据的 S3 源声明了 PresignURL 能力，Presign 时报 `unsupported signer type "noAuth"` | 匿名源直链不可用误报 | 能力位按“是否配置凭据”声明 + PresignGet 运行时兜底（`internal/driver/s3/s3.go`） |
| 驱动包无空导入，init 注册不生效，启动报 unknown source type | 服务无法启动 | 新增 `internal/driver/all` 统一导入，main.go 引用 |

### 6. 源管理入口（2026-10-27 追加）

Web「＋ 新增文件源」弹窗（类型选择 + 动态字段 + 测试连接 + 保存）与源标签 ✕ 删除按钮；对应 REST：POST/DELETE `/api/v1/sources`、POST `/api/v1/sources/test`。源定义持久化于 `data/sources.json`（原子写 + 增删即存），`config.yaml` 仅首次播种。

端到端验证结果：

| 用例 | 结果 |
|---|---|
| 新增 local 源 → 201，立即浏览 ✅ | 重复 id → 409 ✅；非法 id（空格/空/非法字符）→ 400 ✅ |
| 测试连接：正常目录 ok+耗时 ✅；不存在目录返回可读错误 ✅ | 列表响应凭据打码 `******`，内部使用不脱敏 ✅ |
| 重启后新增源仍在（持久化）✅ | 删除后 404；重启不复活 ✅ |
| 单源损坏（未知 type）不阻断启动，warn 跳过 ✅ | 并发增删读 + 单测（含 -race 常规跑）✅ |

### 7. 文件预览（2026-10-27 追加）

后端新增 `GET /api/v1/sources/{id}/raw`（内联输出，与 /download 共用流式内核）：按扩展名白名单放行（图片/视频/音频/PDF/文本/Markdown），inline + Content-Type 明示 + CSP(`default-src 'none'`) + nosniff；非白名单 415。前端预览弹窗：图片/视频/音频/PDF 原生渲染，文本/代码 `<pre>` 直读，Markdown 内置无依赖渲染器（先转义再套标签），文本 >2MB 拒绝预览。

| 用例 | 结果 |
|---|---|
| txt 预览：200 + `text/plain; charset=utf-8` + `inline` + CSP + nosniff | ✅ |
| Range 预览（bytes=0-4）→ 206（视频拖动进度条依赖此能力） | ✅ |
| HTML 预览：200 但强制 `text/plain` 直出（脚本失效，与 CSP 双保险） | ✅ |
| exe 等白名单外类型 → 415 NOT_PREVIEWABLE | ✅ |
| /download 保持 attachment，行为不变 | ✅ |
| 单测：preview_test.go 4 例（headers / 拒绝策略 / Range / attachment） | ✅ |

注：MKV 等依赖浏览器编解码器的格式由前端 fallback 到下载；Office 文档预览（需 mammoth/SheetJS 等前端重依赖）排 M1+。

### 8. 待办（转入 M1）

- 真实云联调：OSS / COS / R2（含 virtual-hosted、aws-chunked 差异）——签名正确性尚未被真实验证
- 鉴权（JWT）、源管理 API、凭据 AES-GCM 加密落库
- 索引缓存（TTL + 定时同步）、游标分页的前端接入
- FTP/SFTP 驱动
