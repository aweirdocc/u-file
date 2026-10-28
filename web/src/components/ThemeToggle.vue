<script setup lang="ts">
// 主题切换：亮/暗切换按钮 + 主题色圆点下拉。
import { computed } from 'vue'
import { Moon, Sunny } from '@element-plus/icons-vue'
import { useSettingsStore, ACCENTS } from '../stores/settings'

const settings = useSettingsStore()
const isDark = computed(() => settings.mode === 'dark')
</script>

<template>
  <div class="theme-toggle">
    <el-dropdown trigger="click" popper-class="accent-popper">
      <button class="accent-btn" title="主题色" aria-label="选择主题色">
        <span
          class="accent-dot"
          :style="{ background: ACCENTS.find(a => a.value === settings.accent)?.color }"
        />
      </button>
      <template #dropdown>
        <div class="accent-panel">
          <div class="panel-title">主题色</div>
          <div class="accent-list">
            <button
              v-for="a in ACCENTS"
              :key="a.value"
              class="accent-item"
              :class="{ active: settings.accent === a.value }"
              :title="a.label"
              @click="settings.setAccent(a.value)"
            >
              <span class="dot" :style="{ background: a.color }" />
              <span class="label">{{ a.label }}</span>
              <span v-if="settings.accent === a.value" class="check">✓</span>
            </button>
          </div>
        </div>
      </template>
    </el-dropdown>

    <button class="mode-btn" :title="isDark ? '切换为亮色' : '切换为暗色'" @click="settings.toggleMode()">
      <el-icon :size="16" color="#fff"><Moon v-if="!isDark" /><Sunny v-else /></el-icon>
    </button>
  </div>
</template>

<style scoped>
.theme-toggle { display: flex; align-items: center; gap: 4px; }
.accent-btn, .mode-btn {
  display: inline-flex; align-items: center; justify-content: center;
  width: 30px; height: 30px; border: none; border-radius: 8px;
  cursor: pointer; background: transparent; outline: none;
}
.accent-btn:hover, .mode-btn:hover { background: var(--ufile-header-hover); }
.accent-dot { width: 14px; height: 14px; border-radius: 50%; box-shadow: 0 0 0 2px rgba(255,255,255,.25); }
.accent-panel { padding: 8px; min-width: 140px; }
.panel-title { font-size: 12px; color: var(--el-text-color-secondary); padding: 2px 8px 6px; }
.accent-list { display: flex; flex-direction: column; gap: 2px; }
.accent-item {
  display: flex; align-items: center; gap: 8px; width: 100%;
  padding: 6px 8px; border: none; border-radius: 6px; cursor: pointer;
  background: transparent; font-size: 13px; color: var(--el-text-color-primary); text-align: left;
}
.accent-item:hover { background: var(--el-fill-color); }
.accent-item.active { background: var(--el-fill-color-dark); }
.accent-item .dot { width: 12px; height: 12px; border-radius: 50%; flex: none; }
.accent-item .label { flex: 1; }
.accent-item .check { color: var(--el-color-primary); font-size: 12px; }
</style>
