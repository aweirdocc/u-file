<script setup lang="ts">
// 面包屑 + 强制刷新（绕过索引缓存直读源）。
import { Refresh } from '@element-plus/icons-vue'
import { useBrowseStore } from '../stores/browse'

const emit = defineEmits<{ navigate: [source: string, path: string]; refresh: [] }>()
const browse = useBrowseStore()
</script>

<template>
  <div class="crumb-bar">
    <el-breadcrumb separator="/">
      <el-breadcrumb-item>
        <a class="crumb-link" @click.prevent="emit('navigate', browse.source, '')">{{ browse.source }}</a>
      </el-breadcrumb-item>
      <el-breadcrumb-item v-for="c in browse.breadcrumbs.slice(1)" :key="c.path">
        <a class="crumb-link" @click.prevent="emit('navigate', browse.source, c.path)">{{ c.name }}</a>
      </el-breadcrumb-item>
    </el-breadcrumb>
    <el-tooltip content="绕过缓存，直接读源" placement="top">
      <el-button link :icon="Refresh" class="refresh-btn" @click="emit('refresh')" />
    </el-tooltip>
  </div>
</template>

<style scoped>
.crumb-bar { display: flex; align-items: center; gap: 8px; margin-bottom: 10px; min-height: 24px; }
.crumb-link { cursor: pointer; font-size: 13px; }
.refresh-btn { padding: 2px; }
</style>
