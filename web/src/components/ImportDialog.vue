<script setup lang="ts">
// 导入配置弹窗：文件选择/粘贴 + 覆盖开关 + 逐条导入结果。
import { ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { useBrowseStore } from '../stores/browse'
import { importSources } from '../api'
import type { ImportReport } from '../api/types'

const visible = defineModel<boolean>({ default: false })
const browse = useBrowseStore()

const text = ref('')
const overwrite = ref(false)
const running = ref(false)
const report = ref<ImportReport | null>(null)

watch(visible, (v) => {
  if (v) { text.value = ''; overwrite.value = false; report.value = null }
})

function onFileChosen(ev: Event) {
  const input = ev.target as HTMLInputElement
  const f = input.files?.[0]
  if (!f) return
  const reader = new FileReader()
  reader.onload = () => { text.value = String(reader.result || '') }
  reader.readAsText(f)
}

async function run() {
  if (!text.value.trim()) { ElMessage.warning('请选择文件或粘贴 JSON'); return }
  running.value = true
  try {
    report.value = await importSources(text.value, overwrite.value)
    await browse.loadSources()
    // 当前源被覆盖/删除的边界：仍在列表则不动，否则回到无源状态
    if (browse.source && !browse.sources.find(s => s.id === browse.source)) {
      browse.fromRoute('', '')
    }
  } catch (e: any) {
    ElMessage.error('导入失败：' + e.message)
  } finally {
    running.value = false
  }
}

const statusIcon: Record<string, string> = { added: '✅', updated: '🔄', skipped: '⏭', failed: '❌' }
const statusText: Record<string, string> = { added: '已新增', updated: '已覆盖更新', skipped: '已跳过', failed: '失败' }
</script>

<template>
  <el-dialog v-model="visible" title="导入文件源配置" width="560px">
    <label class="field-label">选择导出文件（ufile-sources-*.json）</label>
    <input type="file" accept=".json,application/json" @change="onFileChosen">
    <div class="or-divider">—— 或直接粘贴 JSON ——</div>
    <el-input
      v-model="text"
      type="textarea"
      :rows="7"
      placeholder='支持两种格式：完整导出文件（{"version":1,...}）或源数组（[{"id":...,"type":...,"config":{...}}]）'
    />
    <el-checkbox v-model="overwrite" style="margin-top: 10px">覆盖同 ID 的已有源（不勾则跳过）</el-checkbox>

    <div v-if="report" class="report">
      <div class="summary">
        完成：新增 {{ report.added }}，更新 {{ report.updated }}，跳过 {{ report.skipped }}，失败 {{ report.failed }}
      </div>
      <div class="items">
        <div v-for="item in report.items" :key="item.id" class="item">
          {{ statusIcon[item.status] }} <b>{{ item.id }}</b>
          {{ item.status === 'failed' ? '失败：' + (item.error || '') : statusText[item.status] }}
        </div>
      </div>
      <el-alert
        v-if="report.redacted_count"
        type="warning"
        :closable="false"
        show-icon
        :title="`其中 ${report.redacted_count} 条含脱敏凭据（******），连接测试会失败，请编辑补填真实凭据。`"
      />
    </div>

    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="running" @click="run">开始导入</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.field-label { font-size: 13px; color: var(--el-text-color-secondary); display: block; margin-bottom: 4px; }
.or-divider { text-align: center; color: var(--el-text-color-secondary); font-size: 12px; margin: 6px 0; }
.report { margin-top: 12px; }
.summary { font-size: 13px; margin-bottom: 6px; }
.items { max-height: 180px; overflow: auto; font-size: 12.5px; margin-bottom: 8px; }
.item { line-height: 1.8; }
</style>
