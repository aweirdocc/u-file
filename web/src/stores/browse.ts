// 浏览状态：当前源/路径、游标分页栈、源列表、搜索模式、同步状态与细粒度权限判断。
import { defineStore } from 'pinia'
import * as api from '../api'
import type { Source, Entry, SearchHit } from '../api/types'
import { useAuthStore } from './auth'

export interface Crumb { name: string; path: string }

export const useBrowseStore = defineStore('browse', {
  state: () => ({
    sources: [] as Source[],
    /** 当前源 ID；'' = 未选择 */
    source: '',
    /** 当前目录（源内相对路径，'' = 根） */
    path: '',
    /** 游标分页：每页起始游标栈，栈顶为当前页；'' = 从头 */
    pageStack: [''] as string[],
    curNextCursor: '',
    entries: [] as Entry[],
    loading: false,
    error: '',
    /** 非空 = 搜索模式，值为查询词 */
    searchQuery: '',
    searchMode: 'filename' as 'filename' | 'fulltext',
    searchHits: [] as SearchHit[],
    searchLoading: false,
    /** 勾选的路径 */
    selected: [] as string[],
    /** 弹窗控制 */
    sourceDialogMode: null as null | { edit?: string }, // null=关闭；{edit}=编辑；{}=新增
    exportDialogVisible: false,
    importDialogVisible: false,
    previewEntry: null as Entry | null,
  }),
  getters: {
    currentSource: (s) => s.sources.find(x => x.id === s.source) || null,
    /** 当前源对当前用户的单一限制根路径前缀（仅单目录锁定时非空；全选或多目录时为空） */
    rootPrefix(): string {
      const acc = this.currentSource?.access
      if (!acc) return ''
      if (acc.path_prefixes && acc.path_prefixes.length === 1) return acc.path_prefixes[0]
      if (acc.path_prefixes && acc.path_prefixes.length > 1) return ''
      return acc.path_prefix || ''
    },
    /** 当前所在目录是否在用户直接授权的目录内（多目录勾选时公共父目录仅做导航，不可直接写） */
    isInAllowedPath(): boolean {
      const acc = this.currentSource?.access
      if (!acc) return true
      const pfxs = acc.path_prefixes?.length
        ? acc.path_prefixes
        : acc.path_prefix ? [acc.path_prefix] : []
      if (!pfxs.length) return true
      const cur = this.path
      return pfxs.some(p => cur === p || cur.startsWith(p + '/'))
    },
    canWrite(): boolean {
      const auth = useAuthStore()
      const src = this.currentSource
      if (!src || !src.capabilities.write || !this.isInAllowedPath) return false
      if (src.access) return src.access.actions.includes('write')
      return auth.isAdmin
    },
    canDelete(): boolean {
      const auth = useAuthStore()
      const src = this.currentSource
      if (!src || !src.capabilities.write || !this.isInAllowedPath) return false
      if (src.access) return src.access.actions.includes('delete')
      return auth.isAdmin
    },
    canShare(): boolean {
      const auth = useAuthStore()
      const src = this.currentSource
      if (!src || !this.isInAllowedPath) return false
      if (src.access) return src.access.actions.includes('share')
      return auth.isAdmin
    },
    canPresign(): boolean {
      return !!this.currentSource?.capabilities.presign_url
    },
    hasCursor(): boolean {
      return !!this.currentSource?.capabilities.list_cursor
    },
    breadcrumbs(): Crumb[] {
      const rootPfx = this.rootPrefix
      const rootLabel = rootPfx ? `${this.source} (${rootPfx})` : this.source
      const crumbs: Crumb[] = [{ name: rootLabel, path: rootPfx }]
      if (!this.path || this.path === rootPfx) return crumbs
      const rel = rootPfx && this.path.startsWith(rootPfx + '/')
        ? this.path.slice(rootPfx.length + 1)
        : this.path
      let acc = rootPfx
      for (const seg of rel.split('/').filter(Boolean)) {
        acc += (acc ? '/' : '') + seg
        crumbs.push({ name: seg, path: acc })
      }
      return crumbs
    },
    pageNum(): number { return this.pageStack.length },
    isSearching(): boolean { return this.searchQuery !== '' },
  },
  actions: {
    /** 登出/401 后清空全部浏览状态（避免下一个会话看到上个会话的残留） */
    reset() {
      this.sources = []
      this.source = ''
      this.path = ''
      this.pageStack = ['']
      this.curNextCursor = ''
      this.entries = []
      this.loading = false
      this.error = ''
      this.searchQuery = ''
      this.searchHits = []
      this.searchLoading = false
      this.selected = []
      this.sourceDialogMode = null
      this.exportDialogVisible = false
      this.importDialogVisible = false
      this.previewEntry = null
    },
    /** 从 URL 恢复 source/path */
    fromRoute(source: string, path: string) {
      if (this.source !== source) {
        this.entries = []
        this.error = ''
      }
      this.source = source
      this.path = path
      this.searchQuery = ''
      this.pageStack = ['']
      this.curNextCursor = ''
      this.selected = []
    },
    /** 确保当前路径不越出源的授权路径前缀 */
    alignPathToPrefix() {
      const pfx = this.rootPrefix
      if (!pfx) return
      if (!this.path || (this.path !== pfx && !this.path.startsWith(pfx + '/'))) {
        this.path = pfx
      }
    },
    /** 导航（替代旧 nav()）；URL 由组件层用 router 同步 */
    async navigate(source: string, path: string) {
      this.fromRoute(source, path)
      await this.loadSources()
      this.alignPathToPrefix()
      await this.loadEntries()
    },
    async refresh() {
      await this.loadSources()
      this.alignPathToPrefix()
      await this.loadEntries(true)
    },
    async loadSources() {
      try {
        this.sources = await api.listFiles === undefined ? [] : await api.listSources()
      } catch (e) {
        this.sources = []
        throw e
      }
    },
    async loadEntries(refresh = false) {
      if (!this.source || this.isSearching) return
      this.alignPathToPrefix()
      this.loading = true
      this.error = ''
      try {
        const res = await api.listFiles(this.source, this.path, {
          cursor: this.pageStack[this.pageStack.length - 1] || undefined,
          refresh,
        })
        this.entries = res.entries || []
        this.curNextCursor = res.next_cursor || ''
      } catch (e: any) {
        this.entries = []
        this.error = e.message || String(e)
      } finally {
        this.loading = false
      }
    },
    async nextPage() {
      if (!this.curNextCursor) return
      this.pageStack.push(this.curNextCursor)
      await this.loadEntries()
    },
    async prevPage() {
      if (this.pageStack.length <= 1) return
      this.pageStack.pop()
      await this.loadEntries()
    },
    async runSearch(q: string, mode?: 'filename' | 'fulltext') {
      if (mode) this.searchMode = mode
      this.searchQuery = q
      this.searchHits = []
      if (!q) return
      this.searchLoading = true
      try {
        const r = await api.search(q, 100, this.searchMode)
        this.searchHits = r.hits
      } finally {
        this.searchLoading = false
      }
    },
    exitSearch() {
      this.searchQuery = ''
      this.searchHits = []
      this.loadEntries()
    },
    /** 写操作后调用：失效缓存的强刷新 */
    async afterWrite() {
      await this.loadEntries(true)
    },
  },
})
