import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发期通过 proxy 转发 /api 到后端（同源，cookie SameSite=Lax 正常工作）；
// 生产构建产物 dist/ 由后端 go:embed 内嵌，同端口服务，无跨域问题。
export default defineConfig({
  plugins: [vue()],
  base: '/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
