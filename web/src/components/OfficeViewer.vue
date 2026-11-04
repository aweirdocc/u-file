<template>
  <div class="office-viewer">
    <!-- ==================== 1. Word (.docx) 纸张阅读器 ==================== -->
    <div v-if="data.kind === 'docx' && data.docx" class="docx-container">
      <div class="docx-page">
        <div v-if="!data.docx.blocks || data.docx.blocks.length === 0" class="office-empty">
          文档暂无可显示的文本或表格内容
        </div>

        <template v-for="(blk, bIdx) in data.docx.blocks" :key="bIdx">
          <!-- 段落 / 标题 / 列表项 -->
          <component
            :is="headingTag(blk.heading)"
            v-if="blk.type === 'paragraph'"
            class="docx-para"
            :class="[
              blk.heading ? `docx-h${blk.heading}` : '',
              blk.is_list ? 'docx-list-item' : '',
            ]"
            :style="{ textAlign: blk.align || 'left' }"
          >
            <template v-for="(run, rIdx) in blk.runs" :key="rIdx">
              <img
                v-if="run.image_url"
                :src="run.image_url"
                class="docx-inline-img"
                alt="embedded image"
              >
              <a
                v-else-if="run.href"
                :href="run.href"
                target="_blank"
                rel="noopener noreferrer"
                class="docx-link"
                :style="runStyle(run)"
              >{{ run.text }}</a>
              <span v-else :style="runStyle(run)">{{ run.text }}</span>
            </template>
          </component>

          <!-- 表格 -->
          <div v-else-if="blk.type === 'table' && blk.rows" class="docx-table-wrap">
            <table class="docx-table">
              <tbody>
                <tr v-for="(row, rIdx) in blk.rows" :key="rIdx">
                  <component
                    :is="cell.header || rIdx === 0 ? 'th' : 'td'"
                    v-for="(cell, cIdx) in row"
                    :key="cIdx"
                    :colspan="cell.col_span || 1"
                    :style="cell.bg_color ? { backgroundColor: cell.bg_color } : undefined"
                  >
                    {{ cell.text }}
                  </component>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
      </div>
    </div>

    <!-- ==================== 2. Excel (.xlsx) 多工作表网格 ==================== -->
    <div v-else-if="data.kind === 'xlsx' && data.xlsx" class="xlsx-container">
      <div class="xlsx-toolbar">
        <div class="xlsx-tabs">
          <button
            v-for="(sh, idx) in data.xlsx.sheets"
            :key="idx"
            type="button"
            class="xlsx-tab"
            :class="{ active: activeSheetIdx === idx }"
            @click="activeSheetIdx = idx; sheetQuery = ''"
          >
            📗 {{ sh.name || `Sheet${idx + 1}` }}
            <span class="xlsx-tab-count">({{ sh.total_rows || 0 }})</span>
          </button>
        </div>
        <div class="xlsx-filter">
          <input
            v-model.trim="sheetQuery"
            type="text"
            placeholder="在当前工作表中筛选行..."
            class="xlsx-search-input"
          >
          <span v-if="currentSheet?.truncated" class="xlsx-trunc-badge">
            已截断显示前 1,000 行（总计 {{ currentSheet.total_rows }} 行）
          </span>
        </div>
      </div>

      <div v-if="!currentSheet || !currentSheet.rows || currentSheet.rows.length === 0" class="office-empty">
        当前工作表为空
      </div>
      <div v-else class="xlsx-grid-scroll">
        <table class="xlsx-table">
          <thead>
            <tr>
              <th class="xlsx-row-num">#</th>
              <th v-for="(colName, cIdx) in currentSheet.columns" :key="cIdx" class="xlsx-col-hdr">
                {{ colName }}
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in filteredRows" :key="item.rIdx">
              <td class="xlsx-row-num">{{ item.rIdx + 1 }}</td>
              <template v-for="(val, cIdx) in item.row" :key="cIdx">
                <td
                  v-if="!isMergedHidden(item.rIdx, cIdx)"
                  :rowspan="getMergeSpan(item.rIdx, cIdx)?.rowSpan || 1"
                  :colspan="getMergeSpan(item.rIdx, cIdx)?.colSpan || 1"
                  :class="{ 'xlsx-header-row': item.rIdx === 0 && !sheetQuery }"
                >
                  {{ val }}
                </td>
              </template>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- ==================== 3. PowerPoint (.pptx) 幻灯片放映器 ==================== -->
    <div v-else-if="data.kind === 'pptx' && data.pptx" class="pptx-container">
      <div v-if="!data.pptx.slides || data.pptx.slides.length === 0" class="office-empty">
        演示文稿中未找到幻灯片
      </div>
      <div v-else class="pptx-layout">
        <!-- 左侧幻灯片缩略大纲栏 -->
        <div class="pptx-sidebar">
          <div
            v-for="(sl, idx) in data.pptx.slides"
            :key="idx"
            class="pptx-thumb-item"
            :class="{ active: activeSlideIdx === idx }"
            @click="activeSlideIdx = idx"
          >
            <span class="pptx-thumb-num">{{ sl.index || idx + 1 }}</span>
            <div class="pptx-thumb-info">
              <div class="pptx-thumb-title">{{ sl.title || `幻灯片 ${idx + 1}` }}</div>
              <div class="pptx-thumb-meta">{{ sl.elements?.length || 0 }} 个元素</div>
            </div>
          </div>
        </div>

        <!-- 右侧幻灯片主舞台 -->
        <div class="pptx-main">
          <div class="pptx-stage-bar">
            <button
              type="button"
              class="btn btn-sm"
              :disabled="activeSlideIdx <= 0"
              @click="activeSlideIdx--"
            >
              ‹ 上一页
            </button>
            <span class="pptx-page-indicator">
              第 {{ activeSlideIdx + 1 }} / {{ data.pptx.slides.length }} 页
              <template v-if="currentSlide?.title"> · {{ currentSlide.title }}</template>
            </span>
            <button
              type="button"
              class="btn btn-sm"
              :disabled="activeSlideIdx >= data.pptx.slides.length - 1"
              @click="activeSlideIdx++"
            >
              下一页 ›
            </button>
          </div>

          <div class="pptx-canvas-wrapper">
            <div class="pptx-canvas" :style="slideAspectRatioStyle">
              <template v-if="currentSlide">
                <div
                  v-for="(el, eIdx) in currentSlide.elements"
                  :key="eIdx"
                  class="pptx-el"
                  :class="[`pptx-el-${el.type}`, el.is_title ? 'pptx-el-title' : '']"
                  :style="{
                    left: `${el.x}%`,
                    top: `${el.y}%`,
                    width: `${el.w}%`,
                    minHeight: `${el.h}%`,
                  }"
                >
                  <!-- 文本框元素 -->
                  <template v-if="el.type === 'text' && el.paras">
                    <div
                      v-for="(p, pIdx) in el.paras"
                      :key="pIdx"
                      class="pptx-para"
                      :class="{ 'pptx-bullet': p.bullet }"
                      :style="{ textAlign: (p.align as any) || 'left' }"
                    >
                      <span
                        v-for="(r, rIdx) in p.runs"
                        :key="rIdx"
                        :style="runStyle(r)"
                      >{{ r.text }}</span>
                    </div>
                  </template>

                  <!-- 图片元素 -->
                  <img
                    v-else-if="el.type === 'image' && el.image_url"
                    :src="el.image_url"
                    class="pptx-img"
                    alt="slide media"
                  >

                  <!-- 表格元素 -->
                  <table v-else-if="el.type === 'table' && el.rows" class="pptx-table">
                    <tbody>
                      <tr v-for="(r, rIdx) in el.rows" :key="rIdx">
                        <td v-for="(c, cIdx) in r" :key="cIdx">{{ c }}</td>
                      </tr>
                    </tbody>
                  </table>
                </div>
              </template>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import type { OfficeResult, DocxRun } from '../api/types'

