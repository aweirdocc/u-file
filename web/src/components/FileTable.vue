<script setup lang="ts">
// 文件表格：el-table + 勾选列（仅可写源显示）、名称/大小/时间/操作列。
import { computed, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { View, Download, Link, EditPen } from '@element-plus/icons-vue'
import { useBrowseStore } from '../stores/browse'
import { presign, downloadUrl, renameEntry } from '../api'
import { fmtSize, fmtTime, fileIcon, parentOf, joinPath } from '../utils/format'
import { previewKind } from '../utils/preview'
import type { Entry } from '../api/types'

const emit = defineEmits<{
  navigate: [source: string, path: string]
  preview: [entry: Entry]
  refresh: []
}>()

const browse = useBrowseStore()
const tableRef = ref()

const canWrite = computed(() => browse.canWrite)

async function openEntry(e: Entry) {
  if (e.is_dir) emit('navigate', browse.source, e.path)
}

function isPreviewable(e: Entry): boolean {
  return !e.is_dir && previewKind(e.name) !== ''
}

async function openPresign(e: Entry) {
  try {
    const r = await presign(browse.source, e.path)
    window.open(r.url)
  } catch (err: any) {
    ElMessage.error(err.message)
  }
}

async function onRename(e: Entry) {
  let newName: string
  try {
    ({ value: newName } = await ElMessageBox.prompt('重命名为：', `重命名「${e.name}」`, {
      inputValue: e.name,
      confirmButtonText: '确定',
      cancelButtonText: '取消',
      inputPattern: /\S+/,
      inputErrorMessage: '名称不能为空',
    }))
  } catch {
    return
  }
  if (newName === e.name) return
  try {
    await renameEntry(browse.source, e.path, joinPath(parentOf(e.path), newName.trim()))
    ElMessage.success('已重命名')
    emit('refresh')
  } catch (err: any) {
    ElMessage.error('重命名失败：' + err.message)
  }
}

function onSelectionChange(rows: Entry[]) {
  browse.selected = rows.map(r => r.path)
}

/** 上级目录虚拟行 */
interface ParentRow extends Entry { __parent?: boolean }

const parentRow = computed<ParentRow[]>(() =>
  browse.path ? [{ path: parentOf(browse.path), name: '..', is_dir: true, size: 0, mod_time: '', __parent: true }] : [],
)

const tableData = computed<ParentRow[]>(() => [...parentRow.value, ...browse.entries])

function rowKey(row: ParentRow) {
  return row.__parent ? '__parent__' : row.path
}
</script>

<template>
  <el-table
    ref="tableRef"
    :data="tableData"
    v-loading="browse.loading"
    :row-key="rowKey"
    empty-text="空目录"
    @selection-change="onSelectionChange"
  >
    <el-table-column v-if="canWrite" type="selection" width="42" :selectable="(row: ParentRow) => !row.__parent" />

    <el-table-column label="名称" min-width="52%">
      <template #default="{ row }">
        <template v-if="row.__parent">
          <a class="name-link" @click.prevent="emit('navigate', browse.source, row.path)">↰ ..</a>
        </template>
        <template v-else-if="row.is_dir">
          <span class="icon">📁</span>
          <a class="name-link" @click.prevent="openEntry(row)">{{ row.name }}</a>
        </template>
        <template v-else>
          <span class="icon">{{ fileIcon(row.name) }}</span>
          <span class="name-plain">{{ row.name }}</span>
        </template>
      </template>
    </el-table-column>

    <el-table-column label="大小" width="110">
      <template #default="{ row }">
        <span v-if="row.is_dir" class="muted">—</span>
        <span v-else>{{ fmtSize(row.size) }}</span>
      </template>
    </el-table-column>

    <el-table-column label="修改时间" width="180">
      <template #default="{ row }">
        <span class="muted">{{ row.__parent || row.is_dir ? (row.__parent ? '—' : fmtTime(row.mod_time)) : fmtTime(row.mod_time) }}</span>
      </template>
    </el-table-column>

    <el-table-column label="操作" width="220">
      <template #default="{ row }">
        <template v-if="!row.__parent">
          <el-button v-if="isPreviewable(row)" link type="primary" :icon="View" @click="emit('preview', row)">预览</el-button>
          <el-button link type="primary" :icon="Download" tag="a" :href="downloadUrl(browse.source, row.path)">下载</el-button>
          <el-button v-if="browse.canPresign && !row.is_dir" link type="primary" :icon="Link" @click="openPresign(row)">直链</el-button>
          <el-button v-if="canWrite" link type="primary" :icon="EditPen" @click="onRename(row)">重命名</el-button>
        </template>
      </template>
    </el-table-column>
  </el-table>
</template>

<style scoped>
.name-link { color: var(--el-text-color-primary); cursor: pointer; text-decoration: none; }
.name-link:hover { color: var(--el-color-primary); }
.name-plain { color: var(--el-text-color-primary); }
.icon { margin-right: 6px; }
.muted { color: var(--el-text-color-secondary); }
</style>
