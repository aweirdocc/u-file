<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import AppHeader from './components/AppHeader.vue'
import TaskCenterFloat from './components/TaskCenterFloat.vue'

const route = useRoute()
// 公开分享页/登录页不显示平台 header（各自自带布局）
const hideHeader = computed(() => !!route.meta.standalone)
const hideTaskFloat = computed(() => !!route.meta.public)
</script>

<template>
  <el-config-provider>
    <div class="app-shell">
      <AppHeader v-if="!hideHeader" />
      <router-view />
      <TaskCenterFloat v-if="!hideTaskFloat" />
    </div>
  </el-config-provider>
</template>

<style>
* { box-sizing: border-box; }
body {
  margin: 0;
  font: 14px/1.6 -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif;
  background: var(--ufile-body-bg);
  color: var(--el-text-color-primary);
}
.app-shell { min-height: 100vh; display: flex; flex-direction: column; }
.page-main {
  flex: 1;
  width: 100%;
  max-width: 1200px;
  margin: 0 auto;
  padding: 20px 16px;
}
.page-footer {
  text-align: center;
  color: var(--el-text-color-secondary);
  font-size: 12px;
  margin: 20px 0;
}
/* 统一卡片面板：浏览页各区块/管理页表格共用 */
.card-panel {
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ufile-radius);
  box-shadow: var(--ufile-card-shadow);
  padding: 12px;
}
.card-panel + .card-panel { margin-top: 12px; }
/* Markdown 预览排版 */
.md-body h1, .md-body h2, .md-body h3, .md-body h4 { border-bottom: 1px solid var(--el-border-color-lighter); padding-bottom: 4px; }
.md-body pre { background: var(--el-fill-color); border-radius: 6px; padding: 12px; overflow: auto; }
.md-body code { background: var(--el-fill-color); border-radius: 4px; padding: 1px 5px; }
.md-body pre code { background: transparent; padding: 0; }
.md-body blockquote { border-left: 3px solid var(--el-border-color); margin: 8px 0; padding: 2px 12px; color: var(--el-text-color-secondary); }
.md-body table { border-collapse: collapse; }
.md-body td, .md-body th { border: 1px solid var(--el-border-color-lighter); padding: 4px 10px; }
</style>