const props = defineProps<{
  data: OfficeResult
}>()

const activeSheetIdx = ref(0)
const sheetQuery = ref('')
const activeSlideIdx = ref(0)

watch(() => props.data, () => {
  activeSheetIdx.value = 0
  sheetQuery.value = ''
  activeSlideIdx.value = 0
})

function headingTag(level?: number): string {
  if (!level || level < 1 || level > 6) return 'p'
  return `h${level}`
}

function runStyle(r: DocxRun): Record<string, string> {
  const st: Record<string, string> = {}
  if (r.bold) st.fontWeight = '700'
  if (r.italic) st.fontStyle = 'italic'
  const decs: string[] = []
  if (r.underline) decs.push('underline')
  if (r.strike) decs.push('line-through')
  if (decs.length) st.textDecoration = decs.join(' ')
  if (r.color) st.color = r.color
  if (r.font_size && r.font_size >= 8 && r.font_size <= 72) {
    st.fontSize = `${r.font_size}pt`
  }
  return st
}

// —— Excel 计算逻辑 ——
const currentSheet = computed(() => {
  const sheets = props.data.xlsx?.sheets
  if (!sheets || !sheets.length) return null
  return sheets[activeSheetIdx.value] || sheets[0]
})

const filteredRows = computed(() => {
  const sh = currentSheet.value
  if (!sh || !sh.rows) return []
  const q = sheetQuery.value.toLowerCase()
  const out: { rIdx: number; row: string[] }[] = []
  for (let i = 0; i < sh.rows.length; i++) {
    const row = sh.rows[i]
    if (!q || row.some(cell => cell && cell.toLowerCase().includes(q))) {
      out.push({ rIdx: i, row })
    }
  }
  return out
})

