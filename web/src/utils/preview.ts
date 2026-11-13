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

/**
 * 无依赖、安全的 Markdown 渲染。
 *
 * 原先的逐行正则只识别少数语法，GFM 表格、任务列表、删除线、五/六级标题和
 * 连续段落会以原始标记呈现，容易让人误以为预览缺失内容。这里仍先转义全部输入
 * （不允许 Markdown 内嵌 HTML），再处理常用块级和行内语法。
 */
export function renderMarkdown(src: string): string {
  const lines = escapeHtml(src).split(/\r?\n/)
  const html: string[] = []
  let index = 0

  const safeUrl = (url: string) => {
    const value = url.trim()
    // 仅允许常见外链、站内相对路径和锚点，避免由 v-html 执行脚本 URL。
    return /^(https?:|mailto:|\/|\.\/|\.\.\/|#)/i.test(value) ? value : ''
  }
  const inline = (value: string) => value
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/!\[([^\]]*)\]\(([^\s)]+)(?:\s+&quot;[^&]*&quot;)?\)/g, (_all, alt, url) => {
      const src = safeUrl(url)
      return src ? `<img alt="${alt}" src="${src}" loading="lazy">` : alt
    })
    .replace(/\[([^\]]+)\]\(([^\s)]+)(?:\s+&quot;[^&]*&quot;)?\)/g, (_all, label, url) => {
      const href = safeUrl(url)
      return href ? `<a href="${href}" target="_blank" rel="noopener noreferrer">${label}</a>` : label
    })
    .replace(/&lt;(https?:[^\s&gt;]+)&gt;/g, '<a href="$1" target="_blank" rel="noopener noreferrer">$1</a>')
    .replace(/~~([^~]+)~~/g, '<del>$1</del>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/__([^_]+)__/g, '<strong>$1</strong>')
    .replace(/(?<!\*)\*([^*\n]+)\*(?!\*)/g, '<em>$1</em>')
    .replace(/(?<!_)_([^_\n]+)_(?!_)/g, '<em>$1</em>')

  const tableCells = (line: string) => line.trim().replace(/^\||\|$/g, '').split(/(?<!\\)\|/).map(cell => cell.trim().replace(/\\\|/g, '|'))
  const isTableDivider = (line: string) => {
    const cells = tableCells(line)
    return cells.length > 0 && cells.every(cell => /^:?-{3,}:?$/.test(cell))
  }
  const renderList = () => {
    const first = lines[index].match(/^(\s*)([-+*]|\d+[.)])\s+(.*)$/)
    if (!first) return
    const ordered = /\d/.test(first[2])
    const tag = ordered ? 'ol' : 'ul'
    html.push(`<${tag}>`)
    while (index < lines.length) {
      const item = lines[index].match(/^(\s*)([-+*]|\d+[.)])\s+(.*)$/)
      if (!item || /\d/.test(item[2]) !== ordered || item[1].length !== first[1].length) break
      const task = item[3].match(/^\[([ xX])\]\s+(.*)$/)
      if (task) {
        const checked = task[1].toLowerCase() === 'x' ? ' checked' : ''
        html.push(`<li class="task-item"><input type="checkbox" disabled${checked}> ${inline(task[2])}</li>`)
      } else {
        html.push(`<li>${inline(item[3])}</li>`)
      }
      index++
    }
    html.push(`</${tag}>`)
  }

  while (index < lines.length) {
    const raw = lines[index]
    if (raw.trim() === '') { index++; continue }

    const fence = raw.match(/^\s*(`{3,}|~{3,})\s*([^\s]*)/)
    if (fence) {
      const marker = fence[1][0]
      const lang = fence[2] ? ` class="language-${fence[2].replace(/[^\w-]/g, '')}"` : ''
      const code: string[] = []
      index++
      while (index < lines.length && !new RegExp(`^\\s*${marker}{${fence[1].length},}\\s*$`).test(lines[index])) code.push(lines[index++])
      if (index < lines.length) index++
      html.push(`<pre><code${lang}>${code.join('\n')}</code></pre>`)
      continue
    }

    const heading = raw.match(/^\s*(#{1,6})\s+(.*?)(?:\s+#+)?\s*$/)
    if (heading) {
      const level = heading[1].length
      html.push(`<h${level}>${inline(heading[2])}</h${level}>`)
      index++
      continue
    }
    if (/^\s*((\*\s*){3,}|(-\s*){3,}|(_\s*){3,})$/.test(raw)) {
      html.push('<hr>')
      index++
      continue
    }
    if (index + 1 < lines.length && raw.includes('|') && isTableDivider(lines[index + 1])) {
      const headers = tableCells(raw)
      const aligns = tableCells(lines[index + 1]).map(cell => cell.startsWith(':') && cell.endsWith(':') ? 'center' : cell.endsWith(':') ? 'right' : cell.startsWith(':') ? 'left' : '')
      index += 2
      html.push('<table><thead><tr>', ...headers.map((cell, i) => `<th${aligns[i] ? ` style="text-align:${aligns[i]}"` : ''}>${inline(cell)}</th>`), '</tr></thead><tbody>')
      while (index < lines.length && lines[index].includes('|') && lines[index].trim() !== '') {
        const cells = tableCells(lines[index++])
        html.push('<tr>', ...headers.map((_, i) => `<td${aligns[i] ? ` style="text-align:${aligns[i]}"` : ''}>${inline(cells[i] || '')}</td>`), '</tr>')
      }
      html.push('</tbody></table>')
      continue
    }
    if (/^(\s*)([-+*]|\d+[.)])\s+/.test(raw)) {
      renderList()
      continue
    }
    if (/^\s*>\s?/.test(raw)) {
      const quote: string[] = []
      while (index < lines.length && /^\s*>\s?/.test(lines[index])) quote.push(lines[index++].replace(/^\s*>\s?/, ''))
      html.push(`<blockquote>${quote.map(inline).join('<br>')}</blockquote>`)
      continue
    }

    const paragraph: string[] = []
    while (index < lines.length && lines[index].trim() !== '' &&
      !/^\s*(`{3,}|~{3,})/.test(lines[index]) &&
      !/^\s*(#{1,6})\s+/.test(lines[index]) &&
      !/^(\s*)([-+*]|\d+[.)])\s+/.test(lines[index]) &&
      !/^\s*>\s?/.test(lines[index])) {
      paragraph.push(lines[index++])
    }
    html.push(`<p>${paragraph.map(inline).join('<br>')}</p>`)
  }
  return html.join('')
}
