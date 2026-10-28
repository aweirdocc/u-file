// 后端 API 调用函数全集（与旧 UI 的 24 个调用点一一对应）。
import { get, post, put, del, getText, upload } from './client'
import { encryptPassword } from '../utils/crypto'
import type {
  Source, Entry, ListResult, TestResult, ImportReport,
  SyncStatus, SearchResponse, AuthStatus, Me, UserInfo, ShareInfo,
} from './types'

const enc = encodeURIComponent

// —— 鉴权 ——

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
export const createUser = async (username: string, password: string, role: string) =>
  post<{ user: string; role: string }>('/api/v1/auth/users', { username, role, ...(await passwordPayload(password)) })
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
export const testSource = (type: string, config: Record<string, unknown>) =>
  post<TestResult>('/api/v1/sources/test', { type, config })
export const exportUrl = (redact: boolean) => `/api/v1/sources/export${redact ? '?redact=1' : ''}`
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

// —— 下载 / 预览 / 直链 / 打包（原生导航 URL 构造）——

export const downloadUrl = (source: string, path: string) =>
  `/api/v1/sources/${enc(source)}/download?path=${enc(path)}`
export const rawUrl = (source: string, path: string) =>
  `/api/v1/sources/${enc(source)}/raw?path=${enc(path)}`
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
export const search = (q: string, limit = 100) =>
  get<SearchResponse>(`/api/v1/search?q=${enc(q)}&limit=${limit}`)

// —— 分享外链（管理面）——

export const createShare = (sourceId: string, path: string, ttlHours: number, password: string, maxDownloads: number) =>
  post<ShareInfo>('/api/v1/shares', { source_id: sourceId, path, ttl_hours: ttlHours, password, max_downloads: maxDownloads })
export const listShares = () => get<ShareInfo[]>('/api/v1/shares')
export const deleteShare = (id: string) => del<{ ok: boolean }>(`/api/v1/shares/${enc(id)}`)

/** 访客页公开信息（无登录；404/410 交给调用方按状态处理） */
export const shareUrl = (token: string) => `/s/${enc(token)}`

export type { Source, Entry }
