// 格式化与路径辅助（与旧 UI 逻辑逐行对齐）。

/** 1024 进制人性化大小；与旧 fmtSize 一致 */
export function fmtSize(n: number): string {
  if (n < 1024) return n + ' B'
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KB'
  if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB'
  return (n / 1073741824).toFixed(2) + ' GB'
}

/** 时间显示；Go 零值时间（0001-…）返回空串 */
export function fmtTime(t: string | undefined): string {
  if (!t || t.startsWith('0001')) return ''
  return new Date(t).toLocaleString('zh-CN', { hour12: false })
}

/** 父目录路径（路径不含首尾斜杠，与后端约定一致） */
export function parentOf(p: string): string {
  return p.split('/').slice(0, -1).join('')
}

/** 文件图标（旧 icon() 对齐） */
export function fileIcon(name: string): string {
  if (/\.(jpe?g|png|gif|webp|svg|bmp|ico)$/i.test(name)) return '🖼'
  if (/\.(mp4|mkv|avi|mov|flv)$/i.test(name)) return '🎬'
  if (/\.(mp3|flac|wav|ogg|m4a)$/i.test(name)) return '🎵'
  if (/\.(zip|rar|7z|tar|gz|bz2)$/i.test(name)) return '🗜'
  return '📄'
}

/** joinPath('docs', 'a.txt') → 'docs/a.txt'；dir 为空时直接 name */
export function joinPath(dir: string, name: string): string {
  return dir ? dir + '/' + name : name
}

/** 触发浏览器下载（a[download] 点击） */
export function downloadHref(href: string) {
  const a = document.createElement('a')
  a.href = href
  a.download = ''
  a.click()
}
