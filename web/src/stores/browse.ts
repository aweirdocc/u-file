// 浏览状态：当前源/路径、游标分页栈、源列表、搜索模式、同步状态。
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
    searchHits: [] as SearchHit[],
    searchLoading: false,
    /** 勾选的路径（仅 admin + write 能力源） */
    selected: [] as string[],
    /** 弹窗控制 */
    sourceDialogMode: null as null | { edit?: string }, // null=关闭；{edit}=编辑；{}=新增
    exportDialogVisible: false,
    importDialogVisible: false,
    previewEntry: null as Entry | null,
  }),
  getters: {
    currentSource: (s) => s.sources.find(x => x.id === s.source) || null,
    canWrite(): boolean {
      const auth = useAuthStore()
      return auth.isAdmin && !!this.currentSource?.capabilities.write
    },
    canPresign(): boolean {
      return !!this.currentSource?.capabilities.presign_url
    },
    hasCursor(): boolean {
      return !!this.currentSource?.capabilities.list_cursor
    },
    breadcrumbs(): Crumb[] {
      const crumbs: Crumb[] = [{ name: this.source, path: '' }]
      let acc = ''
      for (const seg of this.path.split('/').filter(Boolean)) {
        acc += (acc ? '/' : '') + seg
        crumbs.push({ name: seg, path: acc })
      }
      return crumbs
    },
    pageNum(): number { return this.pageStack.length },
    isSearching(): boolean { return this.searchQuery !== '' },
  },
  actions: {
    /** 从 URL 恢复 source/path */
    fromRoute(source: string, path: string) {
      this.source = source
      this.path = path
      this.searchQuery = ''
      this.pageStack = ['']
      this.curNextCursor = ''
      this.selected = []
    },
    /** 导航（替代旧 nav()）；URL 由组件层用 router 同步 */
    async navigate(source: string, path: string) {
      this.fromRoute(source, path)
      await this.loadSources()
      await this.loadEntries()
    },
    async refresh() {
      await this.loadSources()
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
    async runSearch(q: string) {
      this.searchQuery = q
      this.searchHits = []
      if (!q) return
      this.searchLoading = true
      try {
        const r = await api.search(q, 100)
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
