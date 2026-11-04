<script setup lang="ts">
// 用户与细粒度权限管理独立页（/users）：
// 支持多用户增删改、冻结/解冻、密码重置，以及源级/目录树级 ACL 权限矩阵配置（默认全选目录）。
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  Back, Refresh, Plus, Search, Key, Delete, Setting, Edit,
} from '@element-plus/icons-vue'
import {
  listUsers, createUser, updateUser, updateUserPolicies,
  deleteUser, resetPassword, listSources,
} from '../api'
import { useAuthStore } from '../stores/auth'
import FolderTreeSelector from '../components/FolderTreeSelector.vue'
import type { UserInfo, Source, Policy, Action, Role } from '../api/types'

const router = useRouter()
const auth = useAuthStore()

const loading = ref(false)
const users = ref<UserInfo[]>([])
const sources = ref<Source[]>([])

const searchKeyword = ref('')
const roleFilter = ref<string>('all')

// —— 加载数据 ——

async function loadAll() {
  loading.value = true
  try {
    const [uList, sList] = await Promise.all([listUsers(), listSources()])
    users.value = uList || []
    sources.value = sList || []
  } catch (e: any) {
    ElMessage.error('加载用户数据失败：' + e.message)
  } finally {
    loading.value = false
  }
}

onMounted(loadAll)

const sourceNameMap = computed(() => {
  const m: Record<string, string> = {}
  for (const s of sources.value) m[s.id] = s.name
  return m
})

const filteredUsers = computed(() => {
  const kw = searchKeyword.value.trim().toLowerCase()
  return users.value.filter(u => {
    if (roleFilter.value !== 'all' && u.role !== roleFilter.value) return false
    if (!kw) return true
    return (
      u.username.toLowerCase().includes(kw) ||
      (u.remark || '').toLowerCase().includes(kw)
    )
  })
})

const stats = computed(() => {
  let admins = 0
  let normalUsers = 0
  let guests = 0
  let disabled = 0
  for (const u of users.value) {
    if (u.role === 'admin') admins++
    else if (u.role === 'user') normalUsers++
    else guests++
    if (u.disabled) disabled++
  }
  return { total: users.value.length, admins, normalUsers, guests, disabled }
})

// —— 辅助格式化 ——

function roleMeta(role: Role): { label: string; type: 'danger' | 'primary' | 'info' } {
  if (role === 'admin') return { label: '超级管理员', type: 'danger' }
  if (role === 'user') return { label: '协作用户', type: 'primary' }
  return { label: '只读访客', type: 'info' }
}

const actionLabels: Record<Action, string> = {
  read: '浏览',
  write: '写入',
  delete: '删除',
  share: '分享',
}

function formatActions(actions: Action[]): string {
  return actions.map(a => actionLabels[a] || a).join('·')
}

function formatPolicyDirs(p: Policy): string {
  const pfxs = p.path_prefixes?.length ? p.path_prefixes : p.path_prefix ? [p.path_prefix] : []
  if (pfxs.length === 0) return '全部目录'
  if (pfxs.length <= 2) return pfxs.map(x => '/' + x).join(', ')
  return `/${pfxs[0]} 等 ${pfxs.length} 个目录`
}

// —— 状态切换（启用 / 冻结）——

async function onToggleDisabled(row: UserInfo, enabled: boolean) {
  const targetDisabled = !enabled
  try {
    await updateUser(row.username, { disabled: targetDisabled })
    row.disabled = targetDisabled
    ElMessage.success(targetDisabled ? `已冻结账号「${row.username}」` : `已启用账号「${row.username}」`)
  } catch (e: any) {
    ElMessage.error('操作失败：' + e.message)
  }
}

// —— 新建 / 编辑用户弹窗 ——

const userDialogVisible = ref(false)
const userDialogEditMode = ref(false)
const userSubmitting = ref(false)
const userForm = reactive({
  username: '',
  password: '',
  role: 'user' as Exclude<Role, ''>,
  remark: '',
  disabled: false,
  openPolicyAfterCreate: true,
})

function openCreateDialog() {
  userDialogEditMode.value = false
  userForm.username = ''
  userForm.password = ''
  userForm.role = 'user'
  userForm.remark = ''
  userForm.disabled = false
  userForm.openPolicyAfterCreate = true
  userDialogVisible.value = true
}

