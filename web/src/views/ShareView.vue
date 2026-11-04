<script setup lang="ts">
// 公开分享页（/s/:token，无需登录）：展示文件信息 → 需要时输入提取码 → 下载/在线预览（支持图片缩略图加速与 docx/xlsx/pptx 在线查看）。
// 自建简洁布局，不依赖 AppHeader；接口走原生 fetch（不受 401 登录拦截影响）。
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { Download, View } from '@element-plus/icons-vue'
import { fmtSize, fileIcon } from '../utils/format'
import { previewKind, isThumbable, isOfficeKind } from '../utils/preview'
import type { OfficeResult } from '../api/types'
import ThemeToggle from '../components/ThemeToggle.vue'
import OfficeViewer from '../components/OfficeViewer.vue'
import { useSettingsStore } from '../stores/settings'

interface ShareViewData {
  token: string
  name: string
  size: number
  is_dir: boolean
  has_password: boolean
  expires_at: string
  max_downloads?: number
  downloads: number
  unlocked?: boolean
}

const route = useRoute()
const token = computed(() => String(route.params.token || ''))

// 公开页独立入口也支持主题（settings store 的 apply 在公开路径不走 main.ts 初始化，这里手动应用一次）
useSettingsStore().apply()

const state = ref<'loading' | 'ready' | 'gone' | 'error'>('loading')
const goneReason = ref('')
const info = ref<ShareViewData | null>(null)
const password = ref('')
const unlocking = ref(false)
const downloading = ref(false)

const pvOpen = ref(false)
const pvLoading = ref(false)
const officeData = ref<OfficeResult | null>(null)
const showOriginalImg = ref(false)

const unlocked = computed(() => !!info.value && (!info.value.has_password || info.value.unlocked))

const pvKind = computed(() => {
  if (!info.value || info.value.is_dir) return ''
  return previewKind(info.value.name)
})

const previewable = computed(() => pvKind.value !== '')

const isLargeThumbableImage = computed(() => {
  if (!info.value || pvKind.value !== 'image') return false
  return info.value.size > 512 * 1024 && isThumbable(info.value.name)
})

const imagePreviewSrc = computed(() => {
  const t = encodeURIComponent(token.value)
  if (isLargeThumbableImage.value && !showOriginalImg.value) {
    return `/api/v1/s/${t}/thumb?w=960&h=960&q=82`
  }
  return `/api/v1/s/${t}/raw`
})

async function load() {
  state.value = 'loading'
  try {
    const res = await fetch(`/api/v1/s/${encodeURIComponent(token.value)}`)
    if (res.ok) {
      info.value = await res.json()
      state.value = 'ready'
      return
    }
    const data = await res.json().catch(() => ({}))
    if (res.status === 404 || res.status === 410) {
      goneReason.value = data.message || '分享不存在或已失效'
      state.value = 'gone'
    } else {
      state.value = 'error'
    }
  } catch {
    state.value = 'error'
  }
}

async function onUnlock() {
  if (!password.value.trim()) {
    ElMessage.warning('请输入提取码')
    return
  }
  unlocking.value = true
  try {
    const res = await fetch(`/api/v1/s/${encodeURIComponent(token.value)}/access`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: password.value.trim() }),
    })
    const data = await res.json().catch(() => ({}))
    if (res.ok) {
      if (info.value) info.value.unlocked = true
      ElMessage.success('验证通过')
    } else {
      ElMessage.error(data.message || '提取码错误')
    }
  } catch {
    ElMessage.error('网络异常，请重试')
  } finally {
    unlocking.value = false
  }
}

async function onDownload() {
  downloading.value = true
  try {
    const res = await fetch(`/api/v1/s/${encodeURIComponent(token.value)}/download`)
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      ElMessage.error(data.message || `下载失败（${res.status}）`)
      if (res.status === 404 || res.status === 410) await load()
      return
    }
    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = info.value?.name || 'download'
    a.click()
    URL.revokeObjectURL(url)
    if (info.value && !info.value.is_dir) info.value.downloads++
  } catch {
    ElMessage.error('下载失败，请重试')
  } finally {
    downloading.value = false
  }
}

async function onPreview() {
  const k = pvKind.value
  if (isOfficeKind(k)) {
    pvOpen.value = true
    pvLoading.value = true
    officeData.value = null
    try {
      const res = await fetch(`/api/v1/s/${encodeURIComponent(token.value)}/office`)
      const data = await res.json().catch(() => ({}))
      if (!res.ok) throw new Error(data.message || '文档解析失败')
      officeData.value = data
    } catch (e: any) {
      ElMessage.error(e.message || '预览加载失败')
      pvOpen.value = false
    } finally {
      pvLoading.value = false
    }
    return
  }
  if (k === 'image' || k === 'svg') {
    showOriginalImg.value = false
    pvOpen.value = true
    return
  }
  window.open(`/api/v1/s/${encodeURIComponent(token.value)}/raw`, '_blank')
}

onMounted(load)
</script>

