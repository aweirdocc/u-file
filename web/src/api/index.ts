// 后端 API 调用函数全集。
import { get, post, put, del, getText, upload, getToken } from './client'
import { encryptPassword } from '../utils/crypto'
import type {
  Source, Entry, ListResult, TestResult, ImportReport,
  SyncStatus, SearchResponse, AuthStatus, Me, UserInfo,
  CreateUserInput, UpdateUserInput, Policy, ShareInfo, OfficeResult,
  TaskItem, TransferTaskInput,
  APITokenInfo, CreateAPITokenInput, CreateAPITokenResponse,
  WebhookEndpoint, UpsertWebhookInput, WebhookTestResult,
  MountItem, UpsertMountInput, FullTextStatus,
} from './types'

const enc = encodeURIComponent

// —— 鉴权与用户管理 ——

/** 获取登录密码加密公钥（SPKI PEM）。 */
export const getLoginPubKey = () => get<{ alg: string; public_key: string }>('/api/v1/auth/pubkey')

/**
 * 构造密码提交体：优先信封加密（RSA-OAEP），浏览器不支持/失败时回退明文。
 * 服务端两种都接受（password_enc 优先）。
 */
async function passwordPayload(password: string): Promise<Record<string, string>> {
  try {
    const { public_key } = await getLoginPubKey()
    const password_enc = await encryptPassword(password, public_key)
    if (password_enc) return { password: '', password_enc }
  } catch { /* 公钥不可达：回退明文 */ }
  return { password }
}

export const getAuthStatus = () => get<AuthStatus>('/api/v1/auth/status')
export const getMe = () => get<Me>('/api/v1/auth/me')
export const login = async (username: string, password: string) =>
  post<{ user: string; role: string }>('/api/v1/auth/login', { username, ...(await passwordPayload(password)) })
export const bootstrap = async (username: string, password: string) =>
  post<{ user: string; role: string }>('/api/v1/auth/bootstrap', { username, ...(await passwordPayload(password)) })
export const logout = () => post<{ ok: boolean }>('/api/v1/auth/logout')

export const listUsers = () => get<UserInfo[]>('/api/v1/auth/users')
export const createUser = async (input: CreateUserInput) =>
  post<UserInfo>('/api/v1/auth/users', {
    username: input.username,
    role: input.role,
    disabled: input.disabled ?? false,
    remark: input.remark ?? '',
    policies: input.policies ?? [],
    ...(await passwordPayload(input.password)),
  })
export const updateUser = (name: string, input: UpdateUserInput) =>
  put<UserInfo>(`/api/v1/auth/users/${enc(name)}`, input)
export const updateUserPolicies = (name: string, policies: Policy[]) =>
  put<{ ok: boolean; policies: Policy[] }>(`/api/v1/auth/users/${enc(name)}/policies`, { policies })
export const deleteUser = (name: string) => del<{ ok: boolean }>(`/api/v1/auth/users/${enc(name)}`)
export const resetPassword = async (name: string, password: string) =>
  post<{ ok: boolean }>(`/api/v1/auth/users/${enc(name)}/password`, await passwordPayload(password))

// —— 源管理 ——

export const listSources = () => get<Source[]>('/api/v1/sources')
export const getSource = (id: string) => get<Source>(`/api/v1/sources/${enc(id)}`)
export const addSource = (id: string, name: string, type: string, config: Record<string, unknown>) =>
  post<Source>('/api/v1/sources', { id, name, type, config })
export const updateSource = (id: string, name: string, config: Record<string, unknown>) =>
  put<Source>(`/api/v1/sources/${enc(id)}`, { name, config })
export const removeSource = (id: string) => del<{ ok: boolean }>(`/api/v1/sources/${enc(id)}`)
export const testSource = (type: string, config: Record<string, unknown>, id?: string) =>
  post<TestResult>('/api/v1/sources/test', id ? { id, type, config } : { type, config })
export const exportUrl = (redact: boolean) => `/api/v1/sources/export?redact=${redact ? 1 : 0}`
export const exportText = (redact: boolean) => getText(exportUrl(redact))
export const importSources = (content: string, overwrite: boolean) =>
  post<ImportReport>('/api/v1/sources/import', { content, overwrite })

// —— 浏览 ——

