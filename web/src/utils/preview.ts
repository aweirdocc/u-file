// 预览类型判定 + 内置无依赖 Markdown 渲染器（从旧 UI 迁移，逻辑不变：先整体
// HTML 转义再套标签，无 XSS 面）。

export type PreviewKind = 'image' | 'svg' | 'video' | 'audio' | 'pdf' | 'docx' | 'xlsx' | 'pptx' | 'md' | 'text' | ''

const PV_KINDS: [PreviewKind, RegExp][] = [
  ['image', /\.(jpe?g|png|gif|webp|bmp|ico|avif)$/i],
  ['svg', /\.svg$/i],
  ['video', /\.(mp4|webm|mov|m4v)$/i], // mkv 编码不定，多数浏览器放不了，走 fallback
  ['audio', /\.(mp3|wav|ogg|flac|m4a|aac)$/i],
  ['pdf', /\.pdf$/i],
  ['docx', /\.docx$/i],
  ['xlsx', /\.xlsx$/i],
  ['pptx', /\.pptx$/i],
  ['md', /\.(md|markdown)$/i],
  ['text', /\.(txt|csv|log|json|ya?ml|toml|ini|xml|html?|css|go|js|mjs|ts|py|java|c|h|cpp|rs|sh|sql|bat|ps1|diff|env)$/i],
]

/** 判断图片格式是否支持后端动态缩放缩略图（避免直接加载数 MB 原图） */
export function isThumbable(name: string): boolean {
  return /\.(jpe?g|png|gif|bmp)$/i.test(name)
}

export function isOfficeKind(k: PreviewKind): k is 'docx' | 'xlsx' | 'pptx' {
  return k === 'docx' || k === 'xlsx' || k === 'pptx'
}

export function previewKind(name: string): PreviewKind {
  for (const [k, re] of PV_KINDS) if (re.test(name)) return k
  return ''
}

export const TEXT_PREVIEW_LIMIT = 2 * 1048576 // 2MB

function escapeHtml(s: string): string {
  return s.replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]!))
}

/** 极简 Markdown 渲染（标题/粗斜体/行内块代码/引用/链接/图片/列表/水平线/围栏代码块） */
export function renderMarkdown(src: string): string {
  const lines = escapeHtml(src).split(/\r?\n/)
  let html = ''
  let inCode = false
  const listStack: string[] = []
  const closeLists = () => {
    while (listStack.length) html += listStack.pop() === 'ul' ? '</ul>' : '</ol>'
  }
  const inline = (s: string) => s
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/\*([^*]+)\*/g, '<em>$1</em>')
    .replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g, '<img alt="$1" src="$2">')
    .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>')
  for (const raw of lines) {
    if (/^```/.test(raw)) {
      closeLists(); inCode = !inCode
      html += inCode ? '<pre><code>' : '</code></pre>'
      continue
    }
    if (inCode) { html += raw + '\n'; continue }
    let m: RegExpMatchArray | null
    if ((m = raw.match(/^(#{1,4})\s+(.*)/))) {
      closeLists()
      html += `<h${m[1].length}>${inline(m[2])}</h${m[1].length}>`
      continue
    }
    if (/^\s*[-*]\s+/.test(raw)) {
      if (listStack[listStack.length - 1] !== 'ul') { closeLists(); html += '<ul>'; listStack.push('ul') }
      html += `<li>${inline(raw.replace(/^\s*[-*]\s+/, ''))}</li>`
      continue
    }
    if (/^\s*\d+\.\s+/.test(raw)) {
      if (listStack[listStack.length - 1] !== 'ol') { closeLists(); html += '<ol>'; listStack.push('ol') }
      html += `<li>${inline(raw.replace(/^\s*\d+\.\s+/, ''))}</li>`
      continue
    }
    if (/^\s*>\s?/.test(raw)) {
      closeLists()
      html += `<blockquote>${inline(raw.replace(/^\s*>\s?/, ''))}</blockquote>`
      continue
    }
    if (/^\s*(---+|\*\*\*+)\s*$/.test(raw)) { closeLists(); html += '<hr>'; continue }
    if (raw.trim() === '') { closeLists(); continue }
    closeLists()
    html += `<p>${inline(raw)}</p>`
  }
  closeLists()
  if (inCode) html += '</code></pre>'
  return html
}
