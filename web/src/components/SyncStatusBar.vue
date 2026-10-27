<script setup lang="ts">
// 同步状态条：running 时轮询（单实例去重，组件卸载/源切换即停止——
// 修掉旧 UI 每次 render 叠加轮询链的缺陷）。
import { ref, watch, onBeforeUnmount } from 'vue'
import { useBrowseStore } from '../stores/browse'
import { getSyncStatus } from '../api'
import type { SyncStatus } from '../api/types'
import { fmtTime } from '../utils/format'

const browse = useBrowseStore()
const status = ref<SyncStatus | null>(null)
let timer: ReturnType<typeof setTimeout> | null = null
let seq = 0 // 请求序号：源切换后旧响应作废

async function poll() {
  if (!browse.source) { status.value = null; return }
  const my = ++seq
  try {
    const st = await getSyncStatus(browse.source)
    if (my !== seq) return // 源已切换
    status.value = st
    if (st.running) {
      timer = setTimeout(poll, 2000)
    }
  } catch {
    status.value = null // 状态获取失败不打扰浏览
  }
}

watch(() => browse.source, () => {
  if (timer) { clearTimeout(timer); timer = null }
  status.value = null
  if (browse.source) poll()
}, { immediate: true })

onBeforeUnmount(() => {
  if (timer) clearTimeout(timer)
})
</script>

<template>
  <div v-if="status" class="sync-bar">
    <template v-if="status.running">
      <el-icon class="rotating">⟳</el-icon>
      后台同步中：{{ status.current_dir || '…' }}（已刷 {{ status.synced_dirs }} 个目录）
    </template>
    <template v-else-if="status.last_error">
      <span class="bad">⚠ 同步失败：{{ status.last_error }}</span>
    </template>
    <template v-else-if="status.last_success">
      ✓ 上次同步成功：{{ fmtTime(status.last_success) }}（{{ status.dirs }} 个目录）
    </template>
  </div>
</template>

<style scoped>
.sync-bar {
  font-size: 12px;
  color: var(--el-text-color-secondary);
  margin: -4px 0 10px;
  display: flex;
  align-items: center;
  gap: 4px;
}
.bad { color: var(--el-color-danger); }
.rotating { display: inline-block; animation: spin 1.2s linear infinite; color: var(--el-color-primary); }
@keyframes spin { to { transform: rotate(360deg); } }
</style>