export function listFiles(source: string, path: string, opts: { cursor?: string; refresh?: boolean; limit?: number } = {}) {
  const qs = new URLSearchParams({ path, limit: String(opts.limit ?? 50) })
  if (opts.cursor) qs.set('cursor', opts.cursor)
  if (opts.refresh) qs.set('refresh', '1')
  return get<ListResult>(`/api/v1/sources/${enc(source)}/files?${qs}`)
}

// —— 写操作 ——

export const mkdir = (source: string, path: string) =>
  post<{ ok: boolean; path: string }>(`/api/v1/sources/${enc(source)}/mkdir`, { path })
export const deleteEntries = (source: string, paths: string[]) =>
  post<{ ok: boolean; deleted: number; failed: number; items: { path: string; ok: boolean; error?: string }[] }>(
    `/api/v1/sources/${enc(source)}/delete`, { paths })
export const renameEntry = (source: string, from: string, to: string) =>
  post<{ ok: boolean }>(`/api/v1/sources/${enc(source)}/rename`, { from, to })
export function uploadFiles(source: string, path: string, files: File[]) {
  const form = new FormData()
  form.append('path', path)
  for (const f of files) form.append('file', f)
  return upload(`/api/v1/sources/${enc(source)}/upload`, form)
}

/** 带实时进度的 XHR 多文件上传（供任务中心统一接管上传进度条与速率）。 */
export function uploadFilesWithProgress(
  source: string,
  path: string,
  files: File[],
  onProgress?: (loaded: number, total: number) => void,
  signal?: AbortSignal,
): Promise<{ ok: boolean; uploaded: number; failed: number; items: { file: string; ok: boolean; error?: string }[] }> {
  return new Promise((resolve, reject) => {
    const form = new FormData()
    form.append('path', path)
    for (const f of files) form.append('file', f)

    const xhr = new XMLHttpRequest()
    xhr.open('POST', `/api/v1/sources/${enc(source)}/upload`, true)
    const tok = getToken()
    if (tok) xhr.setRequestHeader('Authorization', 'Bearer ' + tok)

    if (signal) {
      signal.addEventListener('abort', () => xhr.abort(), { once: true })
    }

    xhr.upload.onprogress = (ev) => {
      if (ev.lengthComputable && onProgress) {
        onProgress(ev.loaded, ev.total)
      }
    }
    xhr.onload = () => {
      let data: any = {}
      try { data = JSON.parse(xhr.responseText || '{}') } catch { /* ignore */ }
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(data)
      } else {
        reject(new Error(data.message || `${xhr.status} ${xhr.statusText}`))
      }
    }
    xhr.onerror = () => reject(new Error('网络错误：上传中断'))
    xhr.onabort = () => reject(new Error('上传已取消'))
    xhr.send(form)
  })
}

// —— 下载 / 预览 / 直链 / 打包（原生导航 URL 构造）——

export const downloadUrl = (source: string, path: string) =>
  `/api/v1/sources/${enc(source)}/download?path=${enc(path)}`
export const rawUrl = (source: string, path: string) =>
  `/api/v1/sources/${enc(source)}/raw?path=${enc(path)}`
export const thumbUrl = (source: string, path: string, w = 200, h = 200, q = 78) =>
  `/api/v1/sources/${enc(source)}/thumb?path=${enc(path)}&w=${w}&h=${h}&q=${q}`
export const getOfficePreview = (source: string, path: string) =>
  get<OfficeResult>(`/api/v1/sources/${enc(source)}/office?path=${enc(path)}`)
export const shareThumbUrl = (token: string, w = 200, h = 200, q = 78) =>
  `/api/v1/s/${enc(token)}/thumb?w=${w}&h=${h}&q=${q}`
export const getShareOfficePreview = (token: string) =>
  get<OfficeResult>(`/api/v1/s/${enc(token)}/office`)
export const archiveUrl = (source: string, paths: string[], name: string) =>
  `/api/v1/sources/${enc(source)}/archive?${paths.map(p => 'paths=' + enc(p)).join('&')}&name=${enc(name)}`
