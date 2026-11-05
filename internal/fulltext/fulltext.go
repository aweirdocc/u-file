// Package fulltext 实现 M4.3 文档正文抽取与嵌入式倒排索引全文检索引擎。
//
// 核心特性（对齐 docs/06 5.3 节）：
//   - 多格式正文抽取管道：
//     1. 纯文本/代码/Markdown/配置（.txt/.md/.json/.yaml/.csv/.go/.py/.js/.ts/.sql 等）
//     2. Office 文档（.docx/.xlsx/.pptx，复用 internal/office 结构化解析器提取段落、单元格与幻灯片文本）
//     3. PDF 文档（.pdf，纯 Go 解析 FlateDecode 流与 BT..ET 文本块）
//   - 安全边界：单文件体积上限 10MB（超出自动跳过）、单文件抽取 3 秒超时控制、单协程低优后台运行；
//   - 混合倒排索引 + 词元切分（支持中英文分词与短语精确匹配）+ 命中上下文摘要（Snippet）提取；
//   - 增量指纹比对：按 (source_id, path, size, mod_time) 判断文件是否变更，避免重复抽取。
package fulltext

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"ufile/internal/driver"
	"ufile/internal/office"
)

const (
	MaxExtractBytes   = 10 << 20 // 10MB 单文件上限
	MaxStoredTextRunes = 64000   // 单文档索引保留的最大字符数
	ExtractTimeout    = 3 * time.Second
)

var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".csv": true, ".log": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
	".xml": true, ".html": true, ".htm": true, ".css": true, ".vue": true,
	".go": true, ".js": true, ".ts": true, ".py": true, ".java": true,
	".c": true, ".h": true, ".cpp": true, ".rs": true, ".sh": true,
	".sql": true, ".bat": true, ".ps1": true, ".diff": true, ".env": true,
}

// IsSupportedExt 判断文件名是否支持正文抽取。
func IsSupportedExt(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	if textExts[ext] || ext == ".pdf" {
		return true
	}
	return office.IsOfficeExt(name) != ""
}

// DocRecord 单篇已索引文档的记录。
type DocRecord struct {
	ID       string    `json:"id"` // sourceID + "\x00" + path
	SourceID string    `json:"source_id"`
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	Content  string    `json:"content"` // 规整化后的纯文本正文
}

// Hit 全文检索命中结果（含正文摘要）。
type Hit struct {
	SourceID string    `json:"source_id"`
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	Snippet  string    `json:"snippet,omitempty"`
	Score    int       `json:"score"`
}

// Status 全文索引引擎运行状态。
type Status struct {
	Running       bool       `json:"running"`
	IndexedDocs   int        `json:"indexed_docs"`
	TotalPostings int        `json:"total_postings"`
	LastIndexedAt *time.Time `json:"last_indexed_at,omitempty"`
}

// Engine 嵌入式全文检索引擎。
type Engine struct {
	mu            sync.RWMutex
	file          string
	docs          map[string]*DocRecord      // docID -> DocRecord
	postings      map[string]map[string]bool // token -> set of docID
	running       bool
	lastIndexedAt *time.Time
	indexMu       sync.Mutex // 保证后台抽取单协程串行执行（并发度 = 1）
}

func NewEngine(dataDir string) (*Engine, error) {
	dir := filepath.Join(dataDir, "fulltext")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	e := &Engine{
		file:     filepath.Join(dir, "index.json"),
		docs:     map[string]*DocRecord{},
		postings: map[string]map[string]bool{},
	}
	e.load()
	return e, nil
}

// Status 返回当前索引统计状态。
func (e *Engine) Status() Status {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return Status{
		Running:       e.running,
		IndexedDocs:   len(e.docs),
		TotalPostings: len(e.postings),
		LastIndexedAt: e.lastIndexedAt,
	}
}

// ForgetSource 清理指定源的全部全文索引。
func (e *Engine) ForgetSource(sourceID string) {
	e.mu.Lock()
	removed := false
	prefix := sourceID + "\x00"
	for id := range e.docs {
		if strings.HasPrefix(id, prefix) {
			e.removeDocLocked(id)
			removed = true
		}
	}
	if removed {
		e.saveLocked()
	}
	e.mu.Unlock()
}

