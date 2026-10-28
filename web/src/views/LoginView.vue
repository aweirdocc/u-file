<script setup lang="ts">
// 独立登录页：登录 / 引导（首管理员创建）双模式。引导入口一次性，创建后关闭。
// 认证成功后由 auth store 的 afterAuth 执行回跳（redirect 或 /）。
import { ref, computed, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../stores/auth'
import ThemeToggle from '../components/ThemeToggle.vue'

const auth = useAuthStore()
const username = ref('')
const password = ref('')
const loading = ref(false)
const errorMessage = ref('')

const isBootstrap = computed(() => auth.needsBootstrap === true)

onMounted(async () => {
  // 已登录直接回跳；顺便探测引导态（init 在公开页/登录页会跳过）
  if (auth.isLoggedIn) {
    auth.afterAuth({ user: auth.user, role: auth.role })
    return
  }
  try {
    const st = await (await import('../api')).getAuthStatus()
    if (st.auth_enabled) auth.needsBootstrap = st.needs_bootstrap
  } catch { /* 不可达时保持默认登录态 */ }
})

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
  <div class="login-page">
    <div class="theme-corner">
      <ThemeToggle />
    </div>

    <div class="login-card">
      <div class="brand">
        <span class="logo">🗂</span>
        <div>
          <div class="title">u-file</div>
          <div class="subtitle">统一文件管理服务平台</div>
        </div>
      </div>

      <el-alert
        v-if="isBootstrap"
        type="info"
        :closable="false"
        show-icon
        title="首次使用：创建的账号即为管理员（引导入口随后永久关闭）"
        style="margin-bottom: 16px"
      />

      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="用户名">
          <el-input v-model="username" placeholder="用户名" size="large" autocomplete="username" autofocus />
        </el-form-item>
        <el-form-item label="密码">
          <el-input
            v-model="password"
            type="password"
            placeholder="密码"
            size="large"
            autocomplete="current-password"
            show-password
            @keydown.enter="submit"
          />
        </el-form-item>
        <div v-if="errorMessage" class="error-line">{{ errorMessage }}</div>
        <el-button type="primary" size="large" class="submit-btn" :loading="loading" @click="submit">
          {{ isBootstrap ? '创建并进入' : '登 录' }}
        </el-button>
      </el-form>
    </div>

    <footer class="page-footer">u-file · 凭据已加密落库 · 操作记录审计日志</footer>
  </div>
</template>

<style scoped>
.login-page {
  flex: 1;
  min-height: 100vh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 24px;
  background:
    radial-gradient(1200px 600px at 85% -10%, var(--el-color-primary-light-9), transparent 60%),
    radial-gradient(900px 500px at -10% 110%, var(--el-color-primary-light-9), transparent 55%),
    var(--ufile-body-bg);
}
html.dark .login-page {
  background:
    radial-gradient(1200px 600px at 85% -10%, var(--el-color-primary-light-9), transparent 60%),
    radial-gradient(900px 500px at -10% 110%, var(--el-color-primary-light-9), transparent 55%),
    var(--ufile-body-bg);
}
.theme-corner { position: fixed; top: 16px; right: 16px; }
.login-card {
  width: 400px;
  max-width: calc(100vw - 32px);
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ufile-radius);
  box-shadow: var(--ufile-card-shadow);
  padding: 32px;
}
.brand { display: flex; align-items: center; gap: 12px; margin-bottom: 24px; }
.brand .logo { font-size: 36px; }
.brand .title { font-size: 20px; font-weight: 700; line-height: 1.2; }
.brand .subtitle { font-size: 12px; color: var(--el-text-color-secondary); }
.error-line { color: var(--el-color-danger); font-size: 13px; margin: -6px 0 10px; }
.submit-btn { width: 100%; margin-top: 6px; }
</style>
