// 导出 URL 与下载触发（拆出来避免循环依赖：api/index 不依赖 DOM）
export { exportUrl } from '../api'
export function downloadHref(href: string) {
  const a = document.createElement('a')
  a.href = href
  a.download = ''
  a.click()
}