// IndexSource 遍历并增量索引指定源下的全部可抽取文档（单协程低优先级）。
func (e *Engine) IndexSource(ctx context.Context, sourceID string, drv driver.FSDriver) {
	e.indexMu.Lock()
	defer e.indexMu.Unlock()

	e.mu.Lock()
	e.running = true
	e.mu.Unlock()

	defer func() {
		now := time.Now()
		e.mu.Lock()
		e.running = false
		e.lastIndexedAt = &now
		e.saveLocked()
		e.mu.Unlock()
	}()

	queue := []string{""}
	visited := map[string]bool{"": true}
	seenDocs := map[string]bool{}

	for len(queue) > 0 && ctx.Err() == nil {
		if len(visited) > 5000 {
			break
		}
		dir := queue[0]
		queue = queue[1:]

		res, err := drv.List(ctx, dir, driver.ListOptions{Limit: 1000})
		if err != nil {
			continue
		}
		for _, ent := range res.Entries {
			if ctx.Err() != nil {
				return
			}
			if ent.IsDir {
				if !visited[ent.Path] {
					visited[ent.Path] = true
					queue = append(queue, ent.Path)
				}
				continue
			}
			if !IsSupportedExt(ent.Name) || ent.Size <= 0 || ent.Size > MaxExtractBytes {
				continue
			}
			docID := sourceID + "\x00" + ent.Path
			seenDocs[docID] = true

			e.mu.RLock()
			existing := e.docs[docID]
			unchanged := existing != nil && existing.Size == ent.Size && existing.ModTime.Equal(ent.ModTime)
			e.mu.RUnlock()
			if unchanged {
				continue
			}

			text, err := ExtractFromDriver(ctx, drv, ent.Path, ent.Name)
			if err != nil || strings.TrimSpace(text) == "" {
				continue
			}
			e.UpsertDoc(sourceID, ent, text)
		}
	}

	// 清理该源下已被删除的旧文档记录
	prefix := sourceID + "\x00"
	e.mu.Lock()
	for id := range e.docs {
		if strings.HasPrefix(id, prefix) && !seenDocs[id] {
			e.removeDocLocked(id)
		}
	}
	e.mu.Unlock()
}

// UpsertDoc 直接写入或更新单篇文档的正文索引。
func (e *Engine) UpsertDoc(sourceID string, ent driver.Entry, text string) {
	cleanText := normalizeWhitespace(text)
	if utf8.RuneCountInString(cleanText) > MaxStoredTextRunes {
		runes := []rune(cleanText)
		cleanText = string(runes[:MaxStoredTextRunes])
	}
	docID := sourceID + "\x00" + ent.Path
	rec := &DocRecord{
		ID:       docID,
		SourceID: sourceID,
		Path:     ent.Path,
		Name:     ent.Name,
		Size:     ent.Size,
		ModTime:  ent.ModTime,
		Content:  cleanText,
	}

	e.mu.Lock()
	e.removeDocLocked(docID)
	e.docs[docID] = rec
	for _, tok := range tokenize(rec.Name + " " + rec.Content) {
		set, ok := e.postings[tok]
		if !ok {
			set = map[string]bool{}
			e.postings[tok] = set
		}
		set[docID] = true
	}
	e.mu.Unlock()
}

// RemoveDoc 移除单篇文档的全文索引。
func (e *Engine) RemoveDoc(sourceID, filePath string) {
	docID := sourceID + "\x00" + filePath
	e.mu.Lock()
	if _, ok := e.docs[docID]; ok {
		e.removeDocLocked(docID)
	}
	e.mu.Unlock()
}

func (e *Engine) removeDocLocked(docID string) {
	old, ok := e.docs[docID]
	if !ok {
		return
	}
	for _, tok := range tokenize(old.Name + " " + old.Content) {
		if set, exists := e.postings[tok]; exists {
			delete(set, docID)
			if len(set) == 0 {
				delete(e.postings, tok)
			}
		}
	}
	delete(e.docs, docID)
}

