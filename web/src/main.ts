import { createApp } from 'vue'
import { createPinia } from 'pinia'
import ElementPlus from 'element-plus'
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import './styles/theme.css'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import App from './App.vue'
import router from './router'
import { useAuthStore } from './stores/auth'
import { useSettingsStore } from './stores/settings'

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.use(ElementPlus, { locale: zhCn })

// 主题（亮/暗 + 主题色）：index.html 已做防闪屏预处理，这里把 Pinia 状态对齐
useSettingsStore().apply()

const auth = useAuthStore()
auth.bindInterceptor()

// 公开分享页（/s/:token）不做登录初始化：不校验 token、不弹登录框
function isPublicPath() {
  return location.pathname === '/s' || location.pathname.startsWith('/s/')
}

if (isPublicPath()) {
  app.mount('#app')
} else {
  auth.init().finally(() => {
    app.mount('#app')
  })
}
