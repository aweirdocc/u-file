<script setup lang="ts">
// M4.1 传输任务中心右下角悬浮胶囊与任务面板（支持实时进度、速率、暂停/继续/重试/取消/清理）。
import { computed, watch, onBeforeUnmount } from 'vue'
import { ElMessage } from 'element-plus'
import {
  VideoPause, VideoPlay, RefreshRight, Close, Delete, Sort,
} from '@element-plus/icons-vue'
import { useTaskStore } from '../stores/tasks'
import { useAuthStore } from '../stores/auth'
import { fmtSize, fmtTime } from '../utils/format'
import type { TaskItem, TaskStatus } from '../api/types'

const tasks = useTaskStore()
const auth = useAuthStore()

watch(
  () => auth.isLoggedIn,
  (logged) => {
    if (logged) {
      tasks.startRealtime()
    } else {
      tasks.stopRealtime()
    }
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  tasks.stopRealtime()
})

const hasAnyTasks = computed(() => tasks.allTasks.length > 0)

function percentOf(t: TaskItem): number {
  if (t.status === 'success') return 100
  if (t.total_bytes > 0) {
    return Math.min(99, Math.max(0, Math.round((t.done_bytes / t.total_bytes) * 100)))
  }
  if (t.total_files > 0) {
    return Math.min(99, Math.max(0, Math.round((t.done_files / t.total_files) * 100)))
  }
  return t.status === 'running' ? 15 : 0
}

function typeLabel(t: TaskItem): string {
  switch (t.type) {
    case 'copy': return '跨源复制'
    case 'move': return '跨源移动'
    case 'upload': return '文件上传'
    default: return t.type
  }
}

function statusMeta(st: TaskStatus): { label: string; type: 'primary' | 'success' | 'warning' | 'danger' | 'info' } {
  switch (st) {
    case 'running': return { label: '传输中', type: 'primary' }
    case 'pending': return { label: '排队中', type: 'info' }
    case 'retrying': return { label: '重试中', type: 'warning' }
    case 'paused': return { label: '已暂停', type: 'warning' }
    case 'success': return { label: '已完成', type: 'success' }
    case 'failed': return { label: '失败', type: 'danger' }
    case 'canceled': return { label: '已取消', type: 'info' }
    default: return { label: st, type: 'info' }
  }
}

function taskSummary(t: TaskItem): string {
  if (!t.paths || !t.paths.length) return t.dst_dir || '/'
  if (t.paths.length === 1) return t.paths[0]
  return `${t.paths[0]} 等 ${t.paths.length} 项`
}

async function doAction(fn: () => Promise<void>) {
  try {
    await fn()
  } catch (e: any) {
    ElMessage.error(e.message || '任务操作失败')
  }
}
</script>

