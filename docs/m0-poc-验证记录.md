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

### 8. 配置导出 / 导入（2026-10-27 追加）

- 导出：`GET /api/v1/sources/export`（JSON 附件，`?redact=1` 凭据打码；全量导出文件名 `ufile-sources-full.json` 即提示含明文）
- 导入：`POST /api/v1/sources/import`，兼容完整导出文件 / 裸数组 / 字符串化 JSON 三种载荷；逐条独立处理（单条失败不影响其余），报告 added/updated/skipped/failed；`overwrite` 控制同 ID 覆盖或跳过；识别脱敏凭据并在报告计数提醒补填
- Web：头部「导出配置」（脱敏开关 + 下载/复制）与「导入配置」（选文件自动读入文本框/直接粘贴 + 覆盖开关 + 逐条结果展示）

| 用例 | 结果 |
|---|---|
| 导出全量 200 + attachment 文件名；redact 后凭据为 ****** | ✅ |
| 导入新增 → added；重复导入不覆盖 → skipped（含提示文案） | ✅ |
| overwrite=true → updated，名称真实更新 | ✅ |
| 导出文件原文直接 POST（不经 content 包装）→ 全部 skipped/updated 正确 | ✅ |
| 脱敏导出再导入 → redacted_count 正确计数 | ✅ |
| 非法 JSON → 400 IMPORT_FAILED | ✅ |
| 导入后重启持久化；单测 6 例（往返/覆盖跳过/双格式/脱敏/跨实例） | ✅ |

### 9. 待办（转入 M1）

- 真实云联调：OSS / COS / R2（含 virtual-hosted、aws-chunked 差异）——签名正确性尚未被真实验证
- 鉴权（JWT）、源管理 API、凭据 AES-GCM 加密落库
- 索引缓存（TTL + 定时同步）、游标分页的前端接入
- FTP/SFTP 驱动

---

## M1 验证记录（2026-10-27）

> 范围：P0 安全基线（凭据加密、登录鉴权、审计）+ 索引缓存与后台同步 + 源编辑。
> 环境：Windows 10 x64 / Go 1.27.1。注：本机无 C 编译器，`-race` 不可用，以 `-count=3` 多轮并发测试替代。

### 1. 凭据加密落库（internal/crypt + internal/source）

- AES-256-GCM，密文格式 `enc:v1:<b64(nonce|ct)>`；主密钥 `data/master.key`（32B 随机，0600，首次自动生成）
- 内存明文 / 落库密文：加解密只在 sources.json 读写边界；非敏感字段（endpoint/bucket）保持明文便于排查
- **旧明文自动迁移**：加载时发现明文凭据 → 内存解出后立即加密重写（实测：把 M0 明文文件放入后启动，日志输出「已将明文凭据加密迁移」，磁盘不再含明文）
- 主密钥不匹配：该源跳过并 warn，不阻断启动（其余源正常）
- 单测：往返/随机 nonce/篡改检测/换钥匙失败/迁移/落盘无明文（`crypt_test.go`、`crypto_persist_test.go`）

### 2. 鉴权（internal/auth + API 中间件）

- 用户存 `data/users.json`（bcrypt，0600）；JWT HS256 签名密钥 `data/jwt.key`（与主密钥分离）
- 引导流程：无用户时 `POST /auth/bootstrap` 创建首个管理员，创建后该入口永久 403（实测重放 → 403）
- 角色：admin 全权；guest 只读（实测：列源/浏览 200，新增/删除/导出/列用户 403）
- 登录限速：按「用户名+IP」连续失败 5 次起指数退避（实测：错 10 次后正确密码也被拒，文案含剩余等待；5s 后恢复）
- 会话双通道：XHR 走 `Authorization: Bearer`；预览/下载标签走 HttpOnly Cookie（实测 cookie 访问 /raw 成功）
- 安全细节：用户不存在时也做 bcrypt 比较（抹平时序）；用户被删后其 token 立即失效（Verify 查库）
- 单测：`auth_test.go` 8 例（引导/角色/时序/最后管理员/改密/限速/退避增长/删除用户失效）

### 3. 审计日志（internal/audit）