// Search 执行全文检索（同时匹配文件名与文档正文），返回按相关度排序的命中列表及高亮上下文摘要。
func (e *Engine) Search(query, sourceFilter string, limit int) []Hit {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	qLower := strings.ToLower(q)
	terms := strings.Fields(qLower)
	qTokens := tokenize(q)

	e.mu.RLock()
	defer e.mu.RUnlock()

	// 1. 候选集筛选：通过倒排索引快速定位包含查询词元的文档，同时兜底扫描子串
	candidates := map[string]int{}
	for _, tok := range qTokens {
		if set, ok := e.postings[tok]; ok {
			for docID := range set {
				candidates[docID]++
			}
		}
	}
	// 若查询为极短字符或标点符号未生成词元，则全表扫描
	if len(candidates) == 0 {
		for docID := range e.docs {
			candidates[docID] = 1
		}
	}

	var hits []Hit
	for docID, tokenMatches := range candidates {
		doc := e.docs[docID]
		if doc == nil {
			continue
		}
		if sourceFilter != "" && doc.SourceID != sourceFilter {
			continue
		}

		nameLower := strings.ToLower(doc.Name)
		contentLower := strings.ToLower(doc.Content)

		// 验证所有空格分隔的查询词均出现在文件名或正文中（布尔 AND 语义）
		allMatched := true
		score := tokenMatches
		for _, term := range terms {
			inName := strings.Contains(nameLower, term)
			inBody := strings.Contains(contentLower, term)
			if !inName && !inBody {
				allMatched = false
				break
			}
			if inName {
				score += 10
			}
			if inBody {
				score += 5
			}
		}
		if !allMatched {
			continue
		}

		snippet := buildSnippet(doc.Content, terms)
		hits = append(hits, Hit{
			SourceID: doc.SourceID,
			Path:     doc.Path,
			Name:     doc.Name,
			IsDir:    false,
			Size:     doc.Size,
			ModTime:  doc.ModTime,
			Snippet:  snippet,
			Score:    score,
		})
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Path < hits[j].Path
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// —— 文档内容抽取管道 ——

// ExtractFromDriver 带 3 秒超时与 10MB 大小保护地从驱动读取并抽取文本。
func ExtractFromDriver(parentCtx context.Context, drv driver.FSDriver, filePath, fileName string) (string, error) {
	ctx, cancel := context.WithTimeout(parentCtx, ExtractTimeout)
	defer cancel()

	rc, err := drv.Open(ctx, filePath, driver.OpenOptions{Length: MaxExtractBytes + 1})
	if err != nil {
		return "", err
	}
	defer rc.Close()

	raw, err := io.ReadAll(io.LimitReader(rc, MaxExtractBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(raw)) > MaxExtractBytes {
		return "", nil
	}
	return ExtractBytes(raw, fileName)
}

// ExtractBytes 从字节流按扩展名抽取纯文本内容。
func ExtractBytes(raw []byte, fileName string) (string, error) {
	ext := strings.ToLower(path.Ext(fileName))
	if office.IsOfficeExt(fileName) != "" {
		res, err := office.Parse(bytes.NewReader(raw), fileName)
		if err != nil {
			return "", err
		}
		return extractOfficeResult(res), nil
	}
	if ext == ".pdf" {
		return extractPDFBytes(raw), nil
	}
	if textExts[ext] {
		if !utf8.Valid(raw) {
			return "", nil
		}
		return string(raw), nil
	}
	return "", nil
}

func extractOfficeResult(res *office.Result) string {
	var sb strings.Builder
	if res.Docx != nil {
		for _, blk := range res.Docx.Blocks {
			for _, run := range blk.Runs {
				if run.Text != "" {
					sb.WriteString(run.Text)
				}
			}
			for _, row := range blk.Rows {
				for _, cell := range row {
					if cell.Text != "" {
						sb.WriteString(" ")
						sb.WriteString(cell.Text)
					}
				}
			}
			sb.WriteString("\n")
		}
	}
	if res.Xlsx != nil {
		for _, sh := range res.Xlsx.Sheets {
			sb.WriteString(sh.Name)
			sb.WriteString("\n")
			for _, row := range sh.Rows {
				for _, val := range row {
					if val != "" {
						sb.WriteString(val)
						sb.WriteString(" ")
					}
				}
				sb.WriteString("\n")
			}
		}
	}
	if res.Pptx != nil {
		for _, sl := range res.Pptx.Slides {
			if sl.Title != "" {
				sb.WriteString(sl.Title)
				sb.WriteString("\n")
			}
			for _, el := range sl.Elements {
				for _, p := range el.Paras {
					for _, r := range p.Runs {
						sb.WriteString(r.Text)
					}
					sb.WriteString("\n")
				}
				for _, row := range el.Rows {
					sb.WriteString(strings.Join(row, " "))
					sb.WriteString("\n")
				}
			}
		}
	}
	return sb.String()
}

var pdfStreamRe = regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
var pdfTextParenRe = regexp.MustCompile(`\(([^()\\]*(?:\\.[^()\\]*)*)\)`)

// extractPDFBytes 轻量纯 Go PDF 文本提取（支持未压缩流与 FlateDecode 压缩流中的文本字面量）。
func extractPDFBytes(raw []byte) string {
	var sb strings.Builder
	matches := pdfStreamRe.FindAllSubmatch(raw, 200)
	for _, m := range matches {
		streamData := m[1]
		if zr, err := zlib.NewReader(bytes.NewReader(streamData)); err == nil {
			dec, _ := io.ReadAll(io.LimitReader(zr, 2<<20))
			zr.Close()
			if len(dec) > 0 {
				streamData = dec
			}
		}
		for _, tm := range pdfTextParenRe.FindAllSubmatch(streamData, -1) {
			s := unescapePDFString(string(tm[1]))
			if utf8.ValidString(s) && strings.TrimSpace(s) != "" {
				sb.WriteString(s)
				sb.WriteString(" ")
			}
		}
	}
	// 若无 stream 块，尝试直接在全文提取括号文本块
	if sb.Len() == 0 {
		for _, tm := range pdfTextParenRe.FindAllSubmatch(raw, 500) {
			s := unescapePDFString(string(tm[1]))
			if utf8.ValidString(s) && strings.TrimSpace(s) != "" {
				sb.WriteString(s)
				sb.WriteString(" ")
			}
		}
	}
	return sb.String()
}

func unescapePDFString(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\r`, " ")
	s = strings.ReplaceAll(s, `\t`, " ")
	s = strings.ReplaceAll(s, `\(`, "(")
	s = strings.ReplaceAll(s, `\)`, ")")
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

// —— 分词与上下文摘要生成 ——

func tokenize(text string) []string {
	lower := strings.ToLower(text)
	seen := map[string]bool{}
	var out []string

	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}

	var asciiBuf strings.Builder
	var prevCJK rune
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			asciiBuf.WriteRune(r)
			prevCJK = 0
			continue
		}
		if asciiBuf.Len() > 0 {
			w := strings.Trim(asciiBuf.String(), "_-")
			if len(w) >= 2 {
				add(w)
			}
			asciiBuf.Reset()
		}
		if isCJK(r) {
			add(string(r))
			if prevCJK != 0 {
				add(string([]rune{prevCJK, r}))
			}
			prevCJK = r
		} else {
			prevCJK = 0
		}
	}
	if asciiBuf.Len() > 0 {
		w := strings.Trim(asciiBuf.String(), "_-")
		if len(w) >= 2 {
			add(w)
		}
	}
	return out
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		(r >= 0x3040 && r <= 0x30FF) ||
		(r >= 0xAC00 && r <= 0xD7AF)
}

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func buildSnippet(content string, terms []string) string {
	if content == "" || len(terms) == 0 {
		return ""
	}
	lower := strings.ToLower(content)
	bestByteIdx := -1
	for _, t := range terms {
		if idx := strings.Index(lower, t); idx >= 0 {
			if bestByteIdx == -1 || idx < bestByteIdx {
				bestByteIdx = idx
			}
		}
	}
	runes := []rune(content)
	if bestByteIdx < 0 {
		if len(runes) > 120 {
			return string(runes[:120]) + "…"
		}
		return content
	}
	runeIdx := utf8.RuneCountInString(content[:bestByteIdx])
	start := runeIdx - 35
	if start < 0 {
		start = 0
	}
	end := runeIdx + 95
	if end > len(runes) {
		end = len(runes)
	}
	snip := string(runes[start:end])
	if start > 0 {
		snip = "…" + snip
	}
	if end < len(runes) {
		snip = snip + "…"
	}
	return snip
}

// —— 持久化 ——

type persistedIndex struct {
	UpdatedAt *time.Time   `json:"updated_at,omitempty"`
	Docs      []*DocRecord `json:"docs"`
}

func (e *Engine) load() {
	raw, err := os.ReadFile(e.file)
	if err != nil {
		return
	}
	var snap persistedIndex
	if err := json.Unmarshal(raw, &snap); err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastIndexedAt = snap.UpdatedAt
	for _, doc := range snap.Docs {
		if doc == nil || doc.ID == "" {
			continue
		}
		e.docs[doc.ID] = doc
		for _, tok := range tokenize(doc.Name + " " + doc.Content) {
			set, ok := e.postings[tok]
			if !ok {
				set = map[string]bool{}
				e.postings[tok] = set
			}
			set[doc.ID] = true
		}
	}
}

func (e *Engine) saveLocked() {
	list := make([]*DocRecord, 0, len(e.docs))
	for _, doc := range e.docs {
		list = append(list, doc)
	}
	snap := persistedIndex{
		UpdatedAt: e.lastIndexedAt,
		Docs:      list,
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return
	}
	tmp := e.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		_ = os.Rename(tmp, e.file)
	}
}
