<script setup lang="ts">
// 主浏览视图：源标签 → 面包屑 → 同步状态 → 工具栏 → 文件表/搜索结果 → 分页。
import { computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
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
const router = useRouter()

onMounted(() => {
  if (!browse.sources.length) browse.loadSources().catch(() => {})
})

// 路由 ↔ store 的同步统一由全局守卫负责（router/index.ts）：
// 从分享页等 standalone 路由返回时本视图会重新挂载，视图内 watcher 不可靠。

/** 组件内导航：改路由，守卫会驱动加载 */
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
    <div class="card-panel tabs-panel">
      <SourceTabs @select="go" />
    </div>
    <template v-if="!browse.isSearching">
      <div class="card-panel controls-panel">
        <BreadcrumbBar @navigate="go" @refresh="browse.refresh()" />
        <SyncStatusBar />
        <FileToolbar @refresh="browse.afterWrite()" />
      </div>
      <div class="card-panel table-panel">
        <FileTable @navigate="go" @preview="(e) => (browse.previewEntry = e)" />
        <PagerBar />
      </div>
    </template>
    <div v-else class="card-panel">
      <SearchPanel @navigate="go" />
    </div>
  </main>
  <footer class="page-footer">u-file · 凭据已加密落库 · 操作记录审计日志</footer>
  <PreviewDialog v-model="previewVisible" />
</template>

<style scoped>
.tabs-panel { padding: 8px 12px; margin-bottom: 12px; }
.controls-panel { padding: 12px; margin-bottom: 12px; }
.table-panel { padding: 4px 12px 12px; }
</style>