- 追加写 `data/audit.log`（JSONL）：登录/登出/用户管理/源增删改/导入/手动同步
- 实测记录完整，user 字段对登录类动作记录尝试登录的用户名（登录时尚无会话，auditActionAs 显式传入）

### 4. 索引缓存与后台同步（internal/index）

- 目录列表 TTL 缓存（10 分钟）+ `?refresh=1` 绕过；游标分页请求永不缓存
- 后台每小时全量同步：BFS 遍历目录树逐目录刷新（单源 10 分钟/1 万目录上限）；失败记录原因与时间，下轮重试
- 快照 `data/index.json`：同步后落盘，重启加载（实测：重启后 List 直接命中不回源）
- 单测：`index_test.go` 9 例（命中/refresh/TTL 过期/游标不缓存/BFS 遍历/失败记录与恢复/Forget/快照往返/StartStop 循环）

### 5. 端到端冒烟（真实二进制 + curl，隔离 data 目录）

| 用例 | 结果 |
|---|---|
| 未登录访问任意 API → 401 | ✅ |
| 引导创建管理员 201 + X-Auth-Token；重放 403 | ✅ |
| admin 列源/新增/浏览/下载/PUT 编辑 | ✅ |
| guest 创建 → 只读放行、写操作 403 | ✅ |
| Cookie 方式访问 /raw（预览场景） | ✅ |
| 手动触发同步 202 → 状态 running→success；快照落盘 | ✅ |
| users.json 仅 bcrypt 哈希、sources.json 无明文凭据 | ✅ |
| 登录连续失败触发限速，退避结束后恢复 | ✅ |
| 重启后管理员仍在、索引快照加载 | ✅ |
| 审计日志完整（bootstrap/login/user.create/source.update/sync_trigger） | ✅ |

### 6. 过程中发现并修复的缺陷

| 缺陷 | 影响 | 修复 |
|---|---|---|
| 数据目录硬编码 `./data`，与 `-config` 指定的目录脱节 | 配置与数据分离部署时写错位置 | 新增 `-data` 参数，全链路传递 |
| auditAction 在登录/引导阶段读不到会话用户，记录 `<nil>` | 审计无法追溯操作者 | 新增 auditActionAs 显式传用户名 |
| index.Cache 源枚举依赖「已浏览过」的缓存键 | 从未浏览的源不参与后台同步 | 装配时注入 `listIDs` 回调（接 source.Manager.IDs） |
| dirCache 无 JSON tag | 快照保存后字段丢失，恢复后缓存全空 | 补 `at`/`res` tag |
| 测试向 failOn map 存 nil error | List 误判为应失败 → 空指针 | delete 键代替存 nil |

### 7. M1 已知边界 / 转入 M2

- 真实云联调仍待做（gofakes3 不验签名；本轮冒烟未覆盖 S3 源的鉴权链路，仅 local）
- 游标分页前端未接（S3 大目录翻页仍用「下一页/上一页」栈式游标，可用；万级目录的跳页体验在 M2 搜索/排序一起做）
- FTP/SFTP 驱动、上传、删除/重命名等写操作 → M2
- SQLite 迁移：当前文件型存储在万级以下条目够用，索引规模上来后再评估

---

## M2 验证记录（2026-10-27）

> 范围：P1 写能力（上传/删除/重命名/建目录）、批量与 zip 打包下载、文件名搜索、FTP/FTPS/SFTP 驱动。
> 模块代理：本机默认 proxy.golang.org 不可达，依赖拉取走 `GOPROXY=https://goproxy.cn,direct`。

### 1. 驱动写能力（可选 Writer 接口）

- 接口：`Upload(path, r, size) / Delete / Mkdir / Rename(from, to)`；能力位 `Write` 声明给前端显隐
- local：MkdirAll 父目录、覆盖写、RemoveAll 递归删、Rename（含跨盘退化复制删除、禁止移进自己/根）
- s3：PutObject 上传、DeleteObject + 前缀批量 DeleteObjects（目录递归删）、零字节占位对象建目录、
  CopyObject+Delete 重命名（目录逐对象拷贝）；无凭据的匿名源不声明 Write（只读）
