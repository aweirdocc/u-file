<script setup lang="ts">
// 新增 / 编辑文件源弹窗。支持 7 类源：s3 / local / webdav / gdrive / alipan / ftp / sftp。
// 编辑时 ID/类型锁定；凭据回显打码值（保持 ****** 表示不改）；保存必须先「测试连接」通过。
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

function defaultForm() {
  return {
    id: '',
    name: '',
    type: 's3',
    // local / 通用子目录
    root: '',
    // s3
    endpoint: '',
    region: '',
    bucket: '',
    prefix: '',
    accessKey: '',
    secretKey: '',
    pathStyle: false,
    // webdav / ftp / sftp 公共认证与网络
    host: '',
    port: 21,
    username: '',
    password: '',
    tls: false,
    insecureTls: false,
    privateKey: '',
    privateKeyPassphrase: '',
    proxyUrl: '',
    // gdrive
    authType: 'oauth2',
    clientId: '',
    clientSecret: '',
    refreshToken: '',
    serviceAccountJson: '',
    rootFolderId: 'root',
    driveId: '',
    // alipan
    driveType: 'backup',
    oauthTokenUrl: '',
  }
}

const form = ref(defaultForm())
const testing = ref(false)
const testResult = ref<TestResult | null>(null)
const saving = ref(false)

function resetForm() {
  form.value = defaultForm()
  testResult.value = null
}

function onTypeChange(newType: string) {
  testResult.value = null
  if (newType === 'ftp') form.value.port = 21
  if (newType === 'sftp') form.value.port = 22
}

function fillJianguoyunPreset() {
  form.value.endpoint = 'https://dav.jianguoyun.com/dav/我的坚果云'
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
      switch (detail.type) {
        case 'local':
          form.value.root = String(c.root || '')
          break
        case 's3':
          form.value.endpoint = String(c.endpoint || '')
          form.value.region = String(c.region || '')
          form.value.bucket = String(c.bucket || '')
          form.value.prefix = String(c.prefix || '')
          form.value.accessKey = String(c.access_key || '')
          form.value.secretKey = String(c.secret_key || '')
          form.value.pathStyle = !!c.use_path_style
          break
        case 'webdav':
          form.value.endpoint = String(c.endpoint || '')
          form.value.username = String(c.username || '')
          form.value.password = String(c.password || '')
          form.value.root = String(c.root || '')
          form.value.insecureTls = !!c.insecure_tls
          form.value.proxyUrl = String(c.proxy_url || '')
          break
        case 'gdrive':
          form.value.authType = String(c.auth_type || 'oauth2')
          form.value.clientId = String(c.client_id || '')
          form.value.clientSecret = String(c.client_secret || '')
          form.value.refreshToken = String(c.refresh_token || '')
          form.value.serviceAccountJson = String(c.service_account_json || '')
          form.value.rootFolderId = String(c.root_folder_id || 'root')
          form.value.driveId = String(c.drive_id || '')
          form.value.proxyUrl = String(c.proxy_url || '')
          break
        case 'alipan':
          form.value.driveType = String(c.drive_type || 'backup')
          form.value.refreshToken = String(c.refresh_token || '')
          form.value.clientId = String(c.client_id || '')
          form.value.clientSecret = String(c.client_secret || '')
          form.value.oauthTokenUrl = String(c.oauth_token_url || '')
          form.value.rootFolderId = String(c.root_folder_id || 'root')
          form.value.proxyUrl = String(c.proxy_url || '')
          break
        case 'ftp':
          form.value.host = String(c.host || '')
          form.value.port = Number(c.port || 21)
          form.value.username = String(c.username || '')
          form.value.password = String(c.password || '')
          form.value.root = String(c.root || '')
          form.value.tls = !!c.tls
          form.value.insecureTls = !!c.insecure_tls
          break
        case 'sftp':
          form.value.host = String(c.host || '')
          form.value.port = Number(c.port || 22)
          form.value.username = String(c.username || '')
          form.value.password = String(c.password || '')
          form.value.privateKey = String(c.private_key || '')
          form.value.privateKeyPassphrase = String(c.private_key_passphrase || '')
          form.value.root = String(c.root || '')
          break
      }
    } catch (e: any) {
      ElMessage.error('加载源配置失败：' + e.message)
      visible.value = false
    }
  }
})

