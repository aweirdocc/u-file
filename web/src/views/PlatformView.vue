<script setup lang="ts">
// M4 开放生态与架构深化管理中心（/platform）：
//   Tab 1: 虚拟挂载树 (VFS Mounts — M4.3)
//   Tab 2: 开放 API Token (细粒度 Scope 鉴权 — M4.2)
//   Tab 3: 事件 Webhook (异步签名投递 + 指数退避重试 — M4.2)
//   Tab 4: 全文检索引擎 (多格式文档正文抽取 + 嵌入式倒排索引 — M4.3)
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  Back, Refresh, Plus, Edit, Delete, Connection, Key, Promotion, Document, CopyDocument, FolderOpened,
} from '@element-plus/icons-vue'
import {
  listSources,
  listMounts, createMount, updateMount, deleteMount,
  listAPITokens, createAPIToken, revokeAPIToken, deleteAPIToken,
  listWebhooks, createWebhook, updateWebhook, deleteWebhook, testWebhook,
  getFullTextStatus, rebuildFullText,
} from '../api'
import { useBrowseStore } from '../stores/browse'
import { fmtTime } from '../utils/format'
import type {
  Source, MountItem, APITokenInfo, TokenScope, WebhookEndpoint, FullTextStatus,
} from '../api/types'

const router = useRouter()
const browse = useBrowseStore()

const activeTab = ref<'mounts' | 'tokens' | 'webhooks' | 'fulltext'>('mounts')
const loading = ref(false)

const sources = ref<Source[]>([])
const mounts = ref<MountItem[]>([])
const tokens = ref<APITokenInfo[]>([])
const webhooks = ref<WebhookEndpoint[]>([])
const ftStatus = ref<FullTextStatus | null>(null)

const physicalSources = computed(() => sources.value.filter(s => s.id !== '@vfs'))

async function loadAll() {
  loading.value = true
  try {
    const [sList, mList, tList, wList, ftSt] = await Promise.all([
      listSources(),
      listMounts(),
      listAPITokens(),
      listWebhooks(),
      getFullTextStatus(),
    ])
    sources.value = sList || []
    mounts.value = mList || []
    tokens.value = tList || []
    webhooks.value = wList || []
    ftStatus.value = ftSt
  } catch (e: any) {
    ElMessage.error('加载平台配置失败：' + e.message)
  } finally {
    loading.value = false
  }
}

onMounted(loadAll)

// ==================== 1. 虚拟挂载点 (VFS) ====================

const mountDialogVisible = ref(false)
const editingMountId = ref<string | null>(null)
const mountSaving = ref(false)
const mountForm = reactive({
  mount_path: '',
  source_id: '',
  source_path: '',
  readonly: false,
  remark: '',
})

function openCreateMount() {
  editingMountId.value = null
  mountForm.mount_path = '/财务文档/合同'
  mountForm.source_id = physicalSources.value[0]?.id || ''
  mountForm.source_path = ''
  mountForm.readonly = false
  mountForm.remark = ''
  mountDialogVisible.value = true
}

function openEditMount(m: MountItem) {
  editingMountId.value = m.id
  mountForm.mount_path = m.mount_path
  mountForm.source_id = m.source_id
  mountForm.source_path = m.source_path
  mountForm.readonly = m.readonly
  mountForm.remark = m.remark || ''
  mountDialogVisible.value = true
}

async function saveMount() {
  if (!mountForm.mount_path.trim() || !mountForm.source_id) {
    ElMessage.warning('请填写虚拟挂载路径并选择物理文件源')
    return
  }
  mountSaving.value = true
  try {
    if (editingMountId.value) {
      await updateMount(editingMountId.value, { ...mountForm })
      ElMessage.success('已更新挂载点')
    } else {
      await createMount({ ...mountForm })
      ElMessage.success('已创建挂载点')
    }
    mountDialogVisible.value = false
    await loadAll()
    await browse.loadSources().catch(() => {})
  } catch (e: any) {
    ElMessage.error(e.message || '保存挂载点失败')
  } finally {
    mountSaving.value = false
  }
}