- 冒烟（对 fakes3，`TestS3WriteSmoke`，环境变量门控同 TestS3Smoke）：建目录→上传→读回→文件改名→目录改名→递归删，全部通过

### 2. FTP/FTPS 驱动（internal/driver/ftp，jlaffaye/ftp v0.2.4）

- 配置：host/port/username/password/tls(FTPS)/insecure_tls/root
- 连接管理：池内单控制连接 + NOOP 探活 + 断线重拨重试一次；**下载每次专用连接**（FTP 控制连接上的数据传输互踩，不复用）
- 续传：RetrFrom(REST)；列表：List（解析目录/文件/大小/时间）；写：Stor / Delete / RemoveDirRecur / Rename / 逐级 MakeDir
- 单测：配置解析、路径（..、反斜杠拒绝）、能力位、550/450 错误分类；真实服务器集成 `TestFTPLiveSmoke` 环境变量门控（本机无 FTP 服务，未跑）

### 3. SFTP 驱动（internal/driver/sftpdrv，pkg/sftp v1.13.12）

- 配置：host/port/username + password 或 private_key（PEM 内容或文件路径，passphrase 可选）
- host key 策略：accept-new 语义（首连记录、变更拒绝，防静默中间人；文档注明 M2 简化边界）
- SFTP 单连接多路复用（无 FTP 数据通道问题），断线重拨；Seek 支持随机读
- 单测：配置校验（凭据必填其一）、密码/私钥（PKCS#8 PEM 文件与内容两种形态）/坏私钥、路径、
  host key 策略矩阵（首连/同密钥/变更/异主机）；真实集成 `TestSFTPLiveSmoke` 门控未跑

### 4. API 写端点 + 打包 + 搜索

- `POST /upload`（multipart 多文件逐个报告）、`POST /mkdir`、`POST /delete`（批量，目录递归）、`POST /rename`
- `GET /archive?paths=...`：zip 流式打包（zip.Store 不二次压缩；上限 1 万文件/2GB；目录 BFS 递归）
- `GET /search?q=&source=&limit=`：基于索引缓存/快照的跨源文件名搜索（大小写不敏感包含匹配）
- 全部写端点 admin only + 审计；写后自动失效该源索引缓存

### 5. 前端（内嵌单页）

- 工具栏：⬆ 上传（多选文件、进度文案、部分失败明细）、📁 新建文件夹；仅 admin + 源有 write 能力时显示
- 行内勾选（目录/文件统一）→「🗜 打包下载选中」「🗑 删除选中（N）」；每行「重命名」（prompt 改名）
- 顶栏搜索框：回车跨源搜索，结果列源名/路径，目录可「进入」跳转浏览

### 6. 端到端冒烟（真实二进制 + fakes3 + curl）

| 用例 | 结果 |
|---|---|
| local multipart 双文件上传 → 逐文件 ok + 立即可见（缓存失效链路） | ✅ |
| S3 上传 → 下载读回内容一致；mkdir 嵌套 x/y/z → 列表见占位 | ✅ |
| S3 文件改名读回同内容；目录改名后内容跟随 | ✅ |
| local 改名+移动（docs/f2 → docs/sub/moved）、删除 | ✅ |
| S3 目录递归删（x 含 y2/z）→ 列表为空 | ✅ |
| zip 打包（单文件 + 目录递归）→ Go 标准库解包核对 4 个文件内容与路径 | ✅ |
| 搜索：先触发同步再搜，跨源命中（txt 5 条）、按源过滤、目录跳转 | ✅ |
| guest 权限矩阵：browse/download 200；upload/mkdir/delete/rename 全 403 | ✅ |
| 审计：file.upload/delete/mkdir/rename/archive、index.sync_trigger 全记录 | ✅ |

### 7. M2 已知边界 / 转入 M3

- FTP/SFTP 驱动未对真实服务器联调（本机无服务；协议行为由库层保证 + 单测覆盖配置/错误面）
- SFTP host key 完整 TOFU（known_hosts 持久化）未做，重启后重新首连
- 大目录体验：搜索依赖同步深度（万级条目目录已同步即可命中）；全文检索/排序在 P3
- 数据库源（BLOB/GridFS）、分享外链、多用户 ACL、WebDAV 服务端 → M3（P2）

