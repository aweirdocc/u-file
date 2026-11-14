import { createRouter, createWebHistory } from 'vue-router'
import { useBrowseStore } from '../stores/browse'
import { useAuthStore } from '../stores/auth'

// 路由即 URL 同步：/?source=X&path=Y 的结构不变（书签/分享链接兼容），
// 内部改用路径式匹配 /:source(...) 简化组件取参。
// meta.public        = 无需登录可访问（公开分享页/登录页）
// meta.standalone    = 不参与 source/path ↔ browse store 同步（无平台框架页）
// meta.requiresAdmin = 非 admin 跳回首页
// meta.requiresShare = 需 admin 或具备 share 权限
// 注意：具名路径必须列在 /:source 通配之前，否则会被当成源名。
const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: () => import('../views/BrowseView.vue') },
    { path: '/login', component: () => import('../views/LoginView.vue'), meta: { public: true, standalone: true } },
    { path: '/s/:token', component: () => import('../views/ShareView.vue'), meta: { public: true, standalone: true } },
    { path: '/users', component: () => import('../views/UsersView.vue'), meta: { standalone: true, requiresAdmin: true } },
    { path: '/shares', component: () => import('../views/SharesView.vue'), meta: { standalone: true, requiresShare: true } },
    { path: '/platform', component: () => import('../views/PlatformView.vue'), meta: { standalone: true, requiresAdmin: true } },
    { path: '/:source', component: () => import('../views/BrowseView.vue') },
    // 捕获 /:source/任意深层路径（path 含 /）
    { path: '/:source/:path(.*)', component: () => import('../views/BrowseView.vue') },
  ],
})

// 浏览状态与路由双向同步：URL 变化 → store；导航动作 → URL。
router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (to.meta.requiresAdmin && auth.isLoggedIn && !auth.isAdmin) {
    return '/'
  }
  if (to.meta.requiresShare && auth.isLoggedIn && !auth.isAdmin && !auth.canShare) {
    return '/'
  }
  if (to.meta.standalone) return

  const browse = useBrowseStore()
  const source = (to.params.source as string) || ''
  const path = (to.params.path as string) || ''

  // 根路径（如登录成功回跳 / 、从管理页返回）：加载源列表并默认进入第一个有权限的源。
  if (!source) {
    if (!browse.sources.length) {
      try {
        await browse.loadSources()
      } catch {
        return // 列表加载失败：保持在 /，由视图提示；401 由拦截器跳登录
      }
    }
    if (browse.sources.length) {
      const first = browse.sources[0]
      const pfx = first.access?.path_prefix || ''
      return pfx ? `/${first.id}/${pfx}` : `/${first.id}`
    }
    browse.fromRoute('', '')
    return
  }

  if (source !== browse.source || path !== browse.path) {
    browse.fromRoute(source, path)
    try {
      await browse.loadSources()
    } catch {
      /* 源列表加载失败（如后端不可达）：仍进入页面，由视图呈现错误/空态 */
    }
    browse.loadEntries().catch(() => {}) // 不阻塞导航，列表区自行显示 loading
  }
})

export default router
