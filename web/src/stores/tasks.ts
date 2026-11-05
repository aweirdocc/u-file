// M4.1 传输任务中心全局状态：融合服务端跨源流转任务（SSE 实时流 + 活跃轮询双保险）与浏览器端上传任务进度。
import { defineStore } from 'pinia'
import * as api from '../api'
import { getToken } from '../api/client'
import type { TaskItem, TransferTaskInput } from '../api/types'
import { useBrowseStore } from './browse'

const uploadAbortControllers = new Map<string, AbortController>()

/** 将后端 Task JSON 结构规整为前端统一 TaskItem 视图字段 */
function normalizeServerTask(raw: any): TaskItem {
  const createdAt =
    typeof raw.created_at === 'number'
      ? new Date(raw.created_at * 1000).toISOString()
      : raw.created_at || new Date().toISOString()
  const updatedAt =
    typeof raw.updated_at === 'number'
      ? new Date(raw.updated_at * 1000).toISOString()
      : raw.updated_at

  return {
    id: raw.id,
    type: raw.type,
    status: raw.status,
    user_id: raw.user_id,
    source_id: raw.source_id,
    target_source_id: raw.target_source_id,
    paths: raw.paths || [],
    dst_dir: raw.dst_dir || '',
    total_bytes: raw.total_bytes ?? 0,
    done_bytes: raw.done_bytes ?? raw.transferred_bytes ?? 0,
    total_files: raw.total_files ?? raw.total_items ?? 0,
    done_files: raw.done_files ?? raw.done_items ?? 0,
    speed_bps: raw.speed_bps ?? 0,
    current_file: raw.current_file ?? raw.current_item ?? '',
    error: raw.error ?? raw.error_msg ?? '',
    retries: raw.retries ?? raw.retry_count ?? 0,
    created_at: createdAt,
    updated_at: updatedAt,
    is_local_upload: false,
  }
}

