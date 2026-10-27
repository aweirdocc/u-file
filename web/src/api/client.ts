// 统一 fetch 封装：Bearer 头、X-Auth-Token 捕获、401 全局拦截。
// token 存 sessionStorage（会话级）；预览/下载/打包等原生导航场景由后端
// HttpOnly cookie 兜底，不走本封装。

const TOKEN_KEY = 'ufile_token'

export function getToken(): string {
  return sessionStorage.getItem(TOKEN_KEY) || ''
}

export function setToken(tok: string) {
  if (tok) sessionStorage.setItem(TOKEN_KEY, tok)
  else sessionStorage.removeItem(TOKEN_KEY)
}

/** 401 时的全局回调（由 auth store 注册，弹出登录框） */
let onUnauthorized: (() => void) | null = null
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn
}

export class ApiError extends Error {
  status: number
  code: string
  constructor(status: number, code: string, message: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

export async function request<T>(
  method: string,
  url: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const tok = getToken()
  if (tok) headers['Authorization'] = 'Bearer ' + tok

  const res = await fetch(url, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })

  // 登录/引导成功：捕获下发的 token
  const newTok = res.headers.get('X-Auth-Token')
  if (newTok) setToken(newTok)

  if (res.status === 401) {
    onUnauthorized?.()
    throw new ApiError(401, 'UNAUTHORIZED', '请先登录')
  }

  let data: any = null
  try {
    data = await res.json()
  } catch {
    /* 空响应体 */
  }

  if (!res.ok) {
    throw new ApiError(
      res.status,
      data?.code || 'ERROR',
      data?.message || `${res.status} ${res.statusText}`,
    )
  }
  return data as T
}

export const get = <T>(url: string) => request<T>('GET', url)
export const post = <T>(url: string, body?: unknown) => request<T>('POST', url, body)
export const put = <T>(url: string, body?: unknown) => request<T>('PUT', url, body)
export const del = <T>(url: string) => request<T>('DELETE', url)

/** 文本响应（导出复制等），带 Bearer */
export async function getText(url: string): Promise<string> {
  const headers: Record<string, string> = {}
  const tok = getToken()
  if (tok) headers['Authorization'] = 'Bearer ' + tok
  const res = await fetch(url, { headers })
  if (!res.ok) throw new ApiError(res.status, 'ERROR', `${res.status} ${res.statusText}`)
  return res.text()
}

/** multipart 上传（不设 Content-Type，让浏览器生成 boundary） */
export async function upload(url: string, form: FormData): Promise<Response> {
  const headers: Record<string, string> = {}
  const tok = getToken()
  if (tok) headers['Authorization'] = 'Bearer ' + tok
  return fetch(url, { method: 'POST', headers, body: form })
}