---

## 前端重构验证记录（2026-10-27，Vue 3 + Element Plus）

> 范围：将 963 行单文件原生 JS UI 重构为 Vue 3 + TS + Element Plus + Pinia + Vue Router + Vite 工程（docs/04 既定技术栈落地），产物仍内嵌单二进制。

### 1. 工程结构（web/）

- `.npmrc` 项目级覆盖：registry=npmmirror + 置空代理（本机全局代理 127.0.0.1:7897 已失效，直连正常）
- Vite：`server.proxy['/api'] → localhost:8080`（开发期同源，cookie SameSite=Lax 正常，后端零 CORS 改动）
- 分层：`api/`（client 统一 Bearer/401 拦截/X-Auth-Token 捕获 + 类型化调用函数）· `stores/`（auth/browse）· `components/`（13 个组件）· `utils/`（format/preview + Markdown 渲染器迁移）
- 构建：`vue-tsc -b` 严格模式零错误；vite build 产物 gzip 约 366KB（Element Plus 全量）

### 2. 后端接入

- `web/embed.go`：`//go:embed all:dist`（embed 不能引用包外目录，故放 web/ 内）
- `api.New` 增加 `webFS fs.FS` 参数；`GET /` 直出 index.html；NoRoute 改造：`/api/*` 保持 JSON 404，
  其余先试静态文件、miss 回退 index.html（SPA 深层路由刷新不 404）
- `-web` flag：指向磁盘目录覆盖内嵌产物（前端开发调试逃生口）
- 旧 `internal/api/static/index.html` 已删除（git 历史可找回）

### 3. 鉴权通道统一（修掉旧版缺陷）

- 全部 XHR 统一走 Bearer 头（旧版 files 列表/export 复制是裸 fetch 隐式依赖 cookie）
- download/raw/archive 保留 `<a href>` 原生导航（HttpOnly cookie 兜底），与后端双通道设计一致
- 401 全局拦截 → 自动弹登录框；token 存 sessionStorage

### 4. 顺手修复的旧版缺陷

- 同步状态条 running 轮询叠加（每次 render 新起一条链）→ 单实例轮询 + 源切换作废旧响应
- 搜索结果复用浏览表头（列语义不匹配）→ 独立 SearchPanel（来源/路径列）
- 删除当前源/导入后源消失不同步 URL → 统一路由驱动，URL 始终与状态一致
- 同步失败 `.bad` 无红色样式 → 补齐
- 全部弹窗支持 Esc 关闭（el-dialog 默认），confirm/prompt/alert 全部升级为 ElMessageBox/ElMessage

### 5. 浏览器端到端实测（Chrome 控件 + 内嵌产物，18096 端口）

| 用例 | 结果 |
|---|---|
| 首访弹出「初始化：创建管理员」引导框 | ✅ |
| 填表创建管理员 → 弹窗关、头部显示 admin（管理员）、admin 按钮组出现 | ✅ |
| 新增源：类型切换字段组、测试连接 ✓、保存按钮先禁用后启用、保存成功 | ✅ |
| 源标签点击 → 文件列表（勾选列/预览/下载/重命名）、同步状态条、上传/新建工具栏 | ✅ |
| Markdown 预览渲染（h1/ul/strong 正确） | ✅ |
| 新建文件夹 → prompt 确认 → 列表即时刷新（缓存失效链路） | ✅ |
| 目录进入/返回上级，面包屑两级正确，URL 路由同步 | ✅ |
| 顶栏搜索回车 → 独立结果表（来源/路径/进入）→ 返回浏览 | ✅ |
| 勾选 → 「已选 1 项」+ 删除按钮 → 确认框 → 列表与磁盘同步移除 | ✅ |

### 6. 已知边界

- Element Plus 全量引入（bundle 偏大）；按需引入/手动分包留作后续优化
- 用户管理界面（auth/users API）暂无页面，M3 随多用户 ACL 一起做
- 文件上传走原生 input multiple（el-upload 手动模式语义等价）；分片上传属 M3+ 传输中心范畴
