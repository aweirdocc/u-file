<script setup lang="ts">
// 主浏览视图：源标签 → 面包屑 → 同步状态 → 工具栏 → 文件表/搜索结果 → 分页。
import { computed, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useBrowseStore } from '../stores/browse'
import SourceTabs from '../components/SourceTabs.vue'
import BreadcrumbBar from '../components/BreadcrumbBar.vue'
import SyncStatusBar from '../components/SyncStatusBar.vue'
import FileToolbar from '../components/FileToolbar.vue'
import FileTable from '../components/FileTable.vue'
import SearchPanel from '../components/SearchPanel.vue'
import PagerBar from '../components/PagerBar.vue'
import PreviewDialog from '../components/PreviewDialog.vue'

const browse = useBrowseStore()
const route = useRoute()
const router = useRouter()

onMounted(() => {
  if (!browse.sources.length) browse.loadSources().catch(() => {})
})

// 路由参数变化 → 同步 store（首屏/浏览器前进后退）
watch(
  () => [route.params.source, route.params.path] as const,
  ([s, p]) => {
    const source = (s as string) || ''
    const path = (p as string) || ''
    if (source !== browse.source || path !== browse.path) {
      browse.fromRoute(source, path)
      browse.loadSources().then(() => browse.loadEntries()).catch(() => {})
    }
  },
)

/** 组件内导航：改路由，watch 会驱动加载 */
function go(source: string, path: string) {
  router.push(source ? (path ? `/${source}/${path}` : `/${source}`) : '/')
}

/** 预览弹窗关闭时清空 store 的 previewEntry */
const previewVisible = computed({
  get: () => !!browse.previewEntry,
  set: (v) => { if (!v) browse.previewEntry = null },
})
</script>

<template>
  <main class="page-main">
    <SourceTabs @select="go" />
    <template v-if="!browse.isSearching">
      <BreadcrumbBar @navigate="go" @refresh="browse.refresh()" />
      <SyncStatusBar />
      <FileToolbar @refresh="browse.afterWrite()" />
    </template>
    <SearchPanel v-if="browse.isSearching" @navigate="go" />
    <template v-else>
      <FileTable @navigate="go" @preview="(e) => (browse.previewEntry = e)" />
      <PagerBar />
    </template>
  </main>
  <footer class="page-footer">u-file · 凭据已加密落库 · 操作记录审计日志</footer>
  <PreviewDialog v-model="previewVisible" />
</template>
