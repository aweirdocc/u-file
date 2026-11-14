<script setup lang="ts">
// 主浏览视图：源标签 → 面包屑 → 同步状态 → 工具栏 → 文件表/搜索结果 → 分页。
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Plus } from '@element-plus/icons-vue'
import { useBrowseStore } from '../stores/browse'
import { useAuthStore } from '../stores/auth'
import SourceTabs from '../components/SourceTabs.vue'
import BreadcrumbBar from '../components/BreadcrumbBar.vue'
import SyncStatusBar from '../components/SyncStatusBar.vue'
import FileToolbar from '../components/FileToolbar.vue'
import FileTable from '../components/FileTable.vue'
import SearchPanel from '../components/SearchPanel.vue'
import PagerBar from '../components/PagerBar.vue'
import PreviewDialog from '../components/PreviewDialog.vue'
import SourceDialog from '../components/SourceDialog.vue'

const browse = useBrowseStore()
const auth = useAuthStore()
const router = useRouter()
const addDialogVisible = ref(false)

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
    <div v-if="browse.isSearching" class="card-panel">
      <SearchPanel @navigate="go" />
    </div>
    <template v-else-if="browse.sources.length && browse.currentSource">
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
    <div v-else class="card-panel empty-sources-panel">
      <el-empty
        v-if="!browse.sources.length"
        :description="auth.isAdmin ? '还没有接入任何文件源，请先添加一个文件源' : '管理员尚未配置文件源，请联系管理员添加'"
      >
        <el-button v-if="auth.isAdmin" type="primary" :icon="Plus" @click="addDialogVisible = true">
          新增文件源
        </el-button>
      </el-empty>
      <el-empty v-else description="当前文件源不存在或无权访问">
        <el-button type="primary" @click="go(browse.sources[0].id, '')">
          前往第一个文件源
        </el-button>
      </el-empty>
    </div>
  </main>
  <footer class="page-footer">u-file · 凭据已加密落库 · 操作记录审计日志</footer>
  <PreviewDialog v-model="previewVisible" />
  <SourceDialog v-model="addDialogVisible" @saved="(id) => go(id, '')" />
</template>

<style scoped>
.tabs-panel { padding: 8px 12px; margin-bottom: 12px; }
.controls-panel { padding: 12px; margin-bottom: 12px; }
.table-panel { padding: 4px 12px 12px; }
.empty-sources-panel { padding: 48px 16px; }
</style>
