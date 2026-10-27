<script setup lang="ts">
// 新增 / 编辑文件源弹窗。编辑时 ID/类型锁定；凭据回显打码值（保持 ******
// 表示不改）；保存必须先「测试连接」通过。
import { ref, watch, computed } from 'vue'
import { ElMessage } from 'element-plus'
import { useBrowseStore } from '../stores/browse'
import { getSource, addSource, updateSource, testSource } from '../api'
import type { TestResult } from '../api/types'

const props = defineProps<{ editId?: string | null }>()
const visible = defineModel<boolean>({ default: false })
const emit = defineEmits<{ saved: [id: string] }>()

const browse = useBrowseStore()

const isEdit = computed(() => !!props.editId)
const form = ref({
  id: '',
  name: '',
  type: 's3',
  root: '',
  endpoint: '',
  region: '',
  bucket: '',
  prefix: '',
  accessKey: '',
  secretKey: '',
  pathStyle: false,
})
const testing = ref(false)
const testResult = ref<TestResult | null>(null)
const saving = ref(false)

function resetForm() {
  form.value = { id: '', name: '', type: 's3', root: '', endpoint: '', region: '', bucket: '', prefix: '', accessKey: '', secretKey: '', pathStyle: false }
  testResult.value = null
}

watch(visible, async (v) => {
  if (!v) return
  resetForm()
  if (props.editId) {
    try {
      const detail = await getSource(props.editId)
      form.value.id = detail.id
      form.value.name = detail.name === detail.id ? '' : detail.name
      form.value.type = detail.type
      const c = (detail.config || {}) as Record<string, any>
      if (detail.type === 'local') form.value.root = String(c.root || '')
      else {
        form.value.endpoint = String(c.endpoint || '')
        form.value.region = String(c.region || '')
        form.value.bucket = String(c.bucket || '')
        form.value.prefix = String(c.prefix || '')
        form.value.accessKey = String(c.access_key || '')
        form.value.secretKey = String(c.secret_key || '')
        form.value.pathStyle = !!c.use_path_style
      }
    } catch (e: any) {
      ElMessage.error('加载源配置失败：' + e.message)
      visible.value = false
    }
  }
})

/** 由表单构造提交的 config；空值键省略（与后端/旧 UI 语义一致） */
function curConfig(): Record<string, unknown> {
  if (form.value.type === 'local') return { root: form.value.root.trim() }
  const cfg: Record<string, unknown> = {
    endpoint: form.value.endpoint.trim(),
    region: form.value.region.trim() || 'us-east-1',
    bucket: form.value.bucket.trim(),
    use_path_style: form.value.pathStyle,
  }
  if (form.value.prefix.trim()) cfg.prefix = form.value.prefix.trim()
  if (form.value.accessKey.trim()) cfg.access_key = form.value.accessKey.trim()
  if (form.value.secretKey) cfg.secret_key = form.value.secretKey
  return cfg
}

function onFieldInput() {
  testResult.value = null // 配置变更后需重测
}

async function onTest() {
  testing.value = true
  testResult.value = null
  try {
    testResult.value = await testSource(form.value.type, curConfig())
  } catch (e: any) {
    testResult.value = { ok: false, latency_ms: 0, error: e.message }
  } finally {
    testing.value = false
  }
}

