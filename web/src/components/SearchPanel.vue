<script setup lang="ts">
// 搜索结果表：独立列语义（名称/来源/路径/操作），不再复用浏览表头。
import { useBrowseStore } from '../stores/browse'
import { downloadUrl } from '../api'
import { fileIcon } from '../utils/format'

const emit = defineEmits<{ navigate: [source: string, path: string] }>()
const browse = useBrowseStore()
</script>

<template>
  <el-card shadow="never">
    <template #header>
      搜索：「{{ browse.searchQuery }}」 的结果（{{ browse.searchHits.length }} 条，仅覆盖已索引/已同步的目录）
      <el-button link type="primary" style="margin-left: 12px" @click="browse.exitSearch()">返回浏览</el-button>
    </template>
    <el-table :data="browse.searchHits" v-loading="browse.searchLoading" empty-text="没有匹配的文件">
      <el-table-column label="名称" min-width="30%">
        <template #default="{ row }">
          <span class="icon">{{ row.is_dir ? '📁' : fileIcon(row.name) }}</span>{{ row.name }}
        </template>
      </el-table-column>
      <el-table-column prop="source_name" label="来源" width="160" />
      <el-table-column prop="path" label="路径" min-width="40%" show-overflow-tooltip />
      <el-table-column label="操作" width="100">
        <template #default="{ row }">
          <el-button v-if="row.is_dir" link type="primary" @click="emit('navigate', row.source_id, row.path)">进入</el-button>
          <el-button v-else link type="primary" tag="a" :href="downloadUrl(row.source_id, row.path)">下载</el-button>
        </template>
      </el-table-column>
    </el-table>
  </el-card>
</template>

<style scoped>
.icon { margin-right: 6px; }
</style>
