<script setup lang="ts">
// 搜索结果表：支持「文件名搜索」与「M4.3 全文内容检索（含正文摘要 Snippet）」。
import { useBrowseStore } from '../stores/browse'
import { downloadUrl } from '../api'
import { fileIcon } from '../utils/format'

const emit = defineEmits<{ navigate: [source: string, path: string] }>()
const browse = useBrowseStore()

function switchMode(mode: 'filename' | 'fulltext') {
  if (browse.searchMode === mode) return
  browse.runSearch(browse.searchQuery, mode)
}
</script>

<template>
  <el-card shadow="never">
    <template #header>
      <div class="search-hdr">
        <div>
          搜索：「<b>{{ browse.searchQuery }}</b>」 的结果（{{ browse.searchHits.length }} 条）
          <el-radio-group
            :model-value="browse.searchMode"
            size="small"
            style="margin-left: 12px"
            @change="(val: any) => switchMode(val)"
          >
            <el-radio-button value="filename">文件名匹配</el-radio-button>
            <el-radio-button value="fulltext">全文内容检索</el-radio-button>
          </el-radio-group>
        </div>
        <el-button link type="primary" @click="browse.exitSearch()">返回浏览</el-button>
      </div>
    </template>
    <el-table :data="browse.searchHits" v-loading="browse.searchLoading" empty-text="没有匹配的文件">
      <el-table-column label="名称与正文摘要" min-width="42%">
        <template #default="{ row }">
          <div>
            <span class="icon">{{ row.is_dir ? '📁' : fileIcon(row.name) }}</span>
            <b>{{ row.name }}</b>
          </div>
          <div v-if="row.snippet" class="snippet-line">
            正文摘要：{{ row.snippet }}
          </div>
        </template>
      </el-table-column>
      <el-table-column prop="source_name" label="来源" width="150" />
      <el-table-column prop="path" label="路径" min-width="32%" show-overflow-tooltip />
      <el-table-column label="操作" width="110">
        <template #default="{ row }">
          <el-button v-if="row.is_dir" link type="primary" @click="emit('navigate', row.source_id, row.path)">进入</el-button>
          <el-button v-else link type="primary" tag="a" :href="downloadUrl(row.source_id, row.path)">下载</el-button>
        </template>
      </el-table-column>
    </el-table>
  </el-card>
</template>

<style scoped>
.search-hdr {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.icon { margin-right: 6px; }
.snippet-line {
  margin-top: 4px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
  background: var(--el-fill-color-lighter);
  padding: 3px 8px;
  border-radius: 4px;
  border-left: 2px solid var(--el-color-primary);
}
</style>