function openEditDialog(row: UserInfo) {
  userDialogEditMode.value = true
  userForm.username = row.username
  userForm.password = ''
  userForm.role = row.role
  userForm.remark = row.remark || ''
  userForm.disabled = row.disabled
  userDialogVisible.value = true
}

async function submitUserForm() {
  const name = userForm.username.trim()
  if (!userDialogEditMode.value) {
    if (name.length < 2 || name.length > 64) {
      ElMessage.warning('用户名长度需在 2~64 个字符之间')
      return
    }
    if (userForm.password.length < 8) {
      ElMessage.warning('初始密码长度至少 8 位')
      return
    }
  }
  userSubmitting.value = true
  try {
    if (userDialogEditMode.value) {
      await updateUser(name, {
        role: userForm.role,
        remark: userForm.remark.trim(),
        disabled: userForm.disabled,
      })
      ElMessage.success('用户信息已更新')
      userDialogVisible.value = false
      await loadAll()
    } else {
      await createUser({
        username: name,
        password: userForm.password,
        role: userForm.role,
        remark: userForm.remark.trim(),
        disabled: userForm.disabled,
      })
      ElMessage.success(`已创建用户「${name}」`)
      userDialogVisible.value = false
      await loadAll()
      // 若为普通协作用户且勾选了立即配置权限，自动唤起权限抽屉
      if (userForm.role !== 'admin' && userForm.openPolicyAfterCreate) {
        const created = users.value.find(u => u.username === name)
        if (created) openPolicyDrawer(created)
      }
    }
  } catch (e: any) {
    ElMessage.error('保存失败：' + e.message)
  } finally {
    userSubmitting.value = false
  }
}

// —— 重置密码 ——

async function onResetPassword(row: UserInfo) {
  let newPwd: string
  try {
    ({ value: newPwd } = await ElMessageBox.prompt(
      `请输入「${row.username}」的新密码（至少 8 位，将通过 RSA-OAEP 信封加密传输）：`,
      '重置密码',
      {
        inputType: 'password',
        confirmButtonText: '确认重置',
        cancelButtonText: '取消',
        inputPattern: /^.{8,}$/,
        inputErrorMessage: '密码长度至少 8 位',
      },
    ))
  } catch {
    return
  }
  try {
    await resetPassword(row.username, newPwd)
    ElMessage.success(`已重置「${row.username}」的密码`)
  } catch (e: any) {
    ElMessage.error('重置密码失败：' + e.message)
  }
}

// —— 删除用户 ——

async function onDeleteUser(row: UserInfo) {
  try {
    await ElMessageBox.confirm(
      `确定要删除用户「${row.username}」吗？删除后该账号的登录会话将立即失效。`,
      '删除用户',
      {
        confirmButtonText: '删除',
        cancelButtonText: '取消',
        type: 'warning',
        confirmButtonClass: 'el-button--danger',
      },
    )
  } catch {
    return
  }
  try {
    await deleteUser(row.username)
    ElMessage.success('已删除用户')
    await loadAll()
  } catch (e: any) {
    ElMessage.error('删除失败：' + e.message)
  }
}

// —— 细粒度权限矩阵配置抽屉 ——

interface MatrixRow {
  sourceId: string
  sourceName: string
  sourceType: string
  supportsWrite: boolean
  enabled: boolean
  read: boolean
  write: boolean
  del: boolean
  share: boolean
  /** 允许访问的目录列表；空数组 [] 表示默认全选（整源全部目录） */
  pathPrefixes: string[]
}

const policyDrawerVisible = ref(false)
const policySaving = ref(false)
const policyTargetUser = ref<UserInfo | null>(null)
const policyTargetRole = ref<Exclude<Role, ''>>('user')
const matrixRows = ref<MatrixRow[]>([])