export const useTaskStore = defineStore('tasks', {
  state: () => ({
    serverTasks: [] as TaskItem[],
    localUploads: [] as TaskItem[],
    drawerOpen: false,
    sseConnected: false,
    _es: null as EventSource | null,
    _pollTimer: null as ReturnType<typeof setInterval> | null,
    _prevRunningIds: new Set<string>(),
  }),
  getters: {
    allTasks(state): TaskItem[] {
      return [...state.localUploads, ...state.serverTasks]
    },
    activeTasks(): TaskItem[] {
      return this.allTasks.filter(t => t.status === 'running' || t.status === 'pending' || t.status === 'retrying')
    },
    activeCount(): number {
      return this.activeTasks.length
    },
    totalSpeedBps(): number {
      return this.activeTasks.reduce((sum, t) => sum + (t.speed_bps || 0), 0)
    },
  },
  actions: {
    /** 不可变方式更新本地上传任务，确保 Pinia getter 与 Vue 视图 100% 立即触发重渲染 */
    patchLocalUpload(id: string, patch: Partial<TaskItem>) {
      this.localUploads = this.localUploads.map(t =>
        t.id === id ? { ...t, ...patch } : t,
      )
    },

    async fetchTasks() {
      try {
        const list = await api.listTasks()
        this.applyServerTasks(list || [])
      } catch {
        /* 未登录或无权限时静默忽略 */
      }
    },

    applyServerTasks(rawList: any[]) {
      const list = (rawList || []).map(normalizeServerTask)
      // 检测是否有任务刚刚从运行态变为成功，自动刷新当前浏览目录
      let justCompleted = false
      const curRunning = new Set<string>()
      for (const t of list) {
        if (t.status === 'running' || t.status === 'pending' || t.status === 'retrying') {
          curRunning.add(t.id)
        } else if (t.status === 'success' && this._prevRunningIds.has(t.id)) {
          justCompleted = true
        }
      }
      this._prevRunningIds = curRunning
      this.serverTasks = list

      if (justCompleted) {
        const browse = useBrowseStore()
        if (browse.source) {
          browse.afterWrite().catch(() => {})
        }
      }
    },

    startRealtime() {
      if (this._es) return
      this.fetchTasks()

      // 保底定时器：当存在运行中服务端任务或 SSE 断开时自动同步状态
      if (!this._pollTimer) {
        this._pollTimer = setInterval(() => {
          const hasActiveServerTask = this.serverTasks.some(
            t => t.status === 'running' || t.status === 'pending' || t.status === 'retrying',
          )
          if (!this.sseConnected || hasActiveServerTask) {
            this.fetchTasks()
          }
        }, 1500)
      }

      try {
        const tok = getToken()
        const url = tok ? `/api/v1/tasks/events?token=${encodeURIComponent(tok)}` : '/api/v1/tasks/events'
        const es = new EventSource(url)
        this._es = es
        es.onopen = () => {
          this.sseConnected = true
        }
        es.onmessage = (ev) => {
          try {
            const list = JSON.parse(ev.data)
            if (Array.isArray(list)) {
              this.applyServerTasks(list)
            }
          } catch {
            /* ignore malformed frame */
          }
        }
        es.onerror = () => {
          this.sseConnected = false
          es.close()
          this._es = null
        }
      } catch {
        this.sseConnected = false
      }
    },

    stopRealtime() {
      if (this._es) {
        this._es.close()
        this._es = null
      }
      if (this._pollTimer) {
        clearInterval(this._pollTimer)
        this._pollTimer = null
      }
      this.sseConnected = false
    },

    async submitTransfer(input: TransferTaskInput) {
      const tk = await api.submitTransferTask(input)
      this._prevRunningIds.add(tk.id)
      await this.fetchTasks()
      this.drawerOpen = true
      return tk
    },

    /** 浏览器多文件上传并纳入任务中心实时进度与速率展示 */
    async uploadWithProgress(source: string, path: string, files: File[]) {
      const totalBytes = files.reduce((sum, f) => sum + f.size, 0)
      const id = 'up_' + Date.now().toString(36) + '_' + Math.random().toString(36).slice(2, 6)
      const item: TaskItem = {
        id,
        type: 'upload',
        status: 'running',
        source_id: source,
        target_source_id: source,
        paths: files.map(f => f.name),
        dst_dir: path || '/',
        total_bytes: totalBytes,
        done_bytes: 0,
        total_files: files.length,
        done_files: 0,
        speed_bps: 0,
        current_file: files[0]?.name || '',
        created_at: new Date().toISOString(),
        is_local_upload: true,
      }
      this.localUploads = [item, ...this.localUploads]
      this.drawerOpen = true

      const ac = new AbortController()
      uploadAbortControllers.set(id, ac)

      let lastTime = performance.now()
      let lastLoaded = 0

      try {
        const res = await api.uploadFilesWithProgress(
          source,
          path,
          files,
          (loaded, total) => {
            const now = performance.now()
            const dt = (now - lastTime) / 1000
            const patch: Partial<TaskItem> = {
              done_bytes: loaded,
            }
            if (total > 0) {
              patch.total_bytes = total
            }
            if (dt >= 0.15) {
              patch.speed_bps = Math.max(0, Math.round((loaded - lastLoaded) / dt))
              lastTime = now
              lastLoaded = loaded
            }
            this.patchLocalUpload(id, patch)
          },
          ac.signal,
        )

        const failedItems = (res.items || []).filter(i => !i.ok)
        const isFailed = (res.failed ?? 0) > 0 || failedItems.length > 0
        this.patchLocalUpload(id, {
          status: isFailed ? 'failed' : 'success',
          done_bytes: totalBytes,
          total_bytes: totalBytes,
          done_files: res.uploaded ?? files.length,
          speed_bps: 0,
          current_file: '',
          error: isFailed ? failedItems.map(i => `${i.file}: ${i.error}`).join('；') : '',
          updated_at: new Date().toISOString(),
        })
        return res
      } catch (e: any) {
        const canceled = ac.signal.aborted
        this.patchLocalUpload(id, {
          status: canceled ? 'canceled' : 'failed',
          speed_bps: 0,
          current_file: '',
          error: canceled ? '用户已取消上传' : (e.message || String(e)),
          updated_at: new Date().toISOString(),
        })
        throw e
      } finally {
        uploadAbortControllers.delete(id)
      }
    },

    async pause(t: TaskItem) {
      if (t.is_local_upload) return
      await api.pauseTask(t.id)
      await this.fetchTasks()
    },

    async resume(t: TaskItem) {
      if (t.is_local_upload) return
      await api.resumeTask(t.id)
      await this.fetchTasks()
    },

    async cancel(t: TaskItem) {
      if (t.is_local_upload) {
        uploadAbortControllers.get(t.id)?.abort()
        return
      }
      await api.cancelTask(t.id)
      await this.fetchTasks()
    },

    async retry(t: TaskItem) {
      if (t.is_local_upload) return
      this._prevRunningIds.add(t.id)
      await api.retryTask(t.id)
      await this.fetchTasks()
    },

    async remove(t: TaskItem) {
      if (t.is_local_upload) {
        this.localUploads = this.localUploads.filter(x => x.id !== t.id)
        return
      }
      await api.deleteTask(t.id)
      await this.fetchTasks()
    },

    async clearFinished() {
      this.localUploads = this.localUploads.filter(
        t => t.status === 'running' || t.status === 'pending' || t.status === 'retrying' || t.status === 'paused',
      )
      await api.clearFinishedTasks()
      await this.fetchTasks()
    },
  },
})
