<script setup lang="ts">
// 文件预览弹窗：图片/音视频/PDF 原生渲染，文本/Markdown 取回渲染（>2MB 拒绝），
// 其余 fallback 提示下载。Esc/遮罩/按钮三种关闭方式，关闭即停媒体加载。
import { ref, watch } from 'vue'
import { useBrowseStore } from '../stores/browse'
import { rawUrl, downloadUrl } from '../api'
import { fmtSize, fmtTime } from '../utils/format'
import { previewKind, renderMarkdown, TEXT_PREVIEW_LIMIT } from '../utils/preview'
import type { Entry } from '../api/types'

const browse = useBrowseStore()
const visible = defineModel<boolean>({ default: false })

const kind = ref<ReturnType<typeof previewKind>>('')
const bodyHtml = ref('')   // md 渲染产物
const bodyText = ref('')   // 纯文本
const loading = ref(false)
const error = ref('')
const entryRef = ref<Entry | null>(null)

const mediaUrl = ref('')
const downloadHref = ref('')

watch(visible, async (v) => {
  if (!v) {
    // 关闭即清内容（停掉 video/audio 加载）
    kind.value = ''; bodyHtml.value = ''; bodyText.value = ''
    mediaUrl.value = ''; entryRef.value = null
    return
  }
  const entry = browse.previewEntry
  if (!entry) { visible.value = false; return }
  entryRef.value = entry
  error.value = ''; bodyHtml.value = ''; bodyText.value = ''
  kind.value = previewKind(entry.name)
  downloadHref.value = downloadUrl(browse.source, entry.path)

  if (['image', 'svg', 'video', 'audio', 'pdf'].includes(kind.value)) {
    mediaUrl.value = rawUrl(browse.source, entry.path) // 媒体标签 src（cookie 会话）
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
    width="min(960px, 94vw)"
    top="4vh"
    destroy-on-close
  >
    <div class="meta">
      {{ entryRef ? fmtSize(entryRef.size) + ' · ' + (fmtTime(entryRef.mod_time) || '') : '' }}
      <a :href="downloadHref" class="dl-link">下载</a>
    </div>

    <div v-loading="loading" class="pv-body">
      <img v-if="kind === 'image' || kind === 'svg'" :src="mediaUrl" :alt="entryRef?.name">
      <video v-else-if="kind === 'video'" :src="mediaUrl" controls autoplay />
      <audio v-else-if="kind === 'audio'" :src="mediaUrl" controls autoplay class="audio" />
      <iframe v-else-if="kind === 'pdf'" :src="mediaUrl" title="PDF 预览" />
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
.meta { color: var(--el-text-color-secondary); font-size: 12px; margin-bottom: 10px; }
.dl-link { color: var(--el-color-primary); margin-left: 10px; }
.pv-body {
  min-height: 200px;
  max-height: 78vh;
  overflow: auto;
  background: var(--el-fill-color-lighter);
  border-radius: 6px;
  display: flex;
  align-items: center;
  justify-content: center;
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
