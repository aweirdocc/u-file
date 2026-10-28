// 界面偏好：亮/暗模式 + 主题色。持久化到 localStorage，启动时立即应用
// （main.ts 在 mount 前调用 init，配合 index.html 的防闪屏脚本）。
import { defineStore } from 'pinia'

export type ThemeMode = 'light' | 'dark'
export type AccentColor = 'blue' | 'cyan' | 'purple' | 'green' | 'orange'

const STORAGE_KEY = 'ufile_theme'

interface StoredTheme {
  mode: ThemeMode
  accent: AccentColor
}

function loadStored(): StoredTheme {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw) {
      const t = JSON.parse(raw)
      if ((t.mode === 'light' || t.mode === 'dark') && typeof t.accent === 'string') {
        return t as StoredTheme
      }
    }
  } catch { /* 损坏则用默认 */ }
  return { mode: 'light', accent: 'blue' }
}

export const ACCENTS: { value: AccentColor; label: string; color: string }[] = [
  { value: 'blue', label: '科技蓝', color: '#409eff' },
  { value: 'cyan', label: '清爽青', color: '#14b8a6' },
  { value: 'purple', label: '优雅紫', color: '#8b5cf6' },
  { value: 'green', label: '自然绿', color: '#22c55e' },
  { value: 'orange', label: '活力橙', color: '#f59e0b' },
]

export const useSettingsStore = defineStore('settings', {
  state: () => loadStored(),
  actions: {
    /** 应用到 <html>：dark 类 + data-accent 属性（theme.css 据此切变量） */
    apply() {
      const el = document.documentElement
      el.classList.toggle('dark', this.mode === 'dark')
      el.dataset.accent = this.accent
      el.style.colorScheme = this.mode
    },
    setMode(mode: ThemeMode) {
      this.mode = mode
      this.persist()
      this.apply()
    },
    toggleMode() {
      this.setMode(this.mode === 'dark' ? 'light' : 'dark')
    },
    setAccent(accent: AccentColor) {
      this.accent = accent
      this.persist()
      this.apply()
    },
    persist() {
      localStorage.setItem(STORAGE_KEY, JSON.stringify({ mode: this.mode, accent: this.accent }))
    },
  },
})