const mergeIndex = computed(() => {
  const sh = currentSheet.value
  const starts = new Map<string, { rowSpan: number; colSpan: number }>()
  const hidden = new Set<string>()
  if (!sh || !sh.merges || sheetQuery.value) return { starts, hidden }
  for (const m of sh.merges) {
    starts.set(`${m.row},${m.col}`, { rowSpan: m.row_span, colSpan: m.col_span })
    for (let r = m.row; r < m.row + m.row_span; r++) {
      for (let c = m.col; c < m.col + m.col_span; c++) {
        if (r !== m.row || c !== m.col) {
          hidden.add(`${r},${c}`)
        }
      }
    }
  }
  return { starts, hidden }
})

function isMergedHidden(r: number, c: number): boolean {
  return mergeIndex.value.hidden.has(`${r},${c}`)
}

function getMergeSpan(r: number, c: number) {
  return mergeIndex.value.starts.get(`${r},${c}`)
}

// —— PPTX 计算逻辑 ——
const currentSlide = computed(() => {
  const slides = props.data.pptx?.slides
  if (!slides || !slides.length) return null
  return slides[activeSlideIdx.value] || slides[0]
})

const slideAspectRatioStyle = computed(() => {
  const deck = props.data.pptx
  const w = deck?.width_emu || 9144000
  const h = deck?.height_emu || 5143500
  return {
    aspectRatio: `${w} / ${h}`,
  }
})
</script>