/** 由表单构造提交的 config；空值键省略 */
function curConfig(): Record<string, unknown> {
  const f = form.value
  switch (f.type) {
    case 'local':
      return { root: f.root.trim() }

    case 's3': {
      const cfg: Record<string, unknown> = {
        endpoint: f.endpoint.trim(),
        region: f.region.trim() || 'us-east-1',
        bucket: f.bucket.trim(),
        use_path_style: f.pathStyle,
      }
      if (f.prefix.trim()) cfg.prefix = f.prefix.trim()
      if (f.accessKey.trim()) cfg.access_key = f.accessKey.trim()
      if (f.secretKey) cfg.secret_key = f.secretKey
      return cfg
    }

    case 'webdav': {
      const cfg: Record<string, unknown> = {
        endpoint: f.endpoint.trim(),
        insecure_tls: f.insecureTls,
      }
      if (f.username.trim()) cfg.username = f.username.trim()
      if (f.password) cfg.password = f.password
      if (f.root.trim()) cfg.root = f.root.trim()
      if (f.proxyUrl.trim()) cfg.proxy_url = f.proxyUrl.trim()
      return cfg
    }

    case 'gdrive': {
      const cfg: Record<string, unknown> = {
        auth_type: f.authType,
        root_folder_id: f.rootFolderId.trim() || 'root',
      }
      if (f.authType === 'service_account') {
        if (f.serviceAccountJson.trim()) cfg.service_account_json = f.serviceAccountJson.trim()
      } else {
        if (f.clientId.trim()) cfg.client_id = f.clientId.trim()
        if (f.clientSecret) cfg.client_secret = f.clientSecret
        if (f.refreshToken) cfg.refresh_token = f.refreshToken
      }
      if (f.driveId.trim()) cfg.drive_id = f.driveId.trim()
      if (f.proxyUrl.trim()) cfg.proxy_url = f.proxyUrl.trim()
      return cfg
    }

    case 'alipan': {
      const cfg: Record<string, unknown> = {
        drive_type: f.driveType,
        root_folder_id: f.rootFolderId.trim() || 'root',
      }
      if (f.refreshToken) cfg.refresh_token = f.refreshToken
      if (f.clientId.trim()) cfg.client_id = f.clientId.trim()
      if (f.clientSecret) cfg.client_secret = f.clientSecret
      if (f.oauthTokenUrl.trim()) cfg.oauth_token_url = f.oauthTokenUrl.trim()
      if (f.proxyUrl.trim()) cfg.proxy_url = f.proxyUrl.trim()
      return cfg
    }

    case 'ftp': {
      const cfg: Record<string, unknown> = {
        host: f.host.trim(),
        port: Number(f.port) || 21,
        tls: f.tls,
        insecure_tls: f.insecureTls,
      }
      if (f.username.trim()) cfg.username = f.username.trim()
      if (f.password) cfg.password = f.password
      if (f.root.trim()) cfg.root = f.root.trim()
      return cfg
    }

    case 'sftp': {
      const cfg: Record<string, unknown> = {
        host: f.host.trim(),
        port: Number(f.port) || 22,
        username: f.username.trim(),
      }
      if (f.password) cfg.password = f.password
      if (f.privateKey.trim()) cfg.private_key = f.privateKey.trim()
      if (f.privateKeyPassphrase) cfg.private_key_passphrase = f.privateKeyPassphrase
      if (f.root.trim()) cfg.root = f.root.trim()
      return cfg
    }

    default:
      return {}
  }
}

function onFieldInput() {
  testResult.value = null // 配置变更后需重测
}

