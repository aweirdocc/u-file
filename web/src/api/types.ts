// 后端 API 的 TypeScript 类型定义（与 internal/ 包的 JSON 结构一一对应）。

export type Role = 'admin' | 'guest' | ''

export interface Capabilities {
  list: boolean
  stat: boolean
  download: boolean
  presign_url: boolean
  list_cursor: boolean
  write: boolean
}

/** 文件源（列表/详情；config 中凭据为打码值 ******） */
export interface Source {
  id: string
  name: string
  type: string
  config: Record<string, unknown> | null
  capabilities: Capabilities
}

/** 文件/目录条目 */
export interface Entry {
  path: string
  name: string
  is_dir: boolean
  size: number
  mod_time: string
  etag?: string
}

export interface ListResult {
  entries: Entry[]
  next_cursor?: string
}

export interface TestResult {
  ok: boolean
  latency_ms: number
  error?: string
}

export interface ImportItem {
  id: string
  status: 'added' | 'updated' | 'skipped' | 'failed'
  error?: string
}

export interface ImportReport {
  total: number
  added: number
  updated: number
  skipped: number
  failed: number
  items: ImportItem[]
  redacted_count: number
}

export interface SyncStatus {
  source_id: string
  last_success?: string
  last_error?: string
  last_error_at?: string
  running: boolean
  current_dir?: string
  dirs: number
  entries: number
  synced_dirs: number
}

export interface SearchHit {
  source_id: string
  source_name: string
  path: string
  name: string
  is_dir: boolean
  size: number
  mod_time: string
}

export interface SearchResponse {
  query: string
  count: number
  hits: SearchHit[]
}

export interface AuthStatus {
  auth_enabled: boolean
  needs_bootstrap: boolean
}

export interface Me {
  user: string
  role: Role
}

export interface UserInfo {
  username: string
  role: Exclude<Role, ''>
  created_at: string
}

/** 分享外链（管理面视图；不含提取码哈希） */
export interface ShareInfo {
  id: string
  token: string
  source_id: string
  path: string
  name: string
  size: number
  is_dir: boolean
  has_password: boolean
  expires_at: string
  max_downloads?: number
  downloads: number
  created_by: string
  created_at: string
  revoked: boolean
  /** 仅公开信息接口返回：当前访客是否已解锁（无码或已通过提取码） */
  unlocked?: boolean
}
