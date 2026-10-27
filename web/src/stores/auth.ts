// 鉴权状态：token（sessionStorage）、当前用户、角色、引导/登录弹窗控制。
import { defineStore } from 'pinia'
import { getToken, setToken, setUnauthorizedHandler } from '../api/client'
import * as api from '../api'
import type { Role } from '../api/types'

export const useAuthStore = defineStore('auth', {
  state: () => ({
    token: getToken(),
    user: '',
    role: '' as Role,
    /** null=未初始化；true=无用户需引导；false=正常登录 */
    needsBootstrap: null as boolean | null,
    /** 登录/引导弹窗可见性 */
    authDialogVisible: false,
  }),
  getters: {
    isLoggedIn: (s) => !!s.token && !!s.user,
    isAdmin: (s) => s.role === 'admin',
    roleLabel: (s) => (s.role === 'admin' ? '管理员' : s.role === 'guest' ? '只读' : ''),
  },
  actions: {
    /** 注册 401 全局拦截 → 弹登录 */
    bindInterceptor() {
      setUnauthorizedHandler(() => {
        this.token = ''
        this.user = ''
        this.role = ''
        this.authDialogVisible = true
      })
    },
    /** 启动初始化：status → token 验证 → 弹引导或登录 */
    async init() {
      try {
        const st = await api.getAuthStatus()
        if (!st.auth_enabled) {
          this.needsBootstrap = false
          return // 无鉴权旧版兼容（理论上不会再出现）
        }
        this.needsBootstrap = st.needs_bootstrap
        if (this.token) {
          try {
            await this.refreshMe()
            return // token 有效，直接进入
          } catch {
            /* token 失效，走登录 */
          }
        }
        this.authDialogVisible = true
      } catch {
        // status 接口不可达：按无鉴权兼容处理
        this.needsBootstrap = false
      }
    },
    async refreshMe() {
      const me = await api.getMe()
      this.user = me.user
      this.role = me.role
    },
    async login(username: string, password: string) {
      const r = await api.login(username, password)
      this.afterAuth(r)
    },
    async bootstrap(username: string, password: string) {
      const r = await api.bootstrap(username, password)
      this.needsBootstrap = false
      this.afterAuth(r)
    },
    afterAuth(r: { user: string; role: string }) {
      this.token = getToken() // client 已从 X-Auth-Token 捕获
      this.user = r.user
      this.role = r.role as Role
      this.authDialogVisible = false
    },
    async logout() {
      try { await api.logout() } catch { /* 忽略 */ }
      setToken('')
      this.token = ''; this.user = ''; this.role = ''
      location.href = '/'
    },
  },
})