<style scoped>
.office-viewer {
  width: 100%;
  height: 100%;
  min-height: 460px;
  display: flex;
  flex-direction: column;
  color: var(--fg, #1e293b);
}
.office-empty {
  padding: 48px;
  text-align: center;
  color: var(--muted, #64748b);
}

/* ================= Word (.docx) ================= */
.docx-container {
  flex: 1;
  overflow: auto;
  background: var(--bg-subtle, #f1f5f9);
  padding: 24px 16px;
  border-radius: 8px;
}
.docx-page {
  max-width: 820px;
  min-height: 680px;
  margin: 0 auto;
  background: var(--surface, #ffffff);
  color: var(--fg, #1e293b);
  padding: 40px 48px;
  border-radius: 6px;
  box-shadow: 0 2px 12px rgba(15, 23, 42, 0.08);
  line-height: 1.75;
  font-size: 14.5px;
  word-break: break-word;
}
.docx-para {
  margin: 0 0 10px 0;
  white-space: pre-wrap;
}
.docx-h1 { font-size: 24px; font-weight: 700; margin: 18px 0 12px; border-bottom: 1px solid var(--border, #e2e8f0); padding-bottom: 6px; }
.docx-h2 { font-size: 20px; font-weight: 700; margin: 16px 0 10px; }
.docx-h3 { font-size: 17px; font-weight: 600; margin: 14px 0 8px; }
.docx-h4, .docx-h5, .docx-h6 { font-size: 15px; font-weight: 600; margin: 10px 0 6px; }
.docx-list-item {
  position: relative;
  padding-left: 20px;
}
.docx-list-item::before {
  content: "•";
  position: absolute;
  left: 6px;
  color: var(--primary, #2563eb);
  font-weight: bold;
}
.docx-inline-img {
  max-width: 100%;
  height: auto;
  border-radius: 4px;
  margin: 6px 0;
  display: inline-block;
}
.docx-link {
  color: var(--primary, #2563eb);
  text-decoration: underline;
}
.docx-table-wrap {
  overflow-x: auto;
  margin: 14px 0;
}
.docx-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13.5px;
}
.docx-table th,
.docx-table td {
  border: 1px solid var(--border, #cbd5e1);
  padding: 7px 10px;
  text-align: left;
  vertical-align: top;
  white-space: pre-wrap;
}
.docx-table th {
  background: var(--bg-subtle, #f8fafc);
  font-weight: 600;
}

/* ================= Excel (.xlsx) ================= */
.xlsx-container {
  display: flex;
  flex-direction: column;
  height: 68vh;
  border: 1px solid var(--border, #e2e8f0);
  border-radius: 8px;
  background: var(--surface, #ffffff);
  overflow: hidden;
}
.xlsx-toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 8px 12px;
  background: var(--bg-subtle, #f8fafc);
  border-bottom: 1px solid var(--border, #e2e8f0);
}
.xlsx-tabs {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.xlsx-tab {
  padding: 5px 12px;
  font-size: 13px;
  border: 1px solid var(--border, #cbd5e1);
  border-radius: 6px;
  background: var(--surface, #ffffff);
  color: var(--fg, #334155);
  cursor: pointer;
  transition: all 0.15s;
}
.xlsx-tab.active {
  background: var(--primary, #2563eb);
  color: #ffffff;
  border-color: var(--primary, #2563eb);
  font-weight: 600;
}
.xlsx-tab-count {
  font-size: 11px;
  opacity: 0.8;
  margin-left: 3px;
}
.xlsx-filter {
  display: flex;
  align-items: center;
  gap: 8px;
}
.xlsx-search-input {
  padding: 5px 10px;
  font-size: 13px;
  border: 1px solid var(--border, #cbd5e1);
  border-radius: 6px;
  width: 210px;
  background: var(--surface, #fff);
  color: var(--fg, #1e293b);
}
.xlsx-trunc-badge {
  font-size: 12px;
  color: #b45309;
  background: #fef3c7;
  padding: 3px 8px;
  border-radius: 4px;
}
.xlsx-grid-scroll {
  flex: 1;
  overflow: auto;
}
.xlsx-table {
  border-collapse: collapse;
  width: max-content;
  min-width: 100%;
  font-size: 13px;
}
.xlsx-table th,
.xlsx-table td {
  border: 1px solid var(--border, #e2e8f0);
  padding: 5px 10px;
  max-width: 360px;
  white-space: pre-wrap;
  word-break: break-word;
}
.xlsx-col-hdr {
  position: sticky;
  top: 0;
  z-index: 2;
  background: var(--bg-subtle, #f1f5f9);
  color: var(--muted, #475569);
  font-weight: 600;
  text-align: center;
  min-width: 72px;
}
.xlsx-row-num {
  position: sticky;
  left: 0;
  z-index: 1;
  background: var(--bg-subtle, #f1f5f9);
  color: var(--muted, #64748b);
  text-align: center;
  width: 42px;
  font-variant-numeric: tabular-nums;
  user-select: none;
}
.xlsx-header-row {
  font-weight: 600;
  background: rgba(37, 99, 235, 0.04);
}

/* ================= PowerPoint (.pptx) ================= */
.pptx-container {
  height: 68vh;
  display: flex;
  flex-direction: column;
}
.pptx-layout {
  display: flex;
  flex: 1;
  gap: 14px;
  min-height: 0;
}
.pptx-sidebar {
  width: 210px;
  flex-shrink: 0;
  overflow-y: auto;
  border: 1px solid var(--border, #e2e8f0);
  border-radius: 8px;
  background: var(--surface, #ffffff);
  padding: 8px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.pptx-thumb-item {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 8px 10px;
  border-radius: 6px;
  border: 1px solid transparent;
  cursor: pointer;
  transition: all 0.15s;
}
.pptx-thumb-item:hover {
  background: var(--bg-subtle, #f8fafc);
}
.pptx-thumb-item.active {
  background: rgba(37, 99, 235, 0.08);
  border-color: var(--primary, #2563eb);
}
.pptx-thumb-num {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  border-radius: 4px;
  background: var(--bg-subtle, #e2e8f0);
  font-size: 12px;
  font-weight: 600;
  flex-shrink: 0;
}
.pptx-thumb-item.active .pptx-thumb-num {
  background: var(--primary, #2563eb);
  color: #ffffff;
}
.pptx-thumb-info {
  min-width: 0;
  flex: 1;
}
.pptx-thumb-title {
  font-size: 13px;
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.pptx-thumb-meta {
  font-size: 11px;
  color: var(--muted, #64748b);
  margin-top: 2px;
}
.pptx-main {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
  background: var(--bg-subtle, #0f172a);
  border-radius: 8px;
  padding: 12px 16px;
}
.pptx-stage-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 10px;
  color: #e2e8f0;
  font-size: 13px;
}
.pptx-canvas-wrapper {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: auto;
  min-height: 0;
}
.pptx-canvas {
  position: relative;
  width: 100%;
  max-width: 860px;
  max-height: 100%;
  background: #ffffff;
  color: #0f172a;
  border-radius: 6px;
  box-shadow: 0 8px 28px rgba(0, 0, 0, 0.35);
  overflow: hidden;
}
.pptx-el {
  position: absolute;
  box-sizing: border-box;
  padding: 4px 6px;
  overflow: hidden;
  word-break: break-word;
  font-size: 14px;
  line-height: 1.45;
}
.pptx-el-title {
  font-size: 22px;
  font-weight: 700;
}
.pptx-para {
  margin-bottom: 4px;
}
.pptx-bullet {
  padding-left: 16px;
  position: relative;
}
.pptx-bullet::before {
  content: "•";
  position: absolute;
  left: 4px;
  color: #2563eb;
}
.pptx-img {
  width: 100%;
  height: 100%;
  object-fit: contain;
}
.pptx-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;
  background: #ffffff;
}
.pptx-table td {
  border: 1px solid #cbd5e1;
  padding: 4px 6px;
}
</style>