async function onSave() {
  if (!isEdit.value && !form.value.id.trim()) {
    ElMessage.warning('请填写源 ID')
    return
  }
  saving.value = true
  try {
    if (isEdit.value && props.editId) {
      await updateSource(props.editId, form.value.name.trim(), curConfig())
      ElMessage.success('已保存')
    } else {
      await addSource(form.value.id.trim(), form.value.name.trim(), form.value.type, curConfig())
      ElMessage.success('已创建')
    }
    visible.value = false
    await browse.loadSources()
    emit('saved', isEdit.value ? props.editId! : form.value.id.trim())
  } catch (e: any) {
    ElMessage.error('保存失败：' + e.message)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <el-dialog
    v-model="visible"
    :title="isEdit ? `编辑文件源：${editId}` : '新增文件源'"
    width="560px"
  >
    <el-form label-position="top">
      <el-row :gutter="12">
        <el-col :span="12">
          <el-form-item label="源 ID（唯一，字母数字-_）">
            <el-input v-model="form.id" placeholder="r2-demo" :disabled="isEdit" />
          </el-form-item>
        </el-col>
        <el-col :span="12">
          <el-form-item label="显示名称（可留空）">
            <el-input v-model="form.name" placeholder="我的 R2" />
          </el-form-item>
        </el-col>
      </el-row>

      <el-form-item label="类型">
        <el-select v-model="form.type" :disabled="isEdit" style="width: 100%" @change="onFieldInput">
          <el-option value="s3" label="s3 —— S3 协议族（S3/MinIO/R2/OSS/COS/OBS/Kodo/BOS）" />
          <el-option value="local" label="local —— 本地磁盘" />
        </el-select>
      </el-form-item>

      <!-- local 字段 -->
      <template v-if="form.type === 'local'">
        <el-form-item label="根目录">
          <el-input v-model="form.root" placeholder="D:\data 或 ./data/files" @input="onFieldInput" />
        </el-form-item>
      </template>

      <!-- s3 字段 -->
      <template v-else>
        <el-row :gutter="12">
          <el-col :span="14">
            <el-form-item label="Endpoint">
              <el-input v-model="form.endpoint" placeholder="https://<ACCOUNT_ID>.r2.cloudflarestorage.com" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="10">
            <el-form-item label="Region">
              <el-input v-model="form.region" placeholder="auto / us-east-1 / cn-hangzhou" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="Bucket">
              <el-input v-model="form.bucket" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="前缀（可选）">
              <el-input v-model="form.prefix" placeholder="备份/2024" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="Access Key">
              <el-input v-model="form.accessKey" autocomplete="off" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="Secret Key">
              <el-input v-model="form.secretKey" type="password" show-password autocomplete="new-password" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-checkbox v-model="form.pathStyle" @change="onFieldInput">
          使用 path-style（MinIO / BOS / Kodo 勾选；OSS / COS 新桶不勾）
        </el-checkbox>
        <div class="endpoint-hint">
          常见 endpoint：R2 <code>https://&lt;ACCOUNT_ID&gt;.r2.cloudflarestorage.com</code>(region=auto) ·
          OSS <code>s3.oss-{region}.aliyuncs.com</code> · COS <code>cos.&lt;region&gt;.myqcloud.com</code>(桶名含 -APPID)
        </div>
      </template>
    </el-form>

    <template #footer>
      <div class="footer-bar">
        <div class="test-area">
          <el-button :loading="testing" @click="onTest">测试连接</el-button>
          <span v-if="testResult?.ok" class="test-ok">✓ 连接成功（{{ testResult.latency_ms }} ms）</span>
          <span v-else-if="testResult" class="test-bad">✗ {{ testResult.error || '连接失败' }}</span>
        </div>
        <div>
          <el-button @click="visible = false">取消</el-button>
          <el-button type="primary" :disabled="!testResult?.ok" :loading="saving" @click="onSave">保存</el-button>
        </div>
      </div>
      <div class="hint">建议先「测试连接」，通过后保存（保存后立即可用，无需重启）。</div>
    </template>
  </el-dialog>
</template>

<style scoped>
.footer-bar { display: flex; justify-content: space-between; align-items: center; }
.test-area { display: flex; align-items: center; gap: 8px; font-size: 13px; }
.test-ok { color: var(--el-color-success); }
.test-bad { color: var(--el-color-danger); }
.endpoint-hint { font-size: 12px; color: var(--el-text-color-secondary); margin-top: 6px; line-height: 1.7; }
.hint { font-size: 12px; color: var(--el-text-color-secondary); margin-top: 8px; }
</style>