<template>
  <!-- 右下角悬浮球 -->
  <div class="task-float-pill" :class="{ active: tasks.activeCount > 0 }" @click="tasks.drawerOpen = !tasks.drawerOpen">
    <el-icon :class="{ spinning: tasks.activeCount > 0 }"><Sort /></el-icon>
    <span class="pill-title">传输任务</span>
    <el-badge v-if="tasks.activeCount > 0" :value="tasks.activeCount" type="primary" />
    <span v-else-if="hasAnyTasks" class="pill-count">({{ tasks.allTasks.length }})</span>
    <span v-if="tasks.totalSpeedBps > 0" class="pill-speed">
      {{ fmtSize(tasks.totalSpeedBps) }}/s
    </span>
  </div>

  <!-- 任务中心抽屉 -->
  <el-drawer
    v-model="tasks.drawerOpen"
    title="传输任务中心（跨源复制 / 移动 / 上传）"
    direction="rtl"
    size="520px"
  >
    <template #header>
      <div class="drawer-hdr">
        <div class="hdr-title">
          <b>⚡ 传输任务中心</b>
          <el-tag size="small" :type="tasks.sseConnected ? 'success' : 'info'" effect="plain">
            {{ tasks.sseConnected ? 'SSE 实时推送' : '实时同步' }}
          </el-tag>
        </div>
        <div class="hdr-actions">
          <el-button size="small" @click="doAction(() => tasks.fetchTasks())">刷新</el-button>
          <el-button size="small" type="info" plain @click="doAction(() => tasks.clearFinished())">
            清理已完成
          </el-button>
        </div>
      </div>
    </template>

    <div v-if="!tasks.allTasks.length" class="empty-tasks">
      暂无传输任务。可在文件浏览页选择文件执行「上传」「复制到…」或「移动到…」。
    </div>

    <div v-else class="task-list">
      <div v-for="t in tasks.allTasks" :key="t.id" class="task-card" :class="t.status">
        <div class="task-top">
          <el-tag size="small" effect="plain">{{ typeLabel(t) }}</el-tag>
          <span class="task-name" :title="t.paths?.join(', ')">{{ taskSummary(t) }}</span>
          <el-tag size="small" :type="statusMeta(t.status).type">
            {{ statusMeta(t.status).label }}
          </el-tag>
        </div>

        <div class="task-route">
          <template v-if="t.type === 'upload'">
            本地浏览器 → <b>{{ t.source_id }}</b>:<code>/{{ t.dst_dir.replace(/^\/+/, '') }}</code>
          </template>
          <template v-else>
            <b>{{ t.source_id }}</b> → <b>{{ t.target_source_id }}</b>:<code>/{{ (t.dst_dir || '').replace(/^\/+/, '') }}</code>
          </template>
        </div>

        <el-progress
          :percentage="percentOf(t)"
          :status="t.status === 'success' ? 'success' : t.status === 'failed' ? 'exception' : undefined"
          :stroke-width="8"
        />

        <div class="task-meta">
          <span>
            {{ fmtSize(t.done_bytes) }} / {{ t.total_bytes > 0 ? fmtSize(t.total_bytes) : '计算中' }}
            <template v-if="t.total_files > 1">
              （{{ t.done_files }}/{{ t.total_files }} 个文件）
            </template>
          </span>
          <span v-if="t.status === 'running' && t.speed_bps > 0" class="speed-txt">
            {{ fmtSize(t.speed_bps) }}/s
          </span>
          <span v-else class="time-txt">{{ fmtTime(t.created_at) }}</span>
        </div>

        <div v-if="t.current_file && t.status === 'running'" class="cur-file">
          正在处理：{{ t.current_file }}
        </div>

        <div v-if="t.error" class="err-box">
          {{ t.error }}
        </div>

        <div class="task-ops">
          <el-button
            v-if="!t.is_local_upload && (t.status === 'running' || t.status === 'pending')"
            size="small"
            :icon="VideoPause"
            @click="doAction(() => tasks.pause(t))"
          >
            暂停
          </el-button>
          <el-button
            v-if="!t.is_local_upload && t.status === 'paused'"
            size="small"
            type="primary"
            :icon="VideoPlay"
            @click="doAction(() => tasks.resume(t))"
          >
            继续
          </el-button>
          <el-button
            v-if="!t.is_local_upload && (t.status === 'failed' || t.status === 'canceled')"
            size="small"
            type="primary"
            plain
            :icon="RefreshRight"
            @click="doAction(() => tasks.retry(t))"
          >
            断点重试
          </el-button>
          <el-button
            v-if="t.status === 'running' || t.status === 'pending' || t.status === 'paused' || t.status === 'retrying'"
            size="small"
            type="warning"
            plain
            :icon="Close"
            @click="doAction(() => tasks.cancel(t))"
          >
            取消
          </el-button>
          <el-button
            v-if="t.status === 'success' || t.status === 'failed' || t.status === 'canceled'"
            size="small"
            type="danger"
            link
            :icon="Delete"
            @click="doAction(() => tasks.remove(t))"
          >
            移除记录
          </el-button>
        </div>
      </div>
    </div>
  </el-drawer>
</template>

<style scoped>
.task-float-pill {
  position: fixed;
  right: 24px;
  bottom: 24px;
  z-index: 1900;
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 10px 16px;
  border-radius: 999px;
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color);
  box-shadow: 0 8px 24px rgba(15, 23, 42, 0.14);
  cursor: pointer;
  user-select: none;
  font-size: 13px;
  transition: transform 0.15s, box-shadow 0.15s, border-color 0.15s;
}
.task-float-pill:hover {
  transform: translateY(-2px);
  border-color: var(--el-color-primary);
}
.task-float-pill.active {
  border-color: var(--el-color-primary);
  background: var(--el-color-primary-light-9);
}
.pill-title { font-weight: 600; }
.pill-count { color: var(--el-text-color-secondary); font-size: 12px; }
.pill-speed {
  color: var(--el-color-primary);
  font-weight: 600;
  font-size: 12px;
}
.spinning {
  animation: pulse 1.2s ease-in-out infinite;
  color: var(--el-color-primary);
}
@keyframes pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.4; }
}

.drawer-hdr {
  display: flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
  padding-right: 12px;
}
.hdr-title { display: flex; align-items: center; gap: 8px; font-size: 15px; }
.hdr-actions { display: flex; gap: 6px; }

.empty-tasks {
  text-align: center;
  color: var(--el-text-color-secondary);
  padding: 64px 20px;
  font-size: 13px;
}
.task-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.task-card {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  padding: 12px 14px;
  background: var(--el-bg-color);
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.task-top {
  display: flex;
  align-items: center;
  gap: 8px;
}
.task-name {
  flex: 1;
  font-weight: 600;
  font-size: 13.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.task-route {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.task-route code {
  background: var(--el-fill-color-light);
  padding: 1px 5px;
  border-radius: 4px;
}
.task-meta {
  display: flex;
  justify-content: space-between;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.speed-txt {
  color: var(--el-color-primary);
  font-weight: 600;
}
.cur-file {
  font-size: 11.5px;
  color: var(--el-text-color-secondary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.err-box {
  font-size: 12px;
  color: var(--el-color-danger);
  background: var(--el-color-danger-light-9);
  padding: 6px 8px;
  border-radius: 4px;
}
.task-ops {
  display: flex;
  justify-content: flex-end;
  gap: 6px;
}
</style>
