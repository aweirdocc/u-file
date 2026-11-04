<script setup lang="ts">
// 文件预览弹窗：
// - 支持 Word (.docx) / Excel (.xlsx) / PowerPoint (.pptx) 在线结构化预览
// - 大图 (>512KB) 默认优先加载服务端动态缩放缩略图 (w=960&h=960)，可一键切换查看高清原图
// - 音视频 / PDF / Markdown / 文本原样渲染
import { ref, watch } from 'vue'
import { useBrowseStore } from '../stores/browse'
import { rawUrl, downloadUrl, thumbUrl, getOfficePreview } from '../api'
import { fmtSize, fmtTime } from '../utils/format'
import { previewKind, isThumbable, isOfficeKind, renderMarkdown, TEXT_PREVIEW_LIMIT } from '../utils/preview'
import type { Entry, OfficeResult } from '../api/types'
import OfficeViewer from './OfficeViewer.vue'

const browse = useBrowseStore()
const visible = defineModel<boolean>({ default: false })

const kind = ref<ReturnType<typeof previewKind>>('')
const bodyHtml = ref('')   // md 渲染产物
const bodyText = ref('')   // 纯文本
const officeData = ref<OfficeResult | null>(null)
const loading = ref(false)
const error = ref('')
const entryRef = ref<Entry | null>(null)

const mediaUrl = ref('')
const downloadHref = ref('')
const isUsingThumb = ref(false)

function loadOriginalImage() {
  if (!entryRef.value) return
  isUsingThumb.value = false
  mediaUrl.value = rawUrl(browse.source, entryRef.value.path)
}

watch(visible, async (v) => {
  if (!v) {
    // 关闭即清内容（停掉 video/audio 加载）
    kind.value = ''; bodyHtml.value = ''; bodyText.value = ''
    officeData.value = null; mediaUrl.value = ''; entryRef.value = null
    isUsingThumb.value = false
    return
  }
  const entry = browse.previewEntry
  if (!entry) { visible.value = false; return }
  entryRef.value = entry
  error.value = ''; bodyHtml.value = ''; bodyText.value = ''
  officeData.value = null; isUsingThumb.value = false
  kind.value = previewKind(entry.name)
  downloadHref.value = downloadUrl(browse.source, entry.path)

  if (kind.value === 'image') {
    // 大于 512KB 且支持服务端缩略图的图片，默认优先拉取 960x960 预览缩略图，避免多 MB 原图阻塞渲染
    if (entry.size > 512 * 1024 && isThumbable(entry.name)) {
      isUsingThumb.value = true
      mediaUrl.value = thumbUrl(browse.source, entry.path, 960, 960, 82)
    } else {
      mediaUrl.value = rawUrl(browse.source, entry.path)
    }
    return
  }

  if (['svg', 'video', 'audio', 'pdf'].includes(kind.value)) {
    mediaUrl.value = rawUrl(browse.source, entry.path)
    return
  }

  if (isOfficeKind(kind.value)) {
    loading.value = true
    try {
      officeData.value = await getOfficePreview(browse.source, entry.path)
    } catch (e: any) {
      error.value = '文档解析失败：' + (e.message || '未知错误')
      kind.value = ''
    } finally {
      loading.value = false
    }
    return
  }

  if (kind.value === 'text' || kind.value === 'md') {
    if (entry.size > TEXT_PREVIEW_LIMIT) {
      error.value = '文件超过 2MB，暂不支持在线预览'
      kind.value = ''
      return
    }
    loading.value = true
    try {
      const tok = sessionStorage.getItem('ufile_token') || ''
      const res = await fetch(rawUrl(browse.source, entry.path), {
        headers: tok ? { Authorization: 'Bearer ' + tok } : {},
      })
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
      const text = await res.text()
      if (kind.value === 'md') bodyHtml.value = renderMarkdown(text)
      else bodyText.value = text
    } catch (e: any) {
      error.value = '加载失败：' + e.message
      kind.value = ''
    } finally {
      loading.value = false
    }
  } else {
    error.value = '该格式暂不支持在线预览'
  }
})
</script>

<template>
  <el-dialog
    v-model="visible"
    :title="entryRef?.name || '预览'"
    width="min(1080px, 95vw)"
    top="3vh"
    destroy-on-close
  >
    <div class="meta">
      <span>{{ entryRef ? fmtSize(entryRef.size) + ' · ' + (fmtTime(entryRef.mod_time) || '') : '' }}</span>
      <span v-if="isUsingThumb" class="thumb-hint">
        ⚡ 当前为智能压缩预览图
        <a href="javascript:void(0)" class="dl-link" @click="loadOriginalImage">加载高清原图 ({{ entryRef ? fmtSize(entryRef.size) : '' }})</a>
      </span>
      <a :href="downloadHref" class="dl-link">下载原文件</a>
    </div>

    <div v-loading="loading" class="pv-body" :class="{ 'pv-body-office': officeData }">
      <img
        v-if="kind === 'image' || kind === 'svg'"
        :src="mediaUrl"
        :alt="entryRef?.name"
        @error="isUsingThumb ? loadOriginalImage() : undefined"
      >
      <video v-else-if="kind === 'video'" :src="mediaUrl" controls autoplay />
      <audio v-else-if="kind === 'audio'" :src="mediaUrl" controls autoplay class="audio" />
      <iframe v-else-if="kind === 'pdf'" :src="mediaUrl" title="PDF 预览" />
      <OfficeViewer v-else-if="officeData" :data="officeData" />
      <pre v-else-if="kind === 'text'">{{ bodyText }}</pre>
      <div v-else-if="kind === 'md'" class="md-body md" v-html="bodyHtml" />
      <div v-else class="fallback">
        {{ error || '该格式暂不支持在线预览' }}
        <div style="margin-top: 12px"><a :href="downloadHref" class="el-button el-button--primary">下载后查看</a></div>
      </div>
    </div>
  </el-dialog>
</template>

<style scoped>
.meta {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  color: var(--el-text-color-secondary);
  font-size: 12px;
  margin-bottom: 10px;
}
.thumb-hint {
  background: var(--el-color-primary-light-9);
  color: var(--el-color-primary);
  padding: 2px 8px;
  border-radius: 4px;
}
.dl-link { color: var(--el-color-primary); font-weight: 500; text-decoration: none; }
.dl-link:hover { text-decoration: underline; }
.pv-body {
  min-height: 220px;
  max-height: 80vh;
  overflow: auto;
  background: var(--el-fill-color-lighter);
  border-radius: 6px;
  display: flex;
  align-items: center;
  justify-content: center;
}
.pv-body-office {
  align-items: stretch;
  justify-content: flex-start;
  background: transparent;
}
.pv-body img { max-width: 100%; max-height: 76vh; object-fit: contain; }
.pv-body video, .pv-body audio { width: 100%; }
.pv-body iframe { width: 100%; height: 76vh; border: none; background: var(--el-bg-color); }
.pv-body pre {
  margin: 0; padding: 16px 20px; width: 100%;
  font: 12.5px/1.6 Consolas, Menlo, monospace;
  white-space: pre-wrap; word-break: break-all;
}
.md { padding: 20px 26px; width: 100%; background: var(--el-bg-color); overflow: auto; }
.fallback { padding: 40px; text-align: center; color: var(--el-text-color-secondary); }
</style>