function openPolicyDrawer(row: UserInfo) {
  policyTargetUser.value = row
  policyTargetRole.value = row.role
  const policyMap = new Map<string, Policy>()
  for (const p of row.policies || []) {
    policyMap.set(p.source_id, p)
  }

  matrixRows.value = sources.value.map(src => {
    const p = policyMap.get(src.id) || policyMap.get('*')
    const acts = p?.actions || []
    const isGuestDefault = row.role === 'guest' && (!row.policies || row.policies.length === 0)
    const enabled = !!p || isGuestDefault
    const pfxs = p?.path_prefixes?.length
      ? [...p.path_prefixes]
      : p?.path_prefix ? [p.path_prefix] : []
    return {
      sourceId: src.id,
      sourceName: src.name,
      sourceType: src.type,
      supportsWrite: !!src.capabilities.write,
      enabled,
      read: enabled,
      write: acts.includes('write'),
      del: acts.includes('delete'),
      share: acts.includes('share'),
      pathPrefixes: pfxs,
    }
  })
  policyDrawerVisible.value = true
}

function onRowEnableChange(r: MatrixRow) {
  if (r.enabled) {
    r.read = true
  } else {
    r.read = false
    r.write = false
    r.del = false
    r.share = false
  }
}

function onRowActionChange(r: MatrixRow) {
  if (r.write || r.del || r.share) {
    r.enabled = true
    r.read = true
  }
}

function applyRowPreset(r: MatrixRow, preset: 'readonly' | 'collab' | 'full') {
  r.enabled = true
  r.read = true
  if (preset === 'readonly') {
    r.write = false
    r.del = false
    r.share = false
  } else if (preset === 'collab') {
    r.write = r.supportsWrite
    r.del = false
    r.share = false
  } else {
    r.write = r.supportsWrite
    r.del = r.supportsWrite
    r.share = true
  }
}

function applyGlobalPreset(preset: 'readonly' | 'collab' | 'full' | 'none') {
  if (policyTargetRole.value === 'admin') {
    policyTargetRole.value = 'user'
  }
  for (const r of matrixRows.value) {
    if (preset === 'none') {
      r.enabled = false
      r.read = false
      r.write = false
      r.del = false
      r.share = false
    } else {
      applyRowPreset(r, preset)
    }
  }
}

async function savePolicies() {
  if (!policyTargetUser.value) return
  const username = policyTargetUser.value.username

  const policies: Policy[] = []
  if (policyTargetRole.value !== 'admin') {
    for (const r of matrixRows.value) {
      if (!r.enabled) continue
      const actions: Action[] = ['read']
      if (r.write) actions.push('write')
      if (r.del) actions.push('delete')
      if (r.share) actions.push('share')
      policies.push({
        source_id: r.sourceId,
        actions,
        path_prefixes: r.pathPrefixes,
        path_prefix: r.pathPrefixes.length === 1 ? r.pathPrefixes[0] : '',
      })
    }
  }

  policySaving.value = true
  try {
    if (policyTargetRole.value !== policyTargetUser.value.role) {
      await updateUser(username, {
        role: policyTargetRole.value,
        policies,
      })
    } else {
      await updateUserPolicies(username, policies)
    }
    ElMessage.success(`已保存「${username}」的权限配置（实时生效）`)
    policyDrawerVisible.value = false
    await loadAll()
    if (username === auth.user) {
      await auth.refreshMe()
    }
  } catch (e: any) {
    ElMessage.error('保存权限失败：' + e.message)
  } finally {
    policySaving.value = false
  }
}
</script>

