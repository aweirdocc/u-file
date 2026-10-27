<script setup lang="ts">
// 登录 / 引导（首管理员创建）双模式弹窗。el-dialog 默认不可通过遮罩/Esc 关闭
// （close-on-click-modal=false）：必须登录成功才能进入。
import { ref, computed } from 'vue'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const username = ref('')
const password = ref('')
const loading = ref(false)
const errorMessage = ref('')

const isBootstrap = computed(() => auth.needsBootstrap === true)

async function submit() {
  if (!username.value.trim() || !password.value) {
    errorMessage.value = '请输入用户名和密码'
    return
  }
  loading.value = true
  errorMessage.value = ''
  try {
    if (isBootstrap.value) await auth.bootstrap(username.value.trim(), password.value)
    else await auth.login(username.value.trim(), password.value)
    ElMessage.success(isBootstrap.value ? '管理员创建成功' : '登录成功')
    username.value = ''; password.value = ''
  } catch (e: any) {
    errorMessage.value = e.message || '失败'
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <el-dialog
    v-model="auth.authDialogVisible"
    :title="isBootstrap ? '初始化：创建管理员' : '登录 u-file'"
    width="400px"
    :close-on-click-modal="false"
    :close-on-press-escape="false"
    :show-close="false"
    align-center
  >
    <el-alert
      v-if="isBootstrap"
      type="info"
      :closable="false"
      show-icon
      title="首次使用：创建的账号即为管理员（引导入口随后永久关闭）"
      style="margin-bottom: 14px"
    />
    <el-form label-position="top" @submit.prevent="submit">
      <el-form-item label="用户名">
        <el-input v-model="username" autocomplete="username" autofocus />
      </el-form-item>
      <el-form-item label="密码">
        <el-input
          v-model="password"
          type="password"
          autocomplete="current-password"
          show-password
          @keydown.enter="submit"
        />
      </el-form-item>
      <div v-if="errorMessage" class="error-line">{{ errorMessage }}</div>
    </el-form>
    <template #footer>
      <el-button type="primary" :loading="loading" @click="submit">
        {{ isBootstrap ? '创建并进入' : '登录' }}
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.error-line { color: var(--el-color-danger); font-size: 13px; margin-top: -6px; }
</style>
