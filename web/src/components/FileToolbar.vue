<script setup lang="ts">
// 工具栏：上传（多文件）/ 新建文件夹 / 已选计数 / 打包下载选中 / 删除选中。
// 按当前源的能力位与用户细粒度权限（write / delete）动态控制按钮显隐。
import { computed, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Upload, FolderAdd, Box, Delete } from '@element-plus/icons-vue'
import { useBrowseStore } from '../stores/browse'
import { mkdir, deleteEntries, uploadFiles, archiveUrl } from '../api'
import { downloadHref, joinPath } from '../utils/format'

const emit = defineEmits<{ refresh: [] }>()
const browse = useBrowseStore()

const uploading = ref(false)
const fileInput = ref<HTMLInputElement>()

const canWrite = computed(() => browse.canWrite)
const canDelete = computed(() => browse.canDelete)
const selectedCount = computed(() => browse.selected.length)
const visible = computed(() => (canWrite.value || canDelete.value || selectedCount.value > 0) && !!browse.source)

function pickFiles() {
  fileInput.value?.click()
}

async function onFilesChosen(ev: Event) {
  const input = ev.target as HTMLInputElement
  const files = input.files
  if (!files || !files.length) return
  uploading.value = true
  try {
    const res = await uploadFiles(browse.source, browse.path, Array.from(files))
    const data = await res.json().catch(() => ({}))
    if (!res.ok) throw new Error(data.message || res.statusText)
    const failed = (data.items || []).filter((i: any) => !i.ok)
    if (failed.length) {
      ElMessage.error('部分失败：' + failed.map((i: any) => `${i.file}: ${i.error}`).join('；'))
    } else {
      ElMessage.success(`已上传 ${data.uploaded} 个文件`)
    }
    emit('refresh')
  } catch (e: any) {
    ElMessage.error('上传失败：' + e.message)
  } finally {
    uploading.value = false
    input.value = ''
  }
}

async function onMkdir() {
  let name: string
  try {
    ({ value: name } = await ElMessageBox.prompt('新文件夹名称：', '新建文件夹', {
      confirmButtonText: '创建',
      cancelButtonText: '取消',
      inputPattern: /\S+/,
      inputErrorMessage: '名称不能为空',
    }))
  } catch {
    return
  }
  try {
    await mkdir(browse.source, joinPath(browse.path, name.trim()))
    ElMessage.success('已创建')
    emit('refresh')
  } catch (e: any) {
    ElMessage.error('新建失败：' + e.message)
  }
}

function onArchive() {
  if (!browse.selected.length) return
  downloadHref(archiveUrl(browse.source, browse.selected, browse.path || browse.source))
}

async function onDeleteSelected() {
  const paths = browse.selected
  if (!paths.length) return
  try {
    await ElMessageBox.confirm(
      `选中的 ${paths.length} 项将被删除（目录连同其内容），且不可恢复。`,
      '确认删除',
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning', confirmButtonClass: 'el-button--danger' },
    )
  } catch {
    return
  }
  try {
    const r = await deleteEntries(browse.source, paths)
    if (r.failed) {
      ElMessage.error(`${r.failed} 项删除失败：` + r.items.filter(i => !i.ok).map(i => `${i.path}: ${i.error}`).join('；'))
    } else {
      ElMessage.success(`已删除 ${r.deleted} 项`)
    }
    emit('refresh')
  } catch (e: any) {
    ElMessage.error('删除失败：' + e.message)
  }
}
</script>

<template>
  <div v-if="visible" class="toolbar">
    <template v-if="canWrite">
      <el-button type="primary" :icon="Upload" :loading="uploading" @click="pickFiles">
        {{ uploading ? '上传中…' : '上传' }}
      </el-button>
      <input
        ref="fileInput"
        type="file"
        multiple
        style="display: none"
        @change="onFilesChosen"
      >
      <el-button :icon="FolderAdd" @click="onMkdir">新建文件夹</el-button>
    </template>
    <span class="spacer" />
    <span v-if="selectedCount" class="sel-info">已选 {{ selectedCount }} 项</span>
    <el-button v-if="selectedCount" :icon="Box" @click="onArchive">打包下载选中</el-button>
    <el-button v-if="selectedCount && canDelete" type="danger" :icon="Delete" @click="onDeleteSelected">
      删除选中{{ selectedCount > 1 ? `（${selectedCount}）` : '' }}
    </el-button>
  </div>
</template>

<style scoped>
.toolbar { display: flex; align-items: center; gap: 8px; margin-bottom: 10px; }
.spacer { flex: 1; }
.sel-info { color: var(--el-text-color-secondary); font-size: 12px; margin-right: 4px; }
</style>