export async function presign(source: string, path: string): Promise<{ url: string; expires_in: number }> {
  const tok = sessionStorage.getItem('ufile_token') || ''
  const res = await fetch(`/api/v1/sources/${enc(source)}/presign?path=${enc(path)}`, {
    headers: tok ? { Authorization: 'Bearer ' + tok } : {},
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.message || '获取直链失败')
  return data
}

// —— 索引同步 / 搜索 ——

export const getSyncStatus = (source: string) => get<SyncStatus>(`/api/v1/sources/${enc(source)}/sync`)
export const triggerSync = (source: string) => post<{ ok: boolean }>(`/api/v1/sources/${enc(source)}/sync`)
export const search = (q: string, limit = 100, mode: 'filename' | 'fulltext' = 'filename', source = '') => {
  const qs = new URLSearchParams({ q, limit: String(limit), mode })
  if (source) qs.set('source', source)
  return get<SearchResponse>(`/api/v1/search?${qs}`)
}

// —— 分享外链（管理面）——

export const createShare = (sourceId: string, path: string, ttlHours: number, password: string, maxDownloads: number) =>
  post<ShareInfo>('/api/v1/shares', { source_id: sourceId, path, ttl_hours: ttlHours, password, max_downloads: maxDownloads })
export const listShares = () => get<ShareInfo[]>('/api/v1/shares')
export const deleteShare = (id: string) => del<{ ok: boolean }>(`/api/v1/shares/${enc(id)}`)

/** 访客页公开信息（无登录；404/410 交给调用方按状态处理） */
export const shareUrl = (token: string) => `/s/${enc(token)}`

// —— M4.1 传输任务中心与跨源流转 ——

export const listTasks = () => get<TaskItem[]>('/api/v1/tasks')
export const submitTransferTask = (input: TransferTaskInput) =>
  post<TaskItem>('/api/v1/tasks/transfer', input)
export const pauseTask = (id: string) => post<TaskItem>(`/api/v1/tasks/${enc(id)}/pause`)
export const resumeTask = (id: string) => post<TaskItem>(`/api/v1/tasks/${enc(id)}/resume`)
export const cancelTask = (id: string) => post<TaskItem>(`/api/v1/tasks/${enc(id)}/cancel`)
export const retryTask = (id: string) => post<TaskItem>(`/api/v1/tasks/${enc(id)}/retry`)
export const deleteTask = (id: string) => del<{ ok: boolean }>(`/api/v1/tasks/${enc(id)}`)
export const clearFinishedTasks = () => post<{ ok: boolean; cleared: number }>('/api/v1/tasks/clear')

// —— M4.2 开放 API Token ——

export const listAPITokens = () => get<APITokenInfo[]>('/api/v1/tokens')
export const createAPIToken = (input: CreateAPITokenInput) =>
  post<CreateAPITokenResponse>('/api/v1/tokens', input)
export const revokeAPIToken = (id: string) => post<{ ok: boolean }>(`/api/v1/tokens/${enc(id)}/revoke`)
export const deleteAPIToken = (id: string) => del<{ ok: boolean }>(`/api/v1/tokens/${enc(id)}`)

// —— M4.2 事件 Webhook ——

export const listWebhooks = () => get<WebhookEndpoint[]>('/api/v1/webhooks')
export const createWebhook = (input: UpsertWebhookInput) =>
  post<WebhookEndpoint>('/api/v1/webhooks', input)
export const updateWebhook = (id: string, input: UpsertWebhookInput) =>
  put<WebhookEndpoint>(`/api/v1/webhooks/${enc(id)}`, input)
export const deleteWebhook = (id: string) => del<{ ok: boolean }>(`/api/v1/webhooks/${enc(id)}`)
export const testWebhook = (id: string) => post<WebhookTestResult>(`/api/v1/webhooks/${enc(id)}/test`)

// —— M4.3 虚拟多挂载点树 VFS ——

export const listMounts = () => get<MountItem[]>('/api/v1/mounts')
export const createMount = (input: UpsertMountInput) =>
  post<MountItem>('/api/v1/mounts', input)
export const updateMount = (id: string, input: UpsertMountInput) =>
  put<MountItem>(`/api/v1/mounts/${enc(id)}`, input)
export const deleteMount = (id: string) => del<{ ok: boolean }>(`/api/v1/mounts/${enc(id)}`)

// —— M4.3 全文检索引擎 ——

export const getFullTextStatus = () => get<FullTextStatus>('/api/v1/fulltext/status')
export const rebuildFullText = (sourceId = '') =>
  post<{ ok: boolean; message: string }>('/api/v1/fulltext/rebuild', { source_id: sourceId })

export type { Source, Entry }