<template>
  <main class="page-main">
    <!-- 顶部栏 -->
    <div class="page-head">
      <el-button :icon="Back" link @click="router.push('/')">返回浏览</el-button>
      <h2 class="title">用户与权限管理</h2>
      <div class="stat-pills">
        <el-tag size="small" type="info" effect="plain">共 {{ stats.total }} 人</el-tag>
        <el-tag size="small" type="danger" effect="plain">管理员 {{ stats.admins }}</el-tag>
        <el-tag size="small" type="primary" effect="plain">协作用户 {{ stats.normalUsers }}</el-tag>
        <el-tag v-if="stats.guests" size="small" type="info" effect="plain">访客 {{ stats.guests }}</el-tag>
        <el-tag v-if="stats.disabled" size="small" type="warning" effect="plain">已冻结 {{ stats.disabled }}</el-tag>
      </div>
      <div class="spacer" />
      <el-input
        v-model="searchKeyword"
        placeholder="搜索用户名或备注"
        :prefix-icon="Search"
        clearable
        class="filter-input"
      />
      <el-select v-model="roleFilter" class="role-select">
        <el-option label="全部角色" value="all" />
        <el-option label="超级管理员" value="admin" />
        <el-option label="协作用户" value="user" />
        <el-option label="只读访客" value="guest" />
      </el-select>
      <el-button type="primary" :icon="Plus" @click="openCreateDialog">新建用户</el-button>
      <el-button :icon="Refresh" circle title="刷新" @click="loadAll" />
    </div>

    <!-- 用户列表 -->
    <div class="card-panel">
      <el-table :data="filteredUsers" v-loading="loading" empty-text="暂无匹配的用户">
        <el-table-column label="用户名 / 备注" min-width="180">
          <template #default="{ row }">
            <div class="user-cell">
              <b>{{ row.username }}</b>
              <el-tag v-if="row.username === auth.user" size="small" type="success" effect="plain">当前登录</el-tag>
            </div>
            <div v-if="row.remark" class="muted small">{{ row.remark }}</div>
          </template>
        </el-table-column>

        <el-table-column label="系统角色" width="130">
          <template #default="{ row }">
            <el-tag size="small" :type="roleMeta(row.role).type" :effect="row.role === 'admin' ? 'dark' : 'light'">
              {{ roleMeta(row.role).label }}
            </el-tag>
          </template>
        </el-table-column>

        <el-table-column label="账号状态" width="110">
          <template #default="{ row }">
            <el-switch
              :model-value="!row.disabled"
              :disabled="row.username === auth.user"
              active-text="正常"
              inactive-text="冻结"
              inline-prompt
              @change="(val: any) => onToggleDisabled(row, !!val)"
            />
          </template>
        </el-table-column>

        <el-table-column label="源级与目录访问权限 (ACL)" min-width="360">
          <template #default="{ row }">
            <template v-if="row.role === 'admin'">
              <el-tag size="small" type="danger" effect="plain">全部文件源 · 完全控制</el-tag>
            </template>
            <template v-else-if="row.role === 'guest' && (!row.policies || row.policies.length === 0)">
              <el-tag size="small" type="info" effect="plain">全部文件源 · 默认只读</el-tag>
            </template>
            <template v-else-if="!row.policies || row.policies.length === 0">
              <span class="muted small">未分配任何文件源权限（不可见任何源）</span>
            </template>
            <div v-else class="policy-tags">
              <el-tag
                v-for="p in row.policies"
                :key="p.source_id"
                size="small"
                effect="plain"
                class="policy-tag"
              >
                <b>{{ sourceNameMap[p.source_id] || p.source_id }}</b>
                <span class="policy-acts">[{{ formatActions(p.actions) }}]</span>
                <span class="policy-pfx">({{ formatPolicyDirs(p) }})</span>
              </el-tag>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="创建时间" width="165">
          <template #default="{ row }">
            <span class="small muted">{{ new Date(row.created_at).toLocaleString() }}</span>
          </template>
        </el-table-column>

        <el-table-column label="操作" width="250" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" :icon="Setting" @click="openPolicyDrawer(row)">配置权限</el-button>
            <el-button link type="primary" :icon="Edit" @click="openEditDialog(row)">编辑</el-button>
            <el-button link type="primary" :icon="Key" @click="onResetPassword(row)">改密</el-button>
            <el-button
              link
              type="danger"
              :icon="Delete"
              :disabled="row.username === auth.user"
              @click="onDeleteUser(row)"
            >
              删除
            </el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- 新建 / 编辑用户对话框 -->
    <el-dialog
      v-model="userDialogVisible"
      :title="userDialogEditMode ? `编辑用户「${userForm.username}」` : '新建用户'"
      width="480px"
    >
      <el-form label-width="90px">
        <el-form-item label="用户名" required>
          <el-input
            v-model="userForm.username"
            :disabled="userDialogEditMode"
            placeholder="2~64 个字符，建议使用字母/数字/下划线"
          />
        </el-form-item>

        <el-form-item v-if="!userDialogEditMode" label="初始密码" required>
          <el-input
            v-model="userForm.password"
            type="password"
            show-password
            placeholder="至少 8 位，经 RSA-OAEP 加密传输"
          />
        </el-form-item>

        <el-form-item label="系统角色" required>
          <el-radio-group v-model="userForm.role" :disabled="userDialogEditMode && userForm.username === auth.user">
            <el-radio-button value="user">协作用户 (按源授权)</el-radio-button>
            <el-radio-button value="guest">只读访客</el-radio-button>
            <el-radio-button value="admin">超级管理员</el-radio-button>
          </el-radio-group>
          <div class="role-hint muted small">
            <template v-if="userForm.role === 'user'">
              按 ACL 策略细粒度控制每个文件源的可见性、读写删享权限及可访问目录。
            </template>
            <template v-else-if="userForm.role === 'guest'">
              未配置策略时默认可只读浏览全部源；也可在「配置权限」中限定到特定源与目录。
            </template>
            <template v-else>
              拥有平台全部文件源、源配置、用户与外链的完全控制权限。
            </template>
          </div>
        </el-form-item>

        <el-form-item label="备注说明">
          <el-input v-model="userForm.remark" placeholder="如：研发部-张三 / 财务只读账号（可选）" />
        </el-form-item>

        <el-form-item v-if="!userDialogEditMode && userForm.role !== 'admin'" label="后续操作">
          <el-checkbox v-model="userForm.openPolicyAfterCreate">
            创建完成后立即打开「细粒度权限配置」面板
          </el-checkbox>
        </el-form-item>
      </el-form>

      <template #footer>
        <el-button @click="userDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="userSubmitting" @click="submitUserForm">
          {{ userDialogEditMode ? '保存修改' : '创建用户' }}
        </el-button>
      </template>
    </el-dialog>

    <!-- 细粒度权限矩阵配置抽屉 -->
    <el-drawer
      v-model="policyDrawerVisible"
      :title="`细粒度权限配置 —— ${policyTargetUser?.username || ''}`"
      size="740px"
    >
      <div v-if="policyTargetUser" class="drawer-body">
        <!-- 角色与快捷预设栏 -->
        <div class="preset-bar">
          <div class="preset-role">
            <span class="label">账号角色：</span>
            <el-radio-group
              v-model="policyTargetRole"
              size="small"
              :disabled="policyTargetUser.username === auth.user"
            >
              <el-radio-button value="user">协作用户 (ACL 隔离)</el-radio-button>
              <el-radio-button value="guest">只读访客</el-radio-button>
              <el-radio-button value="admin">超级管理员</el-radio-button>
            </el-radio-group>
          </div>

          <div v-if="policyTargetRole !== 'admin'" class="preset-actions">
            <span class="muted small">一键赋权：</span>
            <el-button size="small" @click="applyGlobalPreset('readonly')">全部源只读</el-button>
            <el-button size="small" @click="applyGlobalPreset('collab')">全部源读写</el-button>
            <el-button size="small" @click="applyGlobalPreset('full')">全部源全权</el-button>
            <el-button size="small" type="danger" plain @click="applyGlobalPreset('none')">清空全部</el-button>
          </div>
        </div>

        <!-- 管理员全权提示 -->
        <el-alert
          v-if="policyTargetRole === 'admin'"
          type="warning"
          :closable="false"
          show-icon
          title="超级管理员默认拥有全部文件源与系统设置的完全控制权"
          description="如需限制该账号只能访问部分文件源或子目录，请先将上方账号角色切换为「协作用户」。"
          style="margin-bottom: 16px"
        />

        <!-- 源级权限矩阵 -->
        <div v-else class="matrix-list">
          <div v-if="!matrixRows.length" class="empty-sources muted">
            当前系统尚未接入任何文件源，请先在首页右上角新增文件源。
          </div>

          <div
            v-for="r in matrixRows"
            :key="r.sourceId"
            class="matrix-card"
            :class="{ 'is-enabled': r.enabled }"
          >
            <div class="matrix-card-head">
              <el-checkbox v-model="r.enabled" @change="onRowEnableChange(r)">
                <b class="src-name">{{ r.sourceName }}</b>
                <span class="src-id muted">({{ r.sourceId }})</span>
              </el-checkbox>
              <el-tag size="small" type="info" effect="plain">{{ r.sourceType }}</el-tag>
              <el-tag v-if="!r.supportsWrite" size="small" type="warning" effect="plain">只读源</el-tag>
              <div class="spacer" />
              <div class="quick-btns">
                <el-button size="small" link type="primary" @click="applyRowPreset(r, 'readonly')">设为只读</el-button>
                <el-button
                  v-if="r.supportsWrite"
                  size="small"
                  link
                  type="primary"
                  @click="applyRowPreset(r, 'collab')"
                >
                  设为读写
                </el-button>
                <el-button size="small" link type="primary" @click="applyRowPreset(r, 'full')">设为全权</el-button>
              </div>
            </div>

            <div v-if="r.enabled" class="matrix-card-body">
              <div class="perm-row">
                <span class="perm-label">操作权限：</span>
                <el-checkbox v-model="r.read" disabled>浏览与下载 (read)</el-checkbox>
                <el-checkbox
                  v-model="r.write"
                  :disabled="!r.supportsWrite"
                  @change="onRowActionChange(r)"
                >
                  上传与改名 (write)
                </el-checkbox>
                <el-checkbox
                  v-model="r.del"
                  :disabled="!r.supportsWrite"
                  @change="onRowActionChange(r)"
                >
                  删除文件 (delete)
                </el-checkbox>
                <el-checkbox v-model="r.share" @change="onRowActionChange(r)">
                  创建分享 (share)
                </el-checkbox>
              </div>

              <!-- 可访问目录树选择（默认全选） -->
              <div class="prefix-row">
                <span class="perm-label">目录范围：</span>
                <div class="prefix-tree-wrap">
                  <FolderTreeSelector v-model="r.pathPrefixes" :source-id="r.sourceId" />
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <template #footer>
        <div class="drawer-footer">
          <span class="muted small">提示：默认全选所有目录；也可展开文件树勾选指定目录，保存后实时生效。</span>
          <div>
            <el-button @click="policyDrawerVisible = false">取消</el-button>
            <el-button type="primary" :loading="policySaving" @click="savePolicies">保存权限配置</el-button>
          </div>
        </div>
      </template>
    </el-drawer>
  </main>
