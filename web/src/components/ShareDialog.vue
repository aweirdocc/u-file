<script setup lang="ts">
// 创建分享外链弹窗：有效期/提取码/次数限制 → 创建成功展示链接与提取码供复制。
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { createShare } from '../api'
import type { ShareInfo } from '../api/types'
import { fmtSize } from '../utils/format'

export interface ShareTarget {
  sourceId: string
  path: string
  name: string
  size: number
  isDir: boolean
}

const visible = defineModel<boolean>({ default: false })
const emit = defineEmits<{ created: [share: ShareInfo] }>()

const target = ref<ShareTarget | null>(null)
const ttlHours = ref(168)
const usePassword = ref(false)
const password = ref('')
const maxDownloadsEnabled = ref(false)
const maxDownloads = ref(10)
const creating = ref(false)
const created = ref<ShareInfo | null>(null)

const ttlOptions = [
  { value: 1, label: '1 小时' },
  { value: 24, label: '24 小时' },
  { value: 168, label: '7 天' },
  { value: 720, label: '30 天' },
]

const pageUrl = computed(() =>
  created.value ? `${location.origin}/s/${created.value.token}` : '',
)

watch(visible, (v) => {
  if (!v) return
  created.value = null
  ttlHours.value = 168
  usePassword.value = false
  password.value = ''
  maxDownloadsEnabled.value = false
  maxDownloads.value = 10
})

/** 打开弹窗（由父组件在点"分享"时调用） */
function open(t: ShareTarget) {
  target.value = t
  visible.value = true
}
defineExpose({ open })

function genPassword() {
  // 浏览器 crypto：6 位无易混字符
  const chars = 'abcdefghjkmnpqrstuvwxyz23456789'
  const buf = new Uint32Array(6)
  crypto.getRandomValues(buf)
  password.value = Array.from(buf, n => chars[n % chars.length]).join('')
}

async function onCreate() {
  if (usePassword.value && password.value.trim().length < 4) {
    ElMessage.warning('提取码至少 4 个字符')
    return
  }
  if (!target.value) return
  creating.value = true
  try {
    created.value = await createShare(
      target.value.sourceId,
      target.value.path,
      ttlHours.value,
      usePassword.value ? password.value.trim() : '',
      maxDownloadsEnabled.value ? Math.max(1, Math.floor(maxDownloads.value) || 1) : 0,
    )
    emit('created', created.value)
  } catch (e: any) {
    ElMessage.error('创建分享失败：' + e.message)
  } finally {
    creating.value = false
  }
}

async function copy(text: string, tip: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success(tip)
  } catch {
    ElMessage.warning('复制失败，请手动选择复制')
  }
}
</script>

<template>
  <el-dialog v-model="visible" title="分享外链" width="520px">
    <!-- 创建前：表单 -->
    <template v-if="!created">
      <div class="target-line">
        <span class="icon">{{ target?.isDir ? '📁' : '📄' }}</span>
        <b>{{ target?.name }}</b>
        <span v-if="!target?.isDir" class="muted">{{ fmtSize(target?.size ?? 0) }}</span>
        <span class="muted">{{ target?.sourceId }}:{{ target?.path }}</span>
      </div>

      <el-form label-position="top">
        <el-form-item label="有效期">
          <el-select v-model="ttlHours" style="width: 100%">
            <el-option v-for="o in ttlOptions" :key="o.value" :value="o.value" :label="o.label" />
          </el-select>
        </el-form-item>

        <el-form-item>
          <el-checkbox v-model="usePassword">提取码</el-checkbox>
          <template v-if="usePassword">
            <div class="pwd-row">
              <el-input v-model="password" placeholder="4~64 字符" style="width: 200px" />
              <el-button @click="genPassword">随机生成</el-button>
            </div>
          </template>
        </el-form-item>

        <el-form-item>
          <el-checkbox v-model="maxDownloadsEnabled">限制下载次数</el-checkbox>
          <el-input-number v-if="maxDownloadsEnabled" v-model="maxDownloads" :min="1" :max="100000" style="margin-left: 12px" />
        </el-form-item>
      </el-form>

      <div class="hint">访客通过链接即可下载，无需登录；目录分享将打包为 zip。</div>
    </template>

    <!-- 创建后：链接展示 -->
    <template v-else>
      <el-result icon="success" title="分享链接已创建">
        <template #sub-title>
          <div class="result-lines">
            <div class="copy-row">
              <span class="label">链接</span>
              <el-link type="primary" :href="pageUrl" target="_blank">{{ pageUrl }}</el-link>
              <el-button size="small" @click="copy(pageUrl, '链接已复制')">复制</el-button>
            </div>
            <div v-if="created.has_password" class="copy-row">
              <span class="label">提取码</span>
              <code class="pwd">{{ password }}</code>
              <el-button size="small" @click="copy(password, '提取码已复制')">复制</el-button>
            </div>
            <div class="muted small">
              有效期至 {{ new Date(created.expires_at).toLocaleString() }}
              <template v-if="created.max_downloads"> · 限 {{ created.max_downloads }} 次下载</template>
            </div>
          </div>
        </template>
      </el-result>
    </template>

    <template #footer>
      <template v-if="!created">
        <el-button @click="visible = false">取消</el-button>
        <el-button type="primary" :loading="creating" @click="onCreate">创建链接</el-button>
      </template>
      <template v-else>
        <el-button type="primary" @click="visible = false">完成</el-button>
      </template>
    </template>
  </el-dialog>
</template>

<style scoped>
.target-line { display: flex; align-items: center; gap: 8px; margin-bottom: 12px; flex-wrap: wrap; }
.target-line .muted { font-size: 12px; color: var(--el-text-color-secondary); }
.pwd-row { display: flex; gap: 8px; margin-top: 4px; }
.hint { font-size: 12px; color: var(--el-text-color-secondary); }
.result-lines { text-align: left; display: inline-block; }
.copy-row { display: flex; align-items: center; gap: 8px; margin: 6px 0; }
.copy-row .label { width: 48px; color: var(--el-text-color-secondary); font-size: 13px; }
.pwd { font-size: 15px; letter-spacing: 1px; }
.small { font-size: 12px; }
.muted { color: var(--el-text-color-secondary); }
</style>
