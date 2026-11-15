<script setup lang="ts">
// 操作审计日志独立页（/audit，仅管理员可见）：
// 展示后端 data/audit.log 的最近记录（倒序），支持按动作类别、操作者、关键字及条数上限筛选。
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { Back, Refresh, Search } from '@element-plus/icons-vue'
import { listAuditLogs } from '../api'
import { fmtTime } from '../utils/format'
import type { AuditEntry } from '../api/types'

const router = useRouter()

const loading = ref(false)
const entries = ref<AuditEntry[]>([])
const actionFilter = ref('')
const keyword = ref('')
const limit = ref(200)

const actionOptions = [
  { label: '全部操作', value: '' },
  { label: '登录与用户 (auth.)', value: 'auth.' },
  { label: '文件操作 (file.)', value: 'file.' },
  { label: '权限拒绝 (acl.denied)', value: 'acl.denied' },
  { label: '分享外链 (share.)', value: 'share.' },
  { label: '源配置 (source.)', value: 'source.' },
  { label: 'API Token (apitoken.)', value: 'apitoken.' },
  { label: 'Webhook (webhook.)', value: 'webhook.' },
  { label: '虚拟挂载 (mount.)', value: 'mount.' },
  { label: '传输任务 (task.)', value: 'task.' },
  { label: '全文索引 (fulltext.)', value: 'fulltext.' },
]

async function loadLogs() {
  loading.value = true
  try {
    const res = await listAuditLogs({
      limit: limit.value,
      action: actionFilter.value || undefined,
      q: keyword.value.trim() || undefined,
    })
    entries.value = res.items || []
  } catch (e: any) {
    ElMessage.error('加载审计日志失败：' + e.message)
  } finally {
    loading.value = false
  }
}

onMounted(loadLogs)

const stats = computed(() => {
  let denied = 0
  let agent = 0
  for (const e of entries.value) {
    if (e.action === 'acl.denied') denied++
    if (e.agent_actor) agent++
  }
  return { total: entries.value.length, denied, agent }
})

function actionTagType(action: string): 'danger' | 'warning' | 'success' | 'info' | 'primary' {
  if (action === 'acl.denied') return 'danger'
  if (action.endsWith('.delete') || action.endsWith('.revoke')) return 'warning'
  if (action.startsWith('auth.')) return 'info'
  if (action.startsWith('file.')) return 'primary'
  return 'success'
}

function formatDetail(detail: unknown): string {
  if (detail === undefined || detail === null) return '-'
  if (typeof detail === 'string') return detail
  try {
    return JSON.stringify(detail)
  } catch {
    return String(detail)
  }
}
</script>

<template>
  <main class="page-main">
    <div class="page-head">
      <el-button :icon="Back" link @click="router.push('/')">返回浏览</el-button>
      <h2 class="title">操作审计日志</h2>
      <div class="stat-pills">
        <el-tag size="small" type="info" effect="plain">当前显示 {{ stats.total }} 条</el-tag>
        <el-tag v-if="stats.denied" size="small" type="danger" effect="plain">权限拒绝 {{ stats.denied }}</el-tag>
        <el-tag v-if="stats.agent" size="small" type="warning" effect="plain">Agent 操作 {{ stats.agent }}</el-tag>
      </div>
      <div class="spacer" />
      <el-select
        v-model="actionFilter"
        style="width: 190px"
        placeholder="按动作类别筛选"
        @change="loadLogs"
      >
        <el-option
          v-for="opt in actionOptions"
          :key="opt.value"
          :label="opt.label"
          :value="opt.value"
        />
      </el-select>
      <el-input
        v-model="keyword"
        placeholder="搜索用户 / IP / TraceID / 详情（回车）"
        :prefix-icon="Search"
        clearable
        style="width: 260px"
        @keydown.enter="loadLogs"
        @clear="loadLogs"
      />
      <el-select v-model="limit" style="width: 110px" @change="loadLogs">
        <el-option label="100 条" :value="100" />
        <el-option label="200 条" :value="200" />
        <el-option label="500 条" :value="500" />
        <el-option label="1000 条" :value="1000" />
      </el-select>
      <el-button :icon="Refresh" :loading="loading" @click="loadLogs">刷新</el-button>
    </div>

    <el-card shadow="never" class="audit-card">
      <el-table v-loading="loading" :data="entries" stripe empty-text="暂无匹配的审计日志">
        <el-table-column label="时间" width="180">
          <template #default="{ row }">
            <span class="mono">{{ fmtTime(row.time) }}</span>
          </template>
        </el-table-column>

        <el-table-column label="操作者" width="170">
          <template #default="{ row }">
            <div class="actor-cell">
              <span class="actor-name">{{ row.user || '-' }}</span>
              <el-tag v-if="row.agent_actor" size="small" type="warning" effect="plain">
                Agent: {{ row.agent_actor }}
              </el-tag>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="动作" width="170">
          <template #default="{ row }">
            <el-tag size="small" :type="actionTagType(row.action)" effect="light">
              {{ row.action }}
            </el-tag>
          </template>
        </el-table-column>

        <el-table-column label="来源 IP" width="145">
          <template #default="{ row }">
            <span class="mono">{{ row.ip || '-' }}</span>
          </template>
        </el-table-column>

        <el-table-column label="Trace ID" width="150" show-overflow-tooltip>
          <template #default="{ row }">
            <span class="mono muted">{{ row.trace_id || '-' }}</span>
          </template>
        </el-table-column>

        <el-table-column label="详情" min-width="260" show-overflow-tooltip>
          <template #default="{ row }">
            <span class="mono detail-text">{{ formatDetail(row.detail) }}</span>
          </template>
        </el-table-column>
      </el-table>
    </el-card>
  </main>
</template>

<style scoped>
.page-main {
  max-width: 1400px;
  margin: 0 auto;
  padding: 20px 24px 40px;
}

.page-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 16px;
}

.title {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
}

.stat-pills {
  display: flex;
  gap: 6px;
  align-items: center;
}

.spacer {
  flex: 1;
}

.audit-card {
  border-radius: 10px;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
}

.muted {
  color: var(--el-text-color-secondary);
}

.actor-cell {
  display: flex;
  flex-direction: column;
  gap: 4px;
  align-items: flex-start;
}

.actor-name {
  font-weight: 500;
}

.detail-text {
  color: var(--el-text-color-regular);
}
</style>