</template>

<style scoped>
.page-main { max-width: 1280px; margin: 18px auto; padding: 0 24px; }
.page-head { display: flex; align-items: center; gap: 12px; margin-bottom: 14px; flex-wrap: wrap; }
.title { font-size: 16px; margin: 0; }
.stat-pills { display: flex; align-items: center; gap: 6px; }
.spacer { flex: 1; }
.filter-input { width: 220px; }
.role-select { width: 130px; }
.card-panel {
  background: var(--el-bg-color-overlay);
  border-radius: 8px;
  padding: 16px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.06);
}
.user-cell { display: flex; align-items: center; gap: 6px; }
.muted { color: var(--el-text-color-secondary); }
.small { font-size: 12px; }
.policy-tags { display: flex; flex-wrap: wrap; gap: 6px; }
.policy-tag { display: inline-flex; align-items: center; gap: 4px; }
.policy-acts { color: var(--el-color-primary); margin-left: 3px; }
.policy-pfx { color: var(--el-text-color-secondary); margin-left: 3px; font-family: monospace; }
.role-hint { margin-top: 6px; line-height: 1.4; }

.drawer-body { display: flex; flex-direction: column; gap: 12px; }
.preset-bar {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px;
  background: var(--el-fill-color-light);
  border-radius: 6px;
}
.preset-role, .preset-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.preset-role .label { font-size: 13px; font-weight: 600; }

.matrix-list { display: flex; flex-direction: column; gap: 10px; }
.matrix-card {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  padding: 12px 14px;
  transition: border-color 0.15s, background-color 0.15s;
}
.matrix-card.is-enabled {
  border-color: var(--el-color-primary-light-5);
  background: var(--el-fill-color-blank);
}
.matrix-card-head { display: flex; align-items: center; gap: 8px; }
.src-name { font-size: 14px; }
.src-id { font-size: 12px; margin-left: 4px; }
.quick-btns { display: flex; align-items: center; gap: 4px; }

.matrix-card-body {
  margin-top: 10px;
  padding-top: 10px;
  border-top: 1px dashed var(--el-border-color-lighter);
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.perm-row { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.prefix-row { display: flex; align-items: flex-start; gap: 12px; }
.perm-label { font-size: 12px; color: var(--el-text-color-secondary); width: 68px; flex-shrink: 0; line-height: 28px; }
.prefix-tree-wrap { flex: 1; min-width: 0; }

.drawer-footer {
  display: flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
}
.empty-sources { text-align: center; padding: 32px 0; }
</style>