async function onDeleteMount(m: MountItem) {
  try {
    await ElMessageBox.confirm(`确定移除虚拟挂载点「${m.mount_path}」？（不会删除物理源中的文件）`, '移除挂载点', {
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await deleteMount(m.id)
    ElMessage.success('已移除')
    await loadAll()
    await browse.loadSources().catch(() => {})
  } catch (e: any) {
    ElMessage.error(e.message)
  }
}

function goVFS() {
  router.push('/@vfs')
}

// ==================== 2. 开放 API Token ====================

const scopeOptions: { value: TokenScope; label: string; desc: string }[] = [
  { value: 'files:read', label: 'files:read', desc: '浏览目录、下载、预览、搜索' },
  { value: 'files:write', label: 'files:write', desc: '上传文件、新建目录、重命名、跨源复制' },
  { value: 'files:delete', label: 'files:delete', desc: '删除文件/目录、跨源移动源清理' },
  { value: 'sources:read', label: 'sources:read', desc: '读取文件源列表与索引状态' },
  { value: 'shares:manage', label: 'shares:manage', desc: '创建与撤销公开分享外链' },
]

const tokenDialogVisible = ref(false)
const tokenSaving = ref(false)
const createdPlainToken = ref('')
const plainTokenDialogVisible = ref(false)

const tokenForm = reactive({
  name: '',
  scopes: ['files:read'] as TokenScope[],
  allSources: true,
  allowed_sources: [] as string[],
  ipText: '',
  ttl_hours: 720,
})

function openCreateToken() {
  tokenForm.name = ''
  tokenForm.scopes = ['files:read']
  tokenForm.allSources = true
  tokenForm.allowed_sources = []
  tokenForm.ipText = ''
  tokenForm.ttl_hours = 720
  tokenDialogVisible.value = true
}

async function saveToken() {
  if (!tokenForm.name.trim()) {
    ElMessage.warning('请输入 Token 名称')
    return
  }
  if (!tokenForm.scopes.length) {
    ElMessage.warning('请至少勾选一项权限作用域 (Scope)')
    return
  }
  const allowedSources = tokenForm.allSources ? ['*'] : tokenForm.allowed_sources
  if (!allowedSources.length) {
    ElMessage.warning('请选择允许访问的文件源')
    return
  }
  const ips = tokenForm.ipText
    .split(/[\n,]/)
    .map(s => s.trim())
    .filter(Boolean)

  tokenSaving.value = true
  try {
    const res = await createAPIToken({
      name: tokenForm.name.trim(),
      scopes: tokenForm.scopes,
      allowed_sources: allowedSources,
      ip_whitelist: ips,
      ttl_hours: tokenForm.ttl_hours,
    })
    tokenDialogVisible.value = false
    createdPlainToken.value = res.plain_token
    plainTokenDialogVisible.value = true
    await loadAll()
  } catch (e: any) {
    ElMessage.error(e.message || '创建 Token 失败')
  } finally {
    tokenSaving.value = false
  }
}

async function copyPlainToken() {
  try {
    await navigator.clipboard.writeText(createdPlainToken.value)
    ElMessage.success('API Token 已复制到剪贴板')
  } catch {
    ElMessage.warning('请手动选中复制')
  }
}

async function onRevokeToken(t: APITokenInfo) {
  try {
    await revokeAPIToken(t.id)
    ElMessage.success('已吊销该 Token')
    await loadAll()
  } catch (e: any) {
    ElMessage.error(e.message)
  }
}

async function onDeleteToken(t: APITokenInfo) {
  try {
    await ElMessageBox.confirm(`确定永久删除 Token「${t.name}」记录？`, '删除 Token', { type: 'warning' })
  } catch {
    return
  }
  try {
    await deleteAPIToken(t.id)
    ElMessage.success('已删除')
    await loadAll()
  } catch (e: any) {
    ElMessage.error(e.message)
  }
}

// ==================== 3. 事件 Webhook ====================

const webhookEventOptions = [
  { value: '*', label: '全部事件 (*)' },
  { value: 'file.uploaded', label: '文件上传 (file.uploaded)' },
  { value: 'file.deleted', label: '文件删除 (file.deleted)' },
  { value: 'file.renamed', label: '重命名 (file.renamed)' },
  { value: 'file.transferred', label: '跨源流转完成 (file.transferred)' },
  { value: 'share.created', label: '分享创建 (share.created)' },
  { value: 'share.deleted', label: '分享撤销 (share.deleted)' },
  { value: 'sync.completed', label: '索引同步完成 (sync.completed)' },
]

const webhookDialogVisible = ref(false)
const editingWebhookId = ref<string | null>(null)
const webhookSaving = ref(false)
const webhookForm = reactive({
  name: '',
  url: '',
  secret: '',
  events: ['*'] as string[],
  enabled: true,
})

function openCreateWebhook() {
  editingWebhookId.value = null
  webhookForm.name = ''
  webhookForm.url = 'https://'
  webhookForm.secret = ''
  webhookForm.events = ['*']
  webhookForm.enabled = true
  webhookDialogVisible.value = true
}

function openEditWebhook(w: WebhookEndpoint) {
  editingWebhookId.value = w.id
  webhookForm.name = w.name
  webhookForm.url = w.url
  webhookForm.secret = w.secret || ''
  webhookForm.events = [...(w.events || ['*'])]
  webhookForm.enabled = w.enabled
  webhookDialogVisible.value = true
}

async function saveWebhook() {
  if (!webhookForm.name.trim() || !webhookForm.url.trim()) {
    ElMessage.warning('请填写 Webhook 名称与回调 URL')
    return
  }
  webhookSaving.value = true
  try {
    if (editingWebhookId.value) {
      await updateWebhook(editingWebhookId.value, { ...webhookForm })
      ElMessage.success('已更新 Webhook')
    } else {
      await createWebhook({ ...webhookForm })
      ElMessage.success('已创建 Webhook')
    }
    webhookDialogVisible.value = false
    await loadAll()
  } catch (e: any) {
    ElMessage.error(e.message || '保存 Webhook 失败')
  } finally {
    webhookSaving.value = false
  }
}

async function onTestWebhook(w: WebhookEndpoint) {
  try {
    const res = await testWebhook(w.id)
    if (res.ok) {
      ElMessage.success(`Ping 投递成功（HTTP ${res.status_code}，耗时 ${res.latency_ms}ms）`)
    } else {
      ElMessage.error(`Ping 投递失败：${res.error || 'HTTP ' + res.status_code}`)
    }
    await loadAll()
  } catch (e: any) {
    ElMessage.error('测试失败：' + e.message)
  }
}

async function onDeleteWebhook(w: WebhookEndpoint) {
  try {
    await ElMessageBox.confirm(`确定删除 Webhook「${w.name}」？`, '删除 Webhook', { type: 'warning' })
  } catch {
    return
  }
  try {
    await deleteWebhook(w.id)
    ElMessage.success('已删除')
    await loadAll()
  } catch (e: any) {
    ElMessage.error(e.message)
  }
}

// ==================== 4. 全文检索引擎 ====================

const rebuildingFT = ref(false)

async function onRebuildFullText(srcId = '') {
  rebuildingFT.value = true
  try {
    const res = await rebuildFullText(srcId)
    ElMessage.success(res.message || '已触发后台全文索引构建')
    setTimeout(() => {
      getFullTextStatus().then(st => { ftStatus.value = st }).catch(() => {})
    }, 800)
  } catch (e: any) {
    ElMessage.error(e.message)
  } finally {
    rebuildingFT.value = false
  }
}
</script>

<template>
  <main class="page-main">
    <div class="page-head">
      <el-button :icon="Back" link @click="router.push('/')">返回浏览</el-button>
      <h2 class="title">M4 开放生态与架构深化中心</h2>
      <div class="spacer" />
      <el-button v-if="mounts.length > 0" type="success" plain :icon="FolderOpened" @click="goVFS">
        进入虚拟挂载视图 (@vfs)
      </el-button>
      <el-button :icon="Refresh" circle title="刷新" @click="loadAll" />
    </div>

    <div class="card-panel" v-loading="loading">
      <el-tabs v-model="activeTab">
        <!-- ==================== Tab 1: 虚拟挂载点 VFS ==================== -->
        <el-tab-pane name="mounts">
          <template #label>
            <span class="tab-lbl"><el-icon><Connection /></el-icon> 虚拟多挂载点树 (VFS)</span>
          </template>

          <div class="pane-toolbar">
            <div class="pane-desc">
              打破“源即顶层目录”的单层限制：支持将不同物理存储源的任意子目录挂载到统一虚拟目录树（<code>@vfs</code>）的不同层级，支持最长前缀路由与中间导航目录自动合成。
            </div>
            <el-button type="primary" :icon="Plus" @click="openCreateMount">新增挂载点</el-button>
          </div>

          <el-table :data="mounts" empty-text="尚未配置虚拟挂载点 — 点击右上角「新增挂载点」即可激活 @vfs 聚合视图">
            <el-table-column prop="mount_path" label="虚拟挂载路径" min-width="200">
              <template #default="{ row }">
                <code>{{ row.mount_path }}</code>
              </template>
            </el-table-column>
            <el-table-column label="映射物理源与子路径" min-width="240">
              <template #default="{ row }">
                <el-tag size="small" type="info">{{ row.source_id }}</el-tag>
                <span style="margin-left: 6px"><code>/{{ row.source_path }}</code></span>
              </template>
            </el-table-column>
            <el-table-column label="读写模式" width="120">
              <template #default="{ row }">
                <el-tag size="small" :type="row.readonly ? 'warning' : 'success'">
                  {{ row.readonly ? '只读挂载' : '读写挂载' }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="remark" label="备注" min-width="160" show-overflow-tooltip />
            <el-table-column label="创建时间" width="170">
              <template #default="{ row }">{{ fmtTime(row.created_at) }}</template>
            </el-table-column>
            <el-table-column label="操作" width="150">
              <template #default="{ row }">
                <el-button link type="primary" :icon="Edit" @click="openEditMount(row)">编辑</el-button>
                <el-button link type="danger" :icon="Delete" @click="onDeleteMount(row)">移除</el-button>
              </template>
            </el-table-column>
          </el-table>
        </el-tab-pane>

        <!-- ==================== Tab 2: 开放 API Token ==================== -->
        <el-tab-pane name="tokens">
          <template #label>
            <span class="tab-lbl"><el-icon><Key /></el-icon> 开放 API Token</span>
          </template>

          <div class="pane-toolbar">
            <div class="pane-desc">
              面向 CI/CD 流水线、备份脚本与第三方系统的长效编程令牌（<code>uft_</code> 前缀，服务端仅存 SHA-256 哈希，支持细粒度 Scope、指定源与 IP/CIDR 白名单）。
            </div>
            <el-button type="primary" :icon="Plus" @click="openCreateToken">签发 API Token</el-button>
          </div>

          <el-table :data="tokens" empty-text="暂无 API Token">
            <el-table-column prop="name" label="名称" width="160" />
            <el-table-column label="令牌前缀" width="140">
              <template #default="{ row }"><code>{{ row.token_prefix }}…</code></template>
            </el-table-column>
            <el-table-column label="权限作用域 (Scopes)" min-width="240">
              <template #default="{ row }">
                <el-tag v-for="sc in row.scopes" :key="sc" size="small" style="margin: 2px 4px 2px 0">
                  {{ sc }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="允许源" width="140">
              <template #default="{ row }">
                <span>{{ row.allowed_sources?.join(', ') || '*' }}</span>
              </template>
            </el-table-column>
            <el-table-column label="状态 / 过期时间" width="190">
              <template #default="{ row }">
                <el-tag v-if="row.revoked" size="small" type="danger">已吊销</el-tag>
                <el-tag v-else size="small" type="success">有效</el-tag>
                <div v-if="row.expires_at" class="sub-muted">到期：{{ fmtTime(row.expires_at) }}</div>
                <div v-else class="sub-muted">永不过期</div>
              </template>
            </el-table-column>
            <el-table-column label="最近使用" width="180">
              <template #default="{ row }">
                <span v-if="row.last_used_at">{{ fmtTime(row.last_used_at) }} ({{ row.last_used_ip }})</span>
                <span v-else class="sub-muted">尚未使用</span>
              </template>
            </el-table-column>
            <el-table-column label="操作" width="150">
              <template #default="{ row }">
                <el-button v-if="!row.revoked" link type="warning" @click="onRevokeToken(row)">吊销</el-button>
                <el-button link type="danger" :icon="Delete" @click="onDeleteToken(row)">删除</el-button>
              </template>
            </el-table-column>
          </el-table>
        </el-tab-pane>

        <!-- ==================== Tab 3: 事件 Webhook ==================== -->
        <el-tab-pane name="webhooks">
          <template #label>
            <span class="tab-lbl"><el-icon><Promotion /></el-icon> 事件驱动 Webhook</span>
          </template>

          <div class="pane-toolbar">
            <div class="pane-desc">
              文件上传、删除、重命名、跨源流转完成、分享创建等事件发生时，异步向外部 URL 推送携带 <code>X-UFile-Signature: sha256=&lt;hmac&gt;</code> 签名的 JSON 载荷，失败自动指数退避重试。
            </div>
            <el-button type="primary" :icon="Plus" @click="openCreateWebhook">新增 Webhook</el-button>
          </div>

          <el-table :data="webhooks" empty-text="暂无 Webhook 订阅端点">
            <el-table-column prop="name" label="名称" width="150" />
            <el-table-column prop="url" label="回调 URL" min-width="230" show-overflow-tooltip />
            <el-table-column label="订阅事件" min-width="200">
              <template #default="{ row }">
                <el-tag v-for="ev in row.events" :key="ev" size="small" type="info" style="margin: 2px 4px 2px 0">
                  {{ ev }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="状态与最近投递" width="210">
              <template #default="{ row }">
                <el-tag size="small" :type="row.enabled ? 'success' : 'info'">
                  {{ row.enabled ? '已启用' : '已停用' }}
                </el-tag>
                <span v-if="row.last_status" style="margin-left: 6px; font-size: 12px">
                  HTTP {{ row.last_status }}
                </span>
                <div v-if="row.last_error" class="err-line" :title="row.last_error">{{ row.last_error }}</div>
                <div v-else-if="row.last_fired_at" class="sub-muted">{{ fmtTime(row.last_fired_at) }}</div>
              </template>
            </el-table-column>
            <el-table-column label="操作" width="210">
              <template #default="{ row }">
                <el-button link type="success" @click="onTestWebhook(row)">测试 Ping</el-button>
                <el-button link type="primary" :icon="Edit" @click="openEditWebhook(row)">编辑</el-button>
                <el-button link type="danger" :icon="Delete" @click="onDeleteWebhook(row)">删除</el-button>
              </template>
            </el-table-column>
          </el-table>
        </el-tab-pane>

        <!-- ==================== Tab 4: 全文检索引擎 ==================== -->
        <el-tab-pane name="fulltext">
          <template #label>
            <span class="tab-lbl"><el-icon><Document /></el-icon> 全文检索引擎</span>
          </template>

          <div class="ft-panel">
            <div class="ft-stats-grid">
              <div class="stat-box">
                <div class="stat-label">引擎状态</div>
                <div class="stat-val">
                  <el-tag :type="ftStatus?.running ? 'primary' : 'success'">
                    {{ ftStatus?.running ? '后台抽取构建中…' : '就绪' }}
                  </el-tag>
                </div>
              </div>
              <div class="stat-box">
                <div class="stat-label">已索引文档数</div>
                <div class="stat-val">{{ ftStatus?.indexed_docs ?? 0 }} 篇</div>
              </div>
              <div class="stat-box">
                <div class="stat-label">倒排词项数 (Postings)</div>
                <div class="stat-val">{{ ftStatus?.total_postings ?? 0 }}</div>
              </div>
              <div class="stat-box">
                <div class="stat-label">最近索引完成时间</div>
                <div class="stat-val small">
                  {{ ftStatus?.last_indexed_at ? fmtTime(ftStatus.last_indexed_at) : '尚未完成全量构建' }}
                </div>
              </div>
            </div>

            <div class="ft-info">
              <h4>支持正文抽取的文档类型（单文件上限 ≤ 10MB）：</h4>
              <ul>
                <li><b>纯文本与代码配置</b>：<code>.txt</code>, <code>.md</code>, <code>.csv</code>, <code>.json</code>, <code>.yaml</code>, <code>.xml</code>, <code>.go</code>, <code>.py</code>, <code>.ts</code>, <code>.js</code>, <code>.sql</code>, <code>.sh</code> 等</li>
                <li><b>Office 办公文档</b>：<code>.docx</code>（段落与表格文本）、<code>.xlsx</code>（工作表单元格文本）、<code>.pptx</code>（幻灯片标题与文本框）</li>
                <li><b>PDF 电子文档</b>：<code>.pdf</code>（纯 Go 流式文本块解析，零外部 CGO 依赖）</li>
              </ul>
              <p class="sub-muted">
                提示：在顶部搜索框左侧下拉切换为「全文」即可按文档内容检索并查看高亮上下文摘要（Snippet）。
              </p>
            </div>

            <div class="ft-actions">
              <el-button
                type="primary"
                :loading="rebuildingFT || ftStatus?.running"
                @click="onRebuildFullText('')"
              >
                立即全量重建全文索引
              </el-button>
              <el-button @click="loadAll">刷新索引状态</el-button>
            </div>
          </div>
        </el-tab-pane>
      </el-tabs>
    </div>

    <!-- 弹窗 1：新增/编辑虚拟挂载点 -->
    <el-dialog v-model="mountDialogVisible" :title="editingMountId ? '编辑虚拟挂载点' : '新增虚拟挂载点'" width="500px">
      <el-form label-width="110px">
        <el-form-item label="虚拟挂载路径" required>
          <el-input v-model="mountForm.mount_path" placeholder="如 /财务文档/合同 或 /研发部/知识库" />
        </el-form-item>
        <el-form-item label="物理文件源" required>
          <el-select v-model="mountForm.source_id" style="width: 100%">
            <el-option
              v-for="s in physicalSources"
              :key="s.id"
              :label="`${s.name || s.id} (${s.type})`"
              :value="s.id"
            />
          </el-select>
        </el-form-item>
        <el-form-item label="源内子目录">
          <el-input v-model="mountForm.source_path" placeholder="留空表示挂载整个源根目录，如 2026/contracts" />
        </el-form-item>
        <el-form-item label="只读保护">
          <el-switch v-model="mountForm.readonly" active-text="开启只读（禁止通过该挂载点上传/修改/删除）" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="mountForm.remark" placeholder="选填备注" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="mountDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="mountSaving" @click="saveMount">保存</el-button>
      </template>
    </el-dialog>

    <!-- 弹窗 2：签发 API Token -->
    <el-dialog v-model="tokenDialogVisible" title="签发开放 API Token" width="540px">
      <el-form label-width="110px">
        <el-form-item label="令牌名称" required>
          <el-input v-model="tokenForm.name" placeholder="如：CI 构建产物归档 / 夜间备份脚本" />
        </el-form-item>
        <el-form-item label="权限作用域" required>
          <el-checkbox-group v-model="tokenForm.scopes">
            <div v-for="sc in scopeOptions" :key="sc.value" style="margin-bottom: 4px">
              <el-checkbox :value="sc.value">
                <b>{{ sc.label }}</b> — <span class="sub-muted">{{ sc.desc }}</span>
              </el-checkbox>
            </div>
          </el-checkbox-group>
        </el-form-item>
        <el-form-item label="授权文件源">
          <el-switch v-model="tokenForm.allSources" active-text="允许访问全部源 (*)" inactive-text="指定文件源" />
          <el-select
            v-if="!tokenForm.allSources"
            v-model="tokenForm.allowed_sources"
            multiple
            placeholder="选择允许该 Token 访问的文件源"
            style="width: 100%; margin-top: 8px"
          >
            <el-option v-for="s in sources" :key="s.id" :label="`${s.name || s.id} (${s.id})`" :value="s.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="IP / CIDR 白名单">
          <el-input
            v-model="tokenForm.ipText"
            type="textarea"
            :rows="2"
            placeholder="留空不限 IP；多个 IP 或 CIDR 用逗号或换行分隔，如 10.0.0.0/8, 192.168.1.10"
          />
        </el-form-item>
        <el-form-item label="有效期 (小时)">
          <el-input-number v-model="tokenForm.ttl_hours" :min="0" :max="87600" />
          <span class="sub-muted" style="margin-left: 8px">（0 表示永不过期，720 = 30天）</span>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="tokenDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="tokenSaving" @click="saveToken">立即签发</el-button>
      </template>
    </el-dialog>

    <!-- 弹窗 2b：一次性展示明文 API Token -->
    <el-dialog v-model="plainTokenDialogVisible" title="请妥善保存您的 API Token（仅展示一次）" width="540px">
      <el-alert
        type="warning"
        :closable="false"
        show-icon
        title="出于安全设计，服务端仅存储 SHA-256 哈希，关闭此窗口后将无法再次查看完整明文 Token。"
        style="margin-bottom: 14px"
      />
      <div class="plain-tok-box">
        <code>{{ createdPlainToken }}</code>
        <el-button type="primary" size="small" :icon="CopyDocument" @click="copyPlainToken">复制</el-button>
      </div>
      <div class="curl-demo">
        <div class="sub-muted" style="margin-bottom: 4px">调用示例：</div>
        <pre>curl -H "Authorization: Bearer {{ createdPlainToken }}" {{ locationOrigin }}/api/v1/sources</pre>
      </div>
      <template #footer>
        <el-button type="primary" @click="plainTokenDialogVisible = false">我已妥善保存</el-button>
      </template>
    </el-dialog>

    <!-- 弹窗 3：新增/编辑 Webhook -->
    <el-dialog v-model="webhookDialogVisible" :title="editingWebhookId ? '编辑 Webhook' : '新增 Webhook'" width="520px">
      <el-form label-width="100px">
        <el-form-item label="名称" required>
          <el-input v-model="webhookForm.name" placeholder="如：企业微信通知 / 自动化入库流水线" />
        </el-form-item>
        <el-form-item label="回调 URL" required>
          <el-input v-model="webhookForm.url" placeholder="https://example.com/webhook/ufile" />
        </el-form-item>
        <el-form-item label="签名密钥">
          <el-input v-model="webhookForm.secret" placeholder="选填 HMAC-SHA256 密钥（生成 X-UFile-Signature 头）" />
        </el-form-item>
        <el-form-item label="订阅事件" required>
          <el-checkbox-group v-model="webhookForm.events">
            <div v-for="ev in webhookEventOptions" :key="ev.value" style="margin-bottom: 4px">
              <el-checkbox :value="ev.value">{{ ev.label }}</el-checkbox>
            </div>
          </el-checkbox-group>
        </el-form-item>
        <el-form-item label="启用状态">
          <el-switch v-model="webhookForm.enabled" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="webhookDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="webhookSaving" @click="saveWebhook">保存</el-button>
      </template>
    </el-dialog>
  </main>
</template>

<script lang="ts">
const locationOrigin = typeof window !== 'undefined' ? window.location.origin : 'http://localhost:8080'
</script>

<style scoped>
.page-head {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 14px;
}
.title { margin: 0; font-size: 18px; }
.spacer { flex: 1; }
.tab-lbl { display: inline-flex; align-items: center; gap: 6px; }
.pane-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 14px;
}
.pane-desc {
  font-size: 13px;
  color: var(--el-text-color-secondary);
}
.sub-muted {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.err-line {
  font-size: 11.5px;
  color: var(--el-color-danger);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.plain-tok-box {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 10px 12px;
  background: var(--el-fill-color-light);
  border: 1px solid var(--el-border-color);
  border-radius: 6px;
  word-break: break-all;
}
.curl-demo {
  margin-top: 12px;
}
.curl-demo pre {
  margin: 0;
  padding: 10px;
  background: var(--el-fill-color);
  border-radius: 6px;
  font-size: 12px;
  overflow-x: auto;
}
.ft-panel {
  padding: 8px 4px;
}
.ft-stats-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 12px;
  margin-bottom: 18px;
}
.stat-box {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  padding: 14px 16px;
  background: var(--el-fill-color-lighter);
}
.stat-label {
  font-size: 12px;
  color: var(--el-text-color-secondary);
  margin-bottom: 6px;
}
.stat-val {
  font-size: 20px;
  font-weight: 600;
}
.stat-val.small {
  font-size: 14px;
}
.ft-info {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  padding: 12px 18px;
  margin-bottom: 16px;
  font-size: 13px;
}
.ft-info h4 { margin: 4px 0 8px; }
.ft-info ul { margin: 0 0 8px; padding-left: 20px; }
.ft-actions {
  display: flex;
  gap: 10px;
}
</style>
