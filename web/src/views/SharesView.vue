<script setup lang="ts">
// 分享管理页：全部分享链接的维护（列表/复制/撤销）。admin 专属，路由守卫拦截。
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Back, Refresh } from '@element-plus/icons-vue'
import { listShares, deleteShare } from '../api'
import { fmtSize } from '../utils/format'
import type { ShareInfo } from '../api/types'

const router = useRouter()
const loading = ref(false)
const items = ref<ShareInfo[]>([])

async function load() {
  loading.value = true
  try {
    items.value = await listShares()
  } catch (e: any) {
    ElMessage.error('加载分享列表失败：' + e.message)
  } finally {
    loading.value = false
  }
}

onMounted(load)

function shareLink(s: ShareInfo) {
  return `${location.origin}/s/${s.token}`
}

function stateOf(s: ShareInfo): { text: string; type: 'success' | 'warning' | 'danger' | 'info' } {
  if (s.revoked) return { text: '已撤销', type: 'info' }
  if (new Date(s.expires_at) < new Date()) return { text: '已过期', type: 'info' }
  if (s.max_downloads && s.downloads >= s.max_downloads) return { text: '次数用尽', type: 'danger' }
  return { text: '生效中', type: 'success' }
}

async function copyLink(s: ShareInfo) {
  try {
    await navigator.clipboard.writeText(shareLink(s))
    ElMessage.success('链接已复制')
  } catch {
    ElMessage.warning('复制失败，请手动复制：' + shareLink(s))
  }
}

async function onRevoke(s: ShareInfo) {
  try {
    await ElMessageBox.confirm(`撤销后链接立即失效，且不可恢复。确定撤销「${s.name}」的分享？`, '撤销分享', {
      confirmButtonText: '撤销',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await deleteShare(s.id)
    ElMessage.success('已撤销')
    await load()
  } catch (e: any) {
    ElMessage.error('撤销失败：' + e.message)
  }
}
</script>

<template>
  <main class="page-main">
    <div class="page-head">
      <el-button :icon="Back" link @click="router.push('/')">返回浏览</el-button>
      <h2 class="title">分享管理</h2>
      <span class="muted small">共 {{ items.length }} 条</span>
      <div class="spacer" />
      <el-button :icon="Refresh" circle @click="load" title="刷新" />
    </div>

    <div class="card-panel">
      <el-table :data="items" v-loading="loading" empty-text="暂无分享 — 在文件行点「分享」创建">
        <el-table-column label="文件" min-width="220">
          <template #default="{ row }">
            <span class="icon">{{ row.is_dir ? '📁' : '📄' }}</span>
            <b>{{ row.name }}</b>
            <div class="muted small">{{ row.source_id }}:{{ row.path }}</div>
          </template>
        </el-table-column>
        <el-table-column label="大小" width="90">
          <template #default="{ row }">
            <span class="muted">{{ row.is_dir ? '—' : fmtSize(row.size) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tag size="small" :type="stateOf(row).type">{{ stateOf(row).text }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="有效期至" width="160">
          <template #default="{ row }">
            <span class="small">{{ new Date(row.expires_at).toLocaleString() }}</span>
          </template>
        </el-table-column>
        <el-table-column label="下载/限次" width="110">
          <template #default="{ row }">
            <span class="small">{{ row.downloads }}<template v-if="row.max_downloads"> / {{ row.max_downloads }}</template><template v-else> / 不限</template></span>
          </template>
        </el-table-column>
        <el-table-column label="提取码" width="70">
          <template #default="{ row }">
            <span class="small">{{ row.has_password ? '有' : '无' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="创建者" width="90">
          <template #default="{ row }">
            <span class="small">{{ row.created_by }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="160" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="copyLink(row)">复制链接</el-button>
            <el-button link type="danger" @click="onRevoke(row)">撤销</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>
  </main>
  <footer class="page-footer">u-file · 分享链接受有效期/提取码/次数约束</footer>
</template>

<style scoped>
.page-head { display: flex; align-items: center; gap: 12px; margin-bottom: 14px; }
.page-head .title { margin: 0; font-size: 18px; }
.spacer { flex: 1; }
.icon { margin-right: 6px; }
.muted { color: var(--el-text-color-secondary); }
.small { font-size: 12px; }
</style>
