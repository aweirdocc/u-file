<script setup lang="ts">
import { ref } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { Search, User, UserFilled, SwitchButton, Plus, Download, Upload as UploadIcon, Share, Connection } from '@element-plus/icons-vue'
import { useAuthStore } from '../stores/auth'
import { useBrowseStore } from '../stores/browse'
import SourceDialog from './SourceDialog.vue'
import ExportDialog from './ExportDialog.vue'
import ImportDialog from './ImportDialog.vue'
import ThemeToggle from './ThemeToggle.vue'

const auth = useAuthStore()
const browse = useBrowseStore()
const router = useRouter()
const route = useRoute()

const searchText = ref('')
const searchMode = ref<'filename' | 'fulltext'>('filename')
const sourceDialogVisible = ref(false)

function onSearch() {
  const q = searchText.value.trim()
  if (q) {
    if (route.meta.standalone) router.push('/')
    browse.runSearch(q, searchMode.value)
  } else {
    browse.exitSearch()
  }
}

function openAddSource() {
  sourceDialogVisible.value = true
}

function onSourceSaved(id: string) {
  if (!route.meta.standalone && !browse.source) {
    router.push(`/${id}`)
  }
}

async function onLogout() {
  browse.reset()
  await auth.logout()
}

function onExport() { browse.exportDialogVisible = true }
function onImport() { browse.importDialogVisible = true }

</script>

<template>
  <el-header class="app-header" height="56px">
    <a class="brand" href="/" @click.prevent="router.push('/')">
      <span class="logo">🗂</span>
      <b>u-file</b>
      <el-tag size="small" type="primary" effect="dark">M4</el-tag>
      <span class="subtitle">统一文件管理服务平台</span>
    </a>

    <el-input
      v-model="searchText"
      class="search-box"
      :placeholder="searchMode === 'fulltext' ? '全文检索文档内容（回车）' : '搜索文件名（回车）'"
      :prefix-icon="Search"
      clearable
      @keydown.enter="onSearch"
      @clear="browse.exitSearch()"
    >
      <template #prepend>
        <el-select v-model="searchMode" style="width: 86px" @change="() => { if (searchText.trim()) onSearch() }">
          <el-option label="文件名" value="filename" />
          <el-option label="全文" value="fulltext" />
        </el-select>
      </template>
    </el-input>

    <div class="actions">
      <template v-if="auth.isLoggedIn">
        <el-dropdown trigger="hover">
          <span class="user-chip">
            <el-icon><User /></el-icon>
            {{ auth.user }}（{{ auth.roleLabel }}）
          </span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item v-if="auth.isAdmin" :icon="Connection" @click="router.push('/platform')">
                开放生态与挂载 (M4)
              </el-dropdown-item>
              <el-dropdown-item v-if="auth.isAdmin" :icon="UserFilled" @click="router.push('/users')">
                用户与权限管理
              </el-dropdown-item>
              <el-dropdown-item v-if="auth.isAdmin || auth.canShare" :icon="Share" @click="router.push('/shares')">
                分享外链管理
              </el-dropdown-item>
              <el-dropdown-item divided :icon="SwitchButton" @click="onLogout">退出登录</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </template>

      <ThemeToggle />

      <el-button
        v-if="auth.isAdmin"
        :type="route.path === '/platform' ? 'primary' : 'default'"
        :plain="route.path === '/platform'"
        :icon="Connection"
        @click="router.push('/platform')"
      >
        开放与挂载
      </el-button>

      <el-button
        v-if="auth.isAdmin"
        :type="route.path === '/users' ? 'primary' : 'default'"
        :plain="route.path === '/users'"
        :icon="UserFilled"
        @click="router.push('/users')"
      >
        用户管理
      </el-button>

      <el-button
        v-if="auth.isAdmin || auth.canShare"
        :type="route.path === '/shares' ? 'primary' : 'default'"
        :plain="route.path === '/shares'"
        :icon="Share"
        @click="router.push('/shares')"
      >
        分享管理
      </el-button>

      <template v-if="auth.isAdmin">
        <el-button :icon="UploadIcon" @click="onImport">导入配置</el-button>
        <el-button :icon="Download" @click="onExport">导出配置</el-button>
        <el-button type="primary" :icon="Plus" @click="openAddSource">新增文件源</el-button>
      </template>
    </div>
  </el-header>

  <SourceDialog v-model="sourceDialogVisible" @saved="onSourceSaved" />
  <ExportDialog v-model="browse.exportDialogVisible" />
  <ImportDialog v-model="browse.importDialogVisible" />
</template>

<style scoped>
.app-header {
  background: var(--ufile-header-bg);
  color: var(--ufile-header-text);
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 0 24px;
}
.brand {
  display: flex;
  align-items: center;
  gap: 8px;
  color: inherit;
  text-decoration: none;
  font-size: 16px;
  white-space: nowrap;
}
.logo { font-size: 20px; }
.subtitle { color: var(--ufile-header-muted); font-size: 12px; margin-left: 4px; }
.search-box { max-width: 360px; margin-left: 16px; }
.actions { margin-left: auto; display: flex; align-items: center; gap: 8px; }
.user-chip {
  color: var(--ufile-header-chip);
  font-size: 13px;
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 4px 8px;
}
</style>