async function onTest() {
  testing.value = true
  testResult.value = null
  try {
    testResult.value = await testSource(
      form.value.type,
      curConfig(),
      isEdit.value && props.editId ? props.editId : undefined,
    )
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
    width="600px"
  >
    <el-form label-position="top">
      <el-row :gutter="12">
        <el-col :span="12">
          <el-form-item label="源 ID（唯一，字母数字-_）">
            <el-input v-model="form.id" placeholder="gdrive-main" :disabled="isEdit" />
          </el-form-item>
        </el-col>
        <el-col :span="12">
          <el-form-item label="显示名称（可留空）">
            <el-input v-model="form.name" placeholder="我的云盘" />
          </el-form-item>
        </el-col>
      </el-row>

      <el-form-item label="类型">
        <el-select v-model="form.type" :disabled="isEdit" style="width: 100%" @change="onTypeChange">
          <el-option value="s3" label="s3 —— 对象存储（S3/MinIO/R2/OSS/COS/OBS/Kodo/BOS）" />
          <el-option value="webdav" label="webdav —— WebDAV（坚果云 / Nextcloud / NAS）" />
          <el-option value="gdrive" label="gdrive —— Google Drive（个人云盘 / 团队共享盘）" />
          <el-option value="alipan" label="alipan —— 阿里云盘（官方 OpenAPI）" />
          <el-option value="ftp" label="ftp —— FTP / FTPS 服务器" />
          <el-option value="sftp" label="sftp —— SFTP (SSH) 服务器" />
          <el-option value="local" label="local —— 本地磁盘" />
        </el-select>
      </el-form-item>

      <!-- local 字段 -->
      <template v-if="form.type === 'local'">
        <el-form-item label="根目录">
          <el-input v-model="form.root" placeholder="D:\data 或 ./data/files" @input="onFieldInput" />
        </el-form-item>
      </template>

      <!-- webdav 字段（含坚果云快捷预设） -->
      <template v-else-if="form.type === 'webdav'">
        <el-form-item label="WebDAV Endpoint（服务地址）">
          <el-input
            v-model="form.endpoint"
            placeholder="https://dav.jianguoyun.com/dav/我的坚果云"
            @input="onFieldInput"
          >
            <template #append>
              <el-button @click="fillJianguoyunPreset">填入坚果云</el-button>
            </template>
          </el-input>
        </el-form-item>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="账号（Username）">
              <el-input v-model="form.username" placeholder="坚果云邮箱 / NAS 用户名" autocomplete="off" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="应用密码（Password）">
              <el-input v-model="form.password" type="password" show-password autocomplete="new-password" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="子目录前缀（可选）">
              <el-input v-model="form.root" placeholder="备份/文档" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="代理地址 Proxy URL（可选）">
              <el-input v-model="form.proxyUrl" placeholder="http://127.0.0.1:7890" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-checkbox v-model="form.insecureTls" @change="onFieldInput">
          跳过 TLS 证书校验（自建 NAS 自签名证书时勾选）
        </el-checkbox>
        <div class="endpoint-hint">
          坚果云提示：请在「账户信息 → 安全选项 → 第三方应用管理」生成应用密码，且不要直接挂载 <code>/dav/</code> 根路径，需指定子文件夹如 <code>https://dav.jianguoyun.com/dav/我的坚果云</code>。
        </div>
      </template>

      <!-- gdrive 字段 -->
      <template v-else-if="form.type === 'gdrive'">
        <el-form-item label="认证方式">
          <el-radio-group v-model="form.authType" @change="onFieldInput">
            <el-radio value="oauth2">OAuth2 Refresh Token（个人盘/团队盘）</el-radio>
            <el-radio value="service_account">Service Account JSON（服务账号密钥）</el-radio>
          </el-radio-group>
        </el-form-item>

        <template v-if="form.authType === 'oauth2'">
          <el-row :gutter="12">
            <el-col :span="12">
              <el-form-item label="Client ID">
                <el-input v-model="form.clientId" placeholder="xxx.apps.googleusercontent.com" @input="onFieldInput" />
              </el-form-item>
            </el-col>
            <el-col :span="12">
              <el-form-item label="Client Secret">
                <el-input v-model="form.clientSecret" type="password" show-password autocomplete="new-password" @input="onFieldInput" />
              </el-form-item>
            </el-col>
          </el-row>
          <el-form-item label="Refresh Token">
            <el-input v-model="form.refreshToken" type="password" show-password autocomplete="new-password" placeholder="1//0g..." @input="onFieldInput" />
          </el-form-item>
        </template>

        <template v-else>
          <el-form-item label="Service Account JSON（完整密钥 JSON 内容）">
            <el-input
              v-model="form.serviceAccountJson"
              type="textarea"
              :rows="4"
              placeholder='{"type":"service_account","client_email":"...","private_key":"-----BEGIN PRIVATE KEY-----..."}'
              @input="onFieldInput"
            />
          </el-form-item>
        </template>

        <el-row :gutter="12">
          <el-col :span="8">
            <el-form-item label="根目录 Folder ID">
              <el-input v-model="form.rootFolderId" placeholder="root" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="团队盘 Drive ID（可选）">
              <el-input v-model="form.driveId" placeholder="留空为个人盘" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="代理 Proxy URL（可选）">
              <el-input v-model="form.proxyUrl" placeholder="http://127.0.0.1:7890" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <div class="endpoint-hint">
          删除操作会将文件移入 Google Drive 回收站（<code>trashed=true</code>），不会永久销毁。国内网络环境请填写本地 HTTP/SOCKS5 代理地址。
        </div>
      </template>

      <!-- alipan 字段 -->
      <template v-else-if="form.type === 'alipan'">
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="云盘空间类型">
              <el-select v-model="form.driveType" style="width: 100%" @change="onFieldInput">
                <el-option value="backup" label="备份盘（backup）" />
                <el-option value="resource" label="资源库（resource）" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="根目录 File ID">
              <el-input v-model="form.rootFolderId" placeholder="root" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>

        <el-form-item label="Refresh Token（必填）">
          <el-input v-model="form.refreshToken" type="password" show-password autocomplete="new-password" placeholder="阿里云盘 OAuth2 refresh_token" @input="onFieldInput" />
        </el-form-item>

        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="开放平台 Client ID（自建应用选填）">
              <el-input v-model="form.clientId" placeholder="与下方 OAuth 代理二选一" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="开放平台 Client Secret（自建应用选填）">
              <el-input v-model="form.clientSecret" type="password" show-password autocomplete="new-password" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>

        <el-row :gutter="12">
          <el-col :span="14">
            <el-form-item label="三方 OAuth Token 刷新代理 URL（无企业 Client Secret 时填写）">
              <el-input v-model="form.oauthTokenUrl" placeholder="https://api.example.com/alist/ali_open/token" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="10">
            <el-form-item label="代理 Proxy URL（可选）">
              <el-input v-model="form.proxyUrl" placeholder="http://127.0.0.1:7890" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <div class="endpoint-hint">
          支持官方 302 直链下载与分片流式上传；删除操作默认移入阿里云盘回收站；刷新产生的新 <code>refresh_token</code> 会自动加密回写。
        </div>
      </template>

      <!-- ftp 字段 -->
      <template v-else-if="form.type === 'ftp'">
        <el-row :gutter="12">
          <el-col :span="16">
            <el-form-item label="主机地址（Host）">
              <el-input v-model="form.host" placeholder="192.168.1.10" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="端口（Port）">
              <el-input-number v-model="form.port" :min="1" :max="65535" style="width: 100%" @change="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="用户名">
              <el-input v-model="form.username" placeholder="anonymous" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="密码">
              <el-input v-model="form.password" type="password" show-password autocomplete="new-password" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item label="根目录（可选）">
          <el-input v-model="form.root" placeholder="/pub" @input="onFieldInput" />
        </el-form-item>
        <el-checkbox v-model="form.tls" @change="onFieldInput">启用显式 FTPS (TLS)</el-checkbox>
        <el-checkbox v-model="form.insecureTls" :disabled="!form.tls" @change="onFieldInput">跳过证书校验</el-checkbox>
      </template>

      <!-- sftp 字段 -->
      <template v-else-if="form.type === 'sftp'">
        <el-row :gutter="12">
          <el-col :span="16">
            <el-form-item label="主机地址（Host）">
              <el-input v-model="form.host" placeholder="192.168.1.10" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="8">
            <el-form-item label="端口（Port）">
              <el-input-number v-model="form.port" :min="1" :max="65535" style="width: 100%" @change="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="用户名（必填）">
              <el-input v-model="form.username" placeholder="root" @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="密码（与私钥二选一）">
              <el-input v-model="form.password" type="password" show-password autocomplete="new-password" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item label="SSH 私钥（PEM 原文或服务端文件路径，可选）">
          <el-input v-model="form.privateKey" type="textarea" :rows="2" placeholder="-----BEGIN OPENSSH PRIVATE KEY----- 或 ~/.ssh/id_ed25519" @input="onFieldInput" />
        </el-form-item>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="私钥口令 Passphrase（可选）">
              <el-input v-model="form.privateKeyPassphrase" type="password" show-password @input="onFieldInput" />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="根目录（可选）">
              <el-input v-model="form.root" placeholder="/data" @input="onFieldInput" />
            </el-form-item>
          </el-col>
        </el-row>
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
