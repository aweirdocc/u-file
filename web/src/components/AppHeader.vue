<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { Search, User, SwitchButton, Plus, Download, Upload as UploadIcon, Share } from '@element-plus/icons-vue'
import { useAuthStore } from '../stores/auth'
import { useBrowseStore } from '../stores/browse'
import { downloadHref, exportUrl } from '../api/exportHelpers'
import SourceDialog from './SourceDialog.vue'
import ExportDialog from './ExportDialog.vue'
import ImportDialog from './ImportDialog.vue'
import ThemeToggle from './ThemeToggle.vue'

const auth = useAuthStore()
const browse = useBrowseStore()
const router = useRouter()

const searchText = ref('')
const sourceDialogVisible = ref(false)

function onSearch() {
  const q = searchText.value.trim()
  if (q) browse.runSearch(q)
  else browse.exitSearch()
}

function openAddSource() {
  sourceDialogVisible.value = true
}

async function onLogout() {
  browse.reset()
  await auth.logout()
}

function onExport() { browse.exportDialogVisible = true }
function onImport() { browse.importDialogVisible = true }

// 导出下载（带 token 的 JSON 附件由后端 Content-Disposition 决定；此处原生导航走 cookie）
function doExportDownload(redact: boolean) {
  downloadHref(exportUrl(redact))
}
</script>

<template>
  <el-header class="app-header" height="56px">
    <a class="brand" href="/">
      <span class="logo">🗂</span>
      <b>u-file</b>
      <el-tag size="small" type="info" effect="dark">M3</el-tag>
      <span class="subtitle">统一文件管理服务平台</span>
    </a>

    <el-input
      v-model="searchText"
      class="search-box"
      placeholder="搜索文件名（回车）"
      :prefix-icon="Search"
      clearable
      @keydown.enter="onSearch"
      @clear="browse.exitSearch()"
    />

    <div class="actions">
      <template v-if="auth.isLoggedIn">
        <el-dropdown trigger="hover">
          <span class="user-chip">
            <el-icon><User /></el-icon>
            {{ auth.user }}（{{ auth.roleLabel }}）
          </span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item :icon="SwitchButton" @click="onLogout">退出登录</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </template>

      <ThemeToggle />

      <template v-if="auth.isAdmin">
        <el-button :icon="Share" @click="router.push('/shares')">分享管理</el-button>
        <el-button :icon="UploadIcon" @click="onImport">导入配置</el-button>
        <el-button :icon="Download" @click="onExport">导出配置</el-button>
        <el-button type="primary" :icon="Plus" @click="openAddSource">新增文件源</el-button>
      </template>
    </div>
  </el-header>

  <SourceDialog v-model="sourceDialogVisible" />
  <ExportDialog v-model="browse.exportDialogVisible" @download="doExportDownload" />
  <ImportDialog v-model="browse.importDialogVisible" />
</template>

<style scoped>
.app-header {
  background: var(--ufile-header-bg);
  color: var(--ufile-header-text);
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 0 20px;
}
.brand { display: flex; align-items: center; gap: 8px; font-size: 16px; color: inherit; text-decoration: none; }
.subtitle { opacity: .55; font-size: 13px; }
.search-box { width: 240px; margin-left: auto; }
.actions { display: flex; align-items: center; gap: 4px; }
.user-chip {
  display: inline-flex; align-items: center; gap: 6px;
  color: var(--ufile-header-text); cursor: pointer; font-size: 13px; outline: none;
  padding: 4px 8px; border-radius: 6px;
}
.user-chip:hover { background: var(--ufile-header-hover); }
</style>
