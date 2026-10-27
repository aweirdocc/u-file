<script setup lang="ts">
// 游标分页栏：仅当源支持游标（S3 族）且翻过页或还有下一页时显示。
import { computed } from 'vue'
import { ArrowLeft, ArrowRight } from '@element-plus/icons-vue'
import { useBrowseStore } from '../stores/browse'

const browse = useBrowseStore()
const visible = computed(() => browse.hasCursor && (browse.curNextCursor !== '' || browse.pageStack.length > 1))
</script>

<template>
  <div v-if="visible" class="pager">
    <el-button :icon="ArrowLeft" :disabled="browse.pageStack.length <= 1" @click="browse.prevPage()">上一页</el-button>
    <span class="info">第 {{ browse.pageNum }} 页 · 本页 {{ browse.entries.length }} 条</span>
    <el-button :disabled="!browse.curNextCursor" @click="browse.nextPage()">
      下一页<el-icon style="margin-left: 4px"><ArrowRight /></el-icon>
    </el-button>
  </div>
</template>

<style scoped>
.pager { display: flex; align-items: center; gap: 14px; padding: 12px 4px 0; }
.info { color: var(--el-text-color-secondary); font-size: 13px; }
</style>
