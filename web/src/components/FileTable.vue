<script setup lang="ts">
// 文件浏览区：支持「列表视图」与「大图网格缩略图视图」一键切换。
// - 列表视图：图片文件旁展示 32x32 懒加载缩略图（避免加载原图）
// - 网格视图：大图卡片式浏览（280x280 动态缩放缩略图 + loading="lazy"），解决大图目录卡顿和带宽浪费
import { computed, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { View, Download, Link, EditPen, Share, Grid, Menu as MenuIcon } from '@element-plus/icons-vue'
import { useBrowseStore } from '../stores/browse'
import { presign, downloadUrl, renameEntry, thumbUrl } from '../api'
import { fmtSize, fmtTime, fileIcon, parentOf, joinPath } from '../utils/format'
import { previewKind, isThumbable } from '../utils/preview'
import type { Entry } from '../api/types'
import ShareDialog from './ShareDialog.vue'
import type { ShareTarget } from './ShareDialog.vue'

const emit = defineEmits<{
  navigate: [source: string, path: string]
  preview: [entry: Entry]
  refresh: []
}>()

const browse = useBrowseStore()
const tableRef = ref()

const viewMode = ref<'list' | 'grid'>(
  (localStorage.getItem('ufile_view_mode') as 'list' | 'grid') === 'grid' ? 'grid' : 'list',
)
watch(viewMode, (m) => {
  localStorage.setItem('ufile_view_mode', m)
})

// 记录缩略图加载失败的条目，优雅降级为默认图标
const failedThumbs = ref<Record<string, boolean>>({})
watch(() => [browse.source, browse.path], () => {
  failedThumbs.value = {}
})

const canWrite = computed(() => browse.canWrite)
const canDelete = computed(() => browse.canDelete)
const canShare = computed(() => browse.canShare)
const showSelection = computed(() => canWrite.value || canDelete.value)

const shareDialogRef = ref<InstanceType<typeof ShareDialog>>()
const shareVisible = ref(false)

/** 分享入口：文件行 → 弹创建对话框 */
function onShare(e: Entry) {
  const t: ShareTarget = {
    sourceId: browse.source,
    path: e.path,
    name: e.name,
    size: e.size,
    isDir: e.is_dir,
  }
  shareVisible.value = true
  shareDialogRef.value?.open(t)
}

async function openEntry(e: Entry) {
  if (e.is_dir) emit('navigate', browse.source, e.path)
  else if (isPreviewable(e)) emit('preview', e)
}

function isPreviewable(e: Entry): boolean {
  return !e.is_dir && previewKind(e.name) !== ''
}

function canShowThumb(e: Entry): boolean {
  return !e.is_dir && isThumbable(e.name) && !failedThumbs.value[e.path]
}

function markThumbError(path: string) {
  failedThumbs.value[path] = true
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

function toggleGridSelect(path: string) {
  const idx = browse.selected.indexOf(path)
  if (idx >= 0) browse.selected.splice(idx, 1)
  else browse.selected.push(path)
}

/** 上级目录虚拟行：仅当当前路径深于用户的授权根路径前缀时才允许返回上一级 */
interface ParentRow extends Entry { __parent?: boolean }

const parentRow = computed<ParentRow[]>(() => {
  const pfx = browse.rootPrefix
  if (!browse.path || browse.path === pfx) return []
  return [{ path: parentOf(browse.path), name: '..', is_dir: true, size: 0, mod_time: '', __parent: true }]
})

const tableData = computed<ParentRow[]>(() => [...parentRow.value, ...browse.entries])

const imageCount = computed(() => browse.entries.filter(e => !e.is_dir && isThumbable(e.name)).length)

function rowKey(row: ParentRow) {
  return row.__parent ? '__parent__' : row.path
}
</script>

<template>
  <div class="file-browser-wrap">
    <!-- 视图切换顶栏 -->
    <div class="view-switch-bar">
      <div class="view-stats muted">
        共 {{ browse.entries.length }} 项
        <template v-if="imageCount > 0">
          · 含 {{ imageCount }} 张图片（已启用按需缩略图加速）
        </template>
      </div>
      <el-button-group size="small">
        <el-button
          :type="viewMode === 'list' ? 'primary' : 'default'"
          :icon="MenuIcon"
          @click="viewMode = 'list'"
        >
          列表
        </el-button>
        <el-button
          :type="viewMode === 'grid' ? 'primary' : 'default'"
          :icon="Grid"
          @click="viewMode = 'grid'"
        >
          缩略图网格
        </el-button>
      </el-button-group>
    </div>

    <!-- ==================== 1. 列表视图 ==================== -->
    <el-table
      v-if="viewMode === 'list'"
      ref="tableRef"
      :data="tableData"
      v-loading="browse.loading"
      :row-key="rowKey"
      empty-text="空目录"
      @selection-change="onSelectionChange"
    >
      <el-table-column v-if="showSelection" type="selection" width="42" :selectable="(row: ParentRow) => !row.__parent" />

      <el-table-column label="名称" min-width="52%">
        <template #default="{ row }">
          <div class="name-cell">
            <template v-if="row.__parent">
              <a class="name-link" @click.prevent="emit('navigate', browse.source, row.path)">↰ ..</a>
            </template>
            <template v-else-if="row.is_dir">
              <span class="icon">📁</span>
              <a class="name-link" @click.prevent="openEntry(row)">{{ row.name }}</a>
            </template>
            <template v-else>
              <img
                v-if="canShowThumb(row)"
                :src="thumbUrl(browse.source, row.path, 96, 96, 75)"
                loading="lazy"
                class="inline-thumb"
                :alt="row.name"
                @click="emit('preview', row)"
                @error="markThumbError(row.path)"
              >
              <span v-else class="icon">{{ fileIcon(row.name) }}</span>
              <a
                v-if="isPreviewable(row)"
                class="name-link"
                @click.prevent="emit('preview', row)"
              >{{ row.name }}</a>
              <span v-else class="name-plain">{{ row.name }}</span>
            </template>
          </div>
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

      <el-table-column label="操作" width="260">
        <template #default="{ row }">
          <template v-if="!row.__parent">
            <el-button v-if="isPreviewable(row)" link type="primary" :icon="View" @click="emit('preview', row)">预览</el-button>
            <el-button link type="primary" :icon="Download" tag="a" :href="downloadUrl(browse.source, row.path)">下载</el-button>
            <el-button v-if="browse.canPresign && !row.is_dir" link type="primary" :icon="Link" @click="openPresign(row)">直链</el-button>
            <el-button v-if="canShare" link type="primary" :icon="Share" @click="onShare(row)">分享</el-button>
            <el-button v-if="canWrite" link type="primary" :icon="EditPen" @click="onRename(row)">重命名</el-button>
          </template>
        </template>
      </el-table-column>
    </el-table>

    <!-- ==================== 2. 缩略图网格视图 ==================== -->
    <div v-else v-loading="browse.loading" class="thumb-grid">
      <div v-if="tableData.length === 0" class="grid-empty muted">空目录</div>

      <div
        v-for="row in tableData"
        :key="rowKey(row)"
        class="grid-card"
        :class="{ selected: !row.__parent && browse.selected.includes(row.path) }"
        @click="row.__parent ? emit('navigate', browse.source, row.path) : openEntry(row)"
      >
        <!-- 多选复选框 -->
        <input
          v-if="showSelection && !row.__parent"
          type="checkbox"
          class="grid-checkbox"
          :checked="browse.selected.includes(row.path)"
          @click.stop="toggleGridSelect(row.path)"
        >

        <!-- 缩略图 / 图标舞台 -->
        <div class="grid-thumb-box">
          <span v-if="row.__parent" class="grid-big-icon">↰</span>
          <span v-else-if="row.is_dir" class="grid-big-icon">📁</span>
          <img
            v-else-if="canShowThumb(row)"
            :src="thumbUrl(browse.source, row.path, 280, 280, 78)"
            loading="lazy"
            class="grid-thumb-img"
            :alt="row.name"
            @error="markThumbError(row.path)"
          >
          <span v-else class="grid-big-icon">{{ fileIcon(row.name) }}</span>
        </div>

        <!-- 文件名与元信息 -->
        <div class="grid-meta">
          <div class="grid-name" :title="row.name">{{ row.name }}</div>
          <div class="grid-sub muted">
            <span v-if="row.__parent">返回上级</span>
            <span v-else-if="row.is_dir">文件夹</span>
            <span v-else>{{ fmtSize(row.size) }}</span>
          </div>
        </div>

        <!-- 快捷操作栏 -->
        <div v-if="!row.__parent" class="grid-actions" @click.stop>
          <el-button v-if="isPreviewable(row)" link size="small" type="primary" :icon="View" @click="emit('preview', row)" />
          <el-button link size="small" type="primary" :icon="Download" tag="a" :href="downloadUrl(browse.source, row.path)" />
          <el-button v-if="canShare" link size="small" type="primary" :icon="Share" @click="onShare(row)" />
          <el-button v-if="canWrite" link size="small" type="primary" :icon="EditPen" @click="onRename(row)" />
        </div>
      </div>
    </div>

    <ShareDialog ref="shareDialogRef" v-model="shareVisible" />
  </div>
</template>

<style scoped>
.view-switch-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 6px 4px 10px;
}
.view-stats { font-size: 12.5px; }
.name-cell {
  display: flex;
  align-items: center;
  gap: 8px;
}
.inline-thumb {
  width: 30px;
  height: 30px;
  object-fit: cover;
  border-radius: 4px;
  border: 1px solid var(--el-border-color-lighter);
  cursor: zoom-in;
  flex-shrink: 0;
  background: var(--el-fill-color-light);
}
.name-link { color: var(--el-text-color-primary); cursor: pointer; text-decoration: none; }
.name-link:hover { color: var(--el-color-primary); }
.name-plain { color: var(--el-text-color-primary); }
.icon { font-size: 16px; flex-shrink: 0; }
.muted { color: var(--el-text-color-secondary); }

/* 网格缩略图视图 */
.thumb-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(175px, 1fr));
  gap: 14px;
  padding: 6px 2px 16px;
}
.grid-empty {
  grid-column: 1 / -1;
  text-align: center;
  padding: 48px 0;
}
.grid-card {
  position: relative;
  display: flex;
  flex-direction: column;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  background: var(--el-bg-color);
  overflow: hidden;
  cursor: pointer;
  transition: transform 0.15s, box-shadow 0.15s, border-color 0.15s;
}
.grid-card:hover {
  transform: translateY(-2px);
  box-shadow: 0 6px 16px rgba(15, 23, 42, 0.08);
  border-color: var(--el-color-primary-light-5);
}
.grid-card.selected {
  border-color: var(--el-color-primary);
  box-shadow: 0 0 0 2px var(--el-color-primary-light-7);
}
.grid-checkbox {
  position: absolute;
  top: 8px;
  left: 8px;
  z-index: 2;
  width: 16px;
  height: 16px;
  cursor: pointer;
}
.grid-thumb-box {
  width: 100%;
  height: 135px;
  background: var(--el-fill-color-lighter);
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
}
.grid-thumb-img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  transition: transform 0.25s;
}
.grid-card:hover .grid-thumb-img {
  transform: scale(1.04);
}
.grid-big-icon {
  font-size: 42px;
  user-select: none;
}
.grid-meta {
  padding: 8px 10px 4px;
}
.grid-name {
  font-size: 13px;
  font-weight: 500;
  color: var(--el-text-color-primary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.grid-sub {
  font-size: 11.5px;
  margin-top: 2px;
}
.grid-actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 2px;
  padding: 2px 8px 6px;
}
</style>
