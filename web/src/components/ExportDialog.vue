<script setup lang="ts">
// 导出配置弹窗：脱敏开关（未勾选时显示明文警告）+ 下载 / 复制。
import { ref, watch } from 'vue'
import { exportUrl, exportText } from '../api'
import { downloadHref } from '../utils/format'

const visible = defineModel<boolean>({ default: false })

const redact = ref(true) // 安全默认：脱敏导出；取消勾选才导出明文凭据
const msg = ref('')

watch(visible, (v) => { if (v) msg.value = '' })

function onDownload() {
  downloadHref(exportUrl(redact.value))
  msg.value = '已开始下载。'
}

async function onCopy() {
  try {
    const text = await exportText(redact.value)
    await navigator.clipboard.writeText(text)
    msg.value = '已复制到剪贴板。'
  } catch (e: any) {
    msg.value = '复制失败：' + e.message
  }
}
</script>

<template>
  <el-dialog v-model="visible" title="导出文件源配置" width="480px">
    <el-checkbox v-model="redact">脱敏导出（凭据替换为 ******，可作为模板分享/入库）</el-checkbox>
    <el-alert
      v-if="!redact"
      type="warning"
      :closable="false"
      show-icon
      style="margin-top: 10px"
      title="未脱敏导出包含明文凭据，请妥善保管导出文件，勿提交到版本库或发给他人。"
    />
    <template #footer>
      <span class="msg">{{ msg }}</span>
      <el-button @click="onCopy">复制 JSON</el-button>
      <el-button type="primary" @click="onDownload">下载 JSON</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.msg { color: var(--el-text-color-secondary); font-size: 12px; margin-right: 10px; }
</style>