<template>
  <div class="share-page">
    <div class="theme-corner">
      <ThemeToggle />
    </div>
    <div class="share-card" :class="{ wide: pvOpen }">
      <div class="brand">🗂 u-file 分享</div>

      <!-- 加载中 -->
      <div v-if="state === 'loading'" class="center muted">加载中……</div>

      <!-- 失效 -->
      <template v-else-if="state === 'gone'">
        <el-result icon="warning" title="链接已失效" :sub-title="goneReason">
          <template #extra>
            <a class="home-link" href="/">访问 u-file</a>
          </template>
        </el-result>
      </template>

      <!-- 异常 -->
      <el-result v-else-if="state === 'error'" icon="error" title="加载失败" sub-title="网络异常或服务不可用" />

      <!-- 正常 -->
      <template v-else-if="info">
        <div class="file-head">
          <img
            v-if="unlocked && !info.is_dir && isThumbable(info.name)"
            :src="`/api/v1/s/${encodeURIComponent(token)}/thumb?w=120&h=120`"
            class="head-thumb"
            :alt="info.name"
          >
          <span v-else class="icon">{{ info.is_dir ? '📁' : fileIcon(info.name) }}</span>
          <div>
            <div class="name">{{ info.name }}</div>
            <div class="muted small">
              {{ info.is_dir ? '目录（zip 打包下载）' : fmtSize(info.size) }}
              · 有效期至 {{ new Date(info.expires_at).toLocaleString() }}
              <template v-if="info.max_downloads"> · 已下载 {{ info.downloads }}/{{ info.max_downloads }} 次</template>
            </div>
          </div>
        </div>

        <!-- 需要提取码且未解锁 -->
        <div v-if="!unlocked" class="unlock-box">
          <p class="muted">该分享受提取码保护，请输入提取码：</p>
          <div class="unlock-row">
            <el-input
              v-model="password"
              placeholder="提取码"
              style="width: 220px"
              @keydown.enter="onUnlock"
            />
            <el-button type="primary" :loading="unlocking" @click="onUnlock">验证</el-button>
          </div>
        </div>

        <!-- 已解锁：操作区 -->
        <div v-else class="ops">
          <el-button type="primary" size="large" :icon="Download" :loading="downloading" @click="onDownload">
            {{ info.is_dir ? '下载 zip 包' : '下载' }}
          </el-button>
          <el-button v-if="previewable" size="large" :icon="View" @click="onPreview">在线预览</el-button>
        </div>

        <!-- 内联预览面板（支持图片智能缩略图 + docx/xlsx/pptx 在线查看） -->
        <div v-if="unlocked && pvOpen" v-loading="pvLoading" class="inline-pv-box">
          <div v-if="pvKind === 'image' || pvKind === 'svg'" class="img-pv-wrap">
            <div v-if="isLargeThumbableImage && !showOriginalImg" class="thumb-banner">
              ⚡ 已加载动态缩放预览图，节省流量
              <el-button link type="primary" size="small" @click="showOriginalImg = true">
                查看原图 ({{ fmtSize(info.size) }})
              </el-button>
            </div>
            <img :src="imagePreviewSrc" :alt="info.name" class="pv-img">
          </div>
          <OfficeViewer v-else-if="officeData" :data="officeData" />
        </div>
      </template>
    </div>

    <footer class="page-footer">u-file · 统一文件管理服务平台</footer>
  </div>
</template>

<style scoped>
.share-page {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
  align-items: center;
  padding: 8vh 16px 32px;
  background:
    radial-gradient(1000px 500px at 85% -10%, var(--el-color-primary-light-9), transparent 60%),
    var(--ufile-body-bg);
}
.theme-corner { position: fixed; top: 16px; right: 16px; }
.share-card {
  width: 520px;
  max-width: calc(100vw - 32px);
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ufile-radius);
  box-shadow: var(--ufile-card-shadow);
  padding: 28px 32px;
  transition: width 0.2s;
}
.share-card.wide {
  width: 980px;
}
.brand { font-size: 15px; font-weight: 600; margin-bottom: 20px; color: var(--el-text-color-primary); }
.center { text-align: center; padding: 40px 0; }
.muted { color: var(--el-text-color-secondary); }
.small { font-size: 12px; }
.file-head { display: flex; gap: 12px; align-items: center; margin-bottom: 20px; }
.file-head .icon { font-size: 34px; }
.head-thumb {
  width: 48px;
  height: 48px;
  object-fit: cover;
  border-radius: 6px;
  border: 1px solid var(--el-border-color-lighter);
}
.file-head .name { font-size: 16px; font-weight: 600; word-break: break-all; }
.unlock-box { border-top: 1px dashed var(--el-border-color); padding-top: 16px; }
.unlock-box p { margin: 0 0 10px; }
.unlock-row { display: flex; gap: 10px; }
.ops { display: flex; gap: 4px; border-top: 1px dashed var(--el-border-color); padding-top: 20px; }
.inline-pv-box {
  margin-top: 20px;
  padding-top: 16px;
  border-top: 1px solid var(--el-border-color-lighter);
}
.img-pv-wrap {
  text-align: center;
}
.thumb-banner {
  margin-bottom: 10px;
  font-size: 12.5px;
  color: var(--el-text-color-secondary);
}
.pv-img {
  max-width: 100%;
  max-height: 70vh;
  border-radius: 6px;
  object-fit: contain;
}
.home-link { color: var(--el-color-primary); text-decoration: none; }
</style>
