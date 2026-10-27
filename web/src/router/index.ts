import { createRouter, createWebHistory } from 'vue-router'
import { useBrowseStore } from '../stores/browse'

// 路由即 URL 同步：/?source=X&path=Y 的结构不变（书签/分享链接兼容），
// 内部改用路径式匹配 /:source(...) 简化组件取参。
const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: () => import('../views/BrowseView.vue') },
    { path: '/:source', component: () => import('../views/BrowseView.vue') },
    // 捕获 /:source/任意深层路径（path 含 /）
    { path: '/:source/:path(.*)', component: () => import('../views/BrowseView.vue') },
  ],
})

// 浏览状态与路由双向同步：URL 变化 → store；导航动作 → URL
router.beforeEach((to) => {
  const browse = useBrowseStore()
  const source = (to.params.source as string) || ''
  const path = (to.params.path as string) || ''
  if (source !== browse.source || path !== browse.path) {
    browse.fromRoute(source, path)
    browse.loadSources().then(() => browse.loadEntries()).catch(() => {})
  }
})

export default router
