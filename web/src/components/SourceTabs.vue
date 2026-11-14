<script setup lang="ts">
// 源标签栏：胶囊高亮当前源；admin hover 显示编辑/删除。
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit, Delete } from '@element-plus/icons-vue'
import { useBrowseStore } from '../stores/browse'
import { useAuthStore } from '../stores/auth'
import { removeSource } from '../api'
import SourceDialog from './SourceDialog.vue'

const emit = defineEmits<{ select: [source: string, path: string] }>()

const browse = useBrowseStore()
const auth = useAuthStore()
const router = useRouter()
const editTarget = ref<string | null>(null)
const dialogVisible = ref(false)

function onSelect(id: string) {
  emit('select', id, '')
}

function onEdit(id: string) {
  if (id === '@vfs') {
    router.push('/platform')
    return
  }
  editTarget.value = id
  dialogVisible.value = true
}

async function onDelete(id: string) {
  try {
    await ElMessageBox.confirm(
      '只删除平台里的接入配置，不会删除存储里的任何文件。',
      `确定删除文件源「${id}」？`,
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return
  }
  const wasCurrent = browse.source === id
  try {
    await removeSource(id)
    // 先同步本地列表再改变路由。否则删除最后一个源时，路由守卫会从旧列表中
    // 重新选中刚删掉的源，界面无法回到空状态。
    await browse.loadSources()
    ElMessage.success('已删除')
    if (wasCurrent) emit('select', '', '')
  } catch (e: any) {
    ElMessage.error('删除失败：' + e.message)
  }
}
</script>

<template>
  <div class="source-tabs">
    <template v-if="browse.sources.length">
      <el-tag
        v-for="s in browse.sources"
        :key="s.id"
        :type="s.id === browse.source ? 'primary' : 'info'"
        :effect="s.id === browse.source ? 'light' : 'plain'"
        size="large"
        class="src-tag"
        @click="onSelect(s.id)"
      >
        {{ s.name || s.id }}
        <span class="src-type">· {{ s.type }}</span>
        <template v-if="auth.isAdmin">
          <el-icon class="op" :title="s.id === '@vfs' ? '管理虚拟挂载点' : '编辑'" @click.stop="onEdit(s.id)"><Edit /></el-icon>
          <el-icon v-if="s.id !== '@vfs'" class="op danger" title="删除" @click.stop="onDelete(s.id)"><Delete /></el-icon>
        </template>
      </el-tag>
    </template>
    <span v-else class="empty-hint">
      {{ auth.isAdmin ? '还没有文件源，点击右上角「新增文件源」' : '管理员尚未配置文件源' }}
    </span>

    <SourceDialog v-model="dialogVisible" :edit-id="editTarget" />
  </div>
</template>

<style scoped>
.source-tabs { margin-bottom: 12px; display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
.src-tag {
  cursor: pointer;
  height: auto;
  padding: 7px 12px;
  font-size: 13px;
  border-radius: 999px;
}
.src-type { opacity: .55; margin: 0 4px 0 2px; font-size: 12px; }
.op {
  margin-left: 6px;
  cursor: pointer;
  color: var(--el-text-color-secondary);
  vertical-align: -2px;
}
.op:hover { color: var(--el-color-primary); }
.op.danger:hover { color: var(--el-color-danger); }
.empty-hint { color: var(--el-text-color-secondary); font-size: 13px; }
</style>
