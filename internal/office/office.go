// Package office 提供纯 Go 零外部依赖的 docx / xlsx / pptx (OOXML) 在线预览解析器。
//
// 安全与性能边界：
//   - 单文件大小上限 25MB，解压流限制单条目 30MB，防御 Zip Bomb；
//   - 输出纯结构化数据（段落/样式/表格/工作表/幻灯片元素），由前端安全渲染，杜绝 XSS；
//   - Excel 单工作表预览上限 1000 行 × 64 列，保证万行大表秒开不卡死浏览器。
package office

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	ErrUnsupportedFormat = errors.New("office: 不支持的 Office 文档格式（仅支持 docx/xlsx/pptx）")
	ErrFileTooLarge      = errors.New("office: 文档体积超出在线预览上限 (25MB)")
	ErrCorruptedFile     = errors.New("office: 文档结构损坏或非标准 OOXML 格式")
)

const (
	MaxOfficeBytes       = 25 << 20 // 25MB
	 maxEntryUncompressed = 30 << 20 // 30MB
	maxEmbeddedImageSize = 4 << 20  // 4MB 单张内嵌图上限
	maxExcelPreviewRows  = 1000
	maxExcelPreviewCols  = 64
)

// IsOfficeExt 判断文件名是否为支持的 Office 预览格式，返回 "docx" | "xlsx" | "pptx" | ""。
func IsOfficeExt(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".docx":
		return "docx"
	case ".xlsx":
		return "xlsx"
	case ".pptx":
		return "pptx"
	default:
		return ""
	}
}

// Result 统一 Office 解析输出。
type Result struct {
	Kind string    `json:"kind"` // "docx" | "xlsx" | "pptx"
	Name string    `json:"name"`
	Docx *DocxDoc  `json:"docx,omitempty"`
	Xlsx *XlsxBook `json:"xlsx,omitempty"`
	Pptx *PptxDeck `json:"pptx,omitempty"`
}

// Parse 读取 Office 文档流并解析为结构化预览数据。
func Parse(r io.Reader, filename string) (*Result, error) {
	kind := IsOfficeExt(filename)
	if kind == "" {
		return nil, ErrUnsupportedFormat
	}
	raw, err := io.ReadAll(io.LimitReader(r, MaxOfficeBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > MaxOfficeBytes {
		return nil, ErrFileTooLarge
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptedFile, err)
	}

	res := &Result{Kind: kind, Name: path.Base(filename)}
	switch kind {
	case "docx":
		doc, err := parseDocx(zr)
		if err != nil {
			return nil, err
		}
		res.Docx = doc
	case "xlsx":
		book, err := parseXlsx(zr)
		if err != nil {
			return nil, err
		}
		res.Xlsx = book
	case "pptx":
		deck, err := parsePptx(zr)
		if err != nil {
			return nil, err
		}
		res.Pptx = deck
	}
	return res, nil
}

// —— ZIP 辅助函数 ——

func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	name = strings.TrimPrefix(name, "/")
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			data, err := io.ReadAll(io.LimitReader(rc, maxEntryUncompressed+1))
			if err != nil {
				return nil, err
			}
			if int64(len(data)) > maxEntryUncompressed {
				return nil, ErrFileTooLarge
			}
			return data, nil
		}
	}
	return nil, errEntryNotFound
}

var errEntryNotFound = errors.New("office: entry not found in zip")

type relsDoc struct {
	Relationships []struct {
		ID     string `xml:"Id,attr"`
		Type   string `xml:"Type,attr"`
		Target string `xml:"Target,attr"`
	} `xml:"Relationship"`
}

func parseRels(zr *zip.Reader, relsPath string) map[string]string {
	out := map[string]string{}
	raw, err := readZipFile(zr, relsPath)
	if err != nil {
		return out
	}
	var doc relsDoc
	if xml.Unmarshal(raw, &doc) != nil {
		return out
	}
	for _, r := range doc.Relationships {
		out[r.ID] = r.Target
	}
	return out
}

func loadMediaDataURL(zr *zip.Reader, baseDir, target string) string {
	clean := path.Clean(path.Join(baseDir, target))
	raw, err := readZipFile(zr, clean)
	if err != nil || len(raw) == 0 || len(raw) > maxEmbeddedImageSize {
		return ""
	}
	ext := strings.ToLower(path.Ext(clean))
	mt := mime.TypeByExtension(ext)
	if mt == "" {
		switch ext {
		case ".png":
			mt = "image/png"
		case ".jpg", ".jpeg":
			mt = "image/jpeg"
		case ".gif":
			mt = "image/gif"
		case ".svg":
			mt = "image/svg+xml"
		default:
			return ""
		}
	}
	return "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// ============================================================================
// 1. Word (.docx) 解析
// ============================================================================

type DocxDoc struct {
	Blocks []DocxBlock `json:"blocks"`
}

type DocxBlock struct {
	Type    string      `json:"type"`              // "paragraph" | "table"
	Heading int         `json:"heading,omitempty"` // 1~6 表示 H1~H6
	Align   string      `json:"align,omitempty"`   // "left" | "center" | "right" | "justify"
	IsList  bool        `json:"is_list,omitempty"`
	Runs    []DocxRun   `json:"runs,omitempty"`
	Rows    [][]DocxCell `json:"rows,omitempty"`
}

type DocxRun struct {
	Text      string `json:"text,omitempty"`
	ImageURL  string `json:"image_url,omitempty"`
	Bold      bool   `json:"bold,omitempty"`
	Italic    bool   `json:"italic,omitempty"`
	Underline bool   `json:"underline,omitempty"`
	Strike    bool   `json:"strike,omitempty"`
	Color     string `json:"color,omitempty"`
	FontSize  int    `json:"font_size,omitempty"` // pt
	Href      string `json:"href,omitempty"`
}

type DocxCell struct {
	Text    string `json:"text"`
	ColSpan int    `json:"col_span,omitempty"`
	BgColor string `json:"bg_color,omitempty"`
	Header  bool   `json:"header,omitempty"`
}

func parseDocx(zr *zip.Reader) (*DocxDoc, error) {
	raw, err := readZipFile(zr, "word/document.xml")
	if err != nil {
		return nil, fmt.Errorf("%w: 缺少 word/document.xml", ErrCorruptedFile)
	}
	rels := parseRels(zr, "word/_rels/document.xml.rels")
	mediaCache := map[string]string{}
	getMedia := func(rId string) string {
		if u, ok := mediaCache[rId]; ok {
			return u
		}
		target, ok := rels[rId]
		if !ok {
			return ""
		}
		u := loadMediaDataURL(zr, "word", target)
		mediaCache[rId] = u
		return u
	}

	dec := xml.NewDecoder(bytes.NewReader(raw))
	var blocks []DocxBlock
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "p":
			blk := decodeDocxParagraph(dec, se, rels, getMedia)
			if len(blk.Runs) > 0 || blk.Heading > 0 {
				blocks = append(blocks, blk)
			}
		case "tbl":
			blk := decodeDocxTable(dec, se)
			if len(blk.Rows) > 0 {
				blocks = append(blocks, blk)
			}
		}
	}
	return &DocxDoc{Blocks: blocks}, nil
}

func attrVal(attrs []xml.Attr, local string) string {
	for _, a := range attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

func decodeDocxParagraph(dec *xml.Decoder, start xml.StartElement, rels map[string]string, getMedia func(string) string) DocxBlock {
	blk := DocxBlock{Type: "paragraph"}
	depth := 1
	var inRun bool
	var curRun DocxRun
	var curHref string

	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "pStyle":
				val := strings.ToLower(attrVal(t.Attr, "val"))
				if strings.HasPrefix(val, "heading") || strings.HasPrefix(val, "标题") {
					nStr := strings.TrimLeft(val, "heading标题 ")
					if n, err := strconv.Atoi(nStr); err == nil && n >= 1 && n <= 6 {
						blk.Heading = n
					} else {
						blk.Heading = 2
					}
				} else if val == "title" {
					blk.Heading = 1
				} else if val == "subtitle" {
					blk.Heading = 2
				}
			case "numPr":
				blk.IsList = true
			case "jc":
				jc := attrVal(t.Attr, "val")
				switch jc {
				case "center", "right":
					blk.Align = jc
				case "both", "distribute":
					blk.Align = "justify"
				}
			case "hyperlink":
				if rId := attrVal(t.Attr, "id"); rId != "" {
					if target, ok := rels[rId]; ok && (strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://")) {
						curHref = target
					}
				}
			case "r":
				inRun = true
				curRun = DocxRun{Href: curHref}
			case "b":
				if inRun && attrVal(t.Attr, "val") != "0" && attrVal(t.Attr, "val") != "false" {
					curRun.Bold = true
				}
			case "i":
				if inRun && attrVal(t.Attr, "val") != "0" && attrVal(t.Attr, "val") != "false" {
					curRun.Italic = true
				}
			case "u":
				if inRun && attrVal(t.Attr, "val") != "none" {
					curRun.Underline = true
				}
			case "strike":
				if inRun && attrVal(t.Attr, "val") != "0" && attrVal(t.Attr, "val") != "false" {
					curRun.Strike = true
				}
			case "color":
				if inRun {
					c := attrVal(t.Attr, "val")
					if len(c) == 6 && c != "auto" && c != "000000" {
						curRun.Color = "#" + c
					}
				}
			case "sz":
				if inRun {
					if halfPt, err := strconv.Atoi(attrVal(t.Attr, "val")); err == nil && halfPt > 0 {
						curRun.FontSize = halfPt / 2
					}
				}
			case "t":
				if inRun {
					var s string
					_ = dec.DecodeElement(&s, &t)
					depth-- // DecodeElement 已消费对应的 EndElement
					curRun.Text += s
				}
			case "br":
				if inRun {
					curRun.Text += "\n"
				}
			case "tab":
				if inRun {
					curRun.Text += "\t"
				}
			case "blip":
				if rId := attrVal(t.Attr, "embed"); rId != "" {
					if dataURL := getMedia(rId); dataURL != "" {
						blk.Runs = append(blk.Runs, DocxRun{ImageURL: dataURL})
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "r" && inRun {
				if curRun.Text != "" || curRun.ImageURL != "" {
					blk.Runs = append(blk.Runs, curRun)
				}
				inRun = false
			} else if t.Name.Local == "hyperlink" {
				curHref = ""
			}
			depth--
		}
	}
	return blk
}

func decodeDocxTable(dec *xml.Decoder, start xml.StartElement) DocxBlock {
	blk := DocxBlock{Type: "table"}
	depth := 1
	var curRow []DocxCell
	var inCell bool
	var curCell DocxCell
	var cellTexts []string
	var curParaText strings.Builder

	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "tr":
				curRow = nil
			case "tc":
				inCell = true
				curCell = DocxCell{ColSpan: 1}
				cellTexts = nil
			case "gridSpan":
				if inCell {
					if n, err := strconv.Atoi(attrVal(t.Attr, "val")); err == nil && n > 1 {
						curCell.ColSpan = n
					}
				}
			case "shd":
				if inCell {
					fill := attrVal(t.Attr, "fill")
					if len(fill) == 6 && fill != "auto" && strings.ToUpper(fill) != "FFFFFF" {
						curCell.BgColor = "#" + fill
					}
				}
			case "p":
				if inCell {
					curParaText.Reset()
				}
			case "t":
				if inCell {
					var s string
					_ = dec.DecodeElement(&s, &t)
					depth--
					curParaText.WriteString(s)
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				if inCell && curParaText.Len() > 0 {
					cellTexts = append(cellTexts, curParaText.String())
				}
			case "tc":
				if inCell {
					curCell.Text = strings.Join(cellTexts, "\n")
					if len(blk.Rows) == 0 {
						curCell.Header = true
					}
					curRow = append(curRow, curCell)
					inCell = false
				}
			case "tr":
				if len(curRow) > 0 {
					blk.Rows = append(blk.Rows, curRow)
				}
			}
			depth--
		}
	}
	return blk
}

// ============================================================================
// 2. Excel (.xlsx) 解析
// ============================================================================

type XlsxBook struct {
	Sheets []XlsxSheet `json:"sheets"`
}

type XlsxSheet struct {
	Name      string      `json:"name"`
	Columns   []string    `json:"columns"`
	Rows      [][]string  `json:"rows"`
	Merges    []CellMerge `json:"merges,omitempty"`
	TotalRows int         `json:"total_rows"`
	Truncated bool        `json:"truncated,omitempty"`
}

type CellMerge struct {
	Row     int `json:"row"`
	Col     int `json:"col"`
	RowSpan int `json:"row_span"`
	ColSpan int `json:"col_span"`
}

func parseXlsx(zr *zip.Reader) (*XlsxBook, error) {
	wbRaw, err := readZipFile(zr, "xl/workbook.xml")
	if err != nil {
		return nil, fmt.Errorf("%w: 缺少 xl/workbook.xml", ErrCorruptedFile)
	}
	rels := parseRels(zr, "xl/_rels/workbook.xml.rels")
	shared := parseSharedStrings(zr)

	var wb struct {
		Sheets []struct {
			Name    string `xml:"name,attr"`
			SheetID string `xml:"sheetId,attr"`
			RID     string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(wbRaw, &wb); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptedFile, err)
	}

	book := &XlsxBook{}
	for idx, s := range wb.Sheets {
		target := rels[s.RID]
		if target == "" {
			target = fmt.Sprintf("worksheets/sheet%d.xml", idx+1)
		}
		sheetPath := path.Clean(path.Join("xl", strings.TrimPrefix(target, "/xl/")))
		if strings.HasPrefix(target, "/xl/") {
			sheetPath = strings.TrimPrefix(target, "/")
		}
		shRaw, err := readZipFile(zr, sheetPath)
		if err != nil {
			continue
		}
		sh := decodeXlsxSheet(s.Name, shRaw, shared)
		book.Sheets = append(book.Sheets, sh)
	}
	return book, nil
}

func parseSharedStrings(zr *zip.Reader) []string {
	raw, err := readZipFile(zr, "xl/sharedStrings.xml")
	if err != nil {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var out []string
	var inSI bool
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "si" {
				inSI = true
				sb.Reset()
			} else if inSI && t.Name.Local == "t" {
				var s string
				_ = dec.DecodeElement(&s, &t)
				sb.WriteString(s)
			}
		case xml.EndElement:
			if t.Name.Local == "si" && inSI {
				out = append(out, sb.String())
				inSI = false
			}
		}
	}
	return out
}

var cellRefRe = regexp.MustCompile(`^([A-Z]+)(\d+)$`)

func splitCellRef(ref string) (rowIdx, colIdx int, ok bool) {
	m := cellRefRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(ref)))
	if m == nil {
		return 0, 0, false
	}
	col := 0
	for _, ch := range m[1] {
		col = col*26 + int(ch-'A'+1)
	}
	r, err := strconv.Atoi(m[2])
	if err != nil || r < 1 || col < 1 {
		return 0, 0, false
	}
	return r - 1, col - 1, true
}

func colIndexToName(idx int) string {
	n := idx + 1
	var buf []byte
	for n > 0 {
		n--
		buf = append([]byte{byte('A' + n%26)}, buf...)
		n /= 26
	}
	return string(buf)
}

func decodeXlsxSheet(name string, raw []byte, shared []string) XlsxSheet {
	type xmlCell struct {
		R  string `xml:"r,attr"`
		T  string `xml:"t,attr"`
		V  string `xml:"v"`
		IS struct {
			T string `xml:"t"`
		} `xml:"is"`
	}
	type xmlRow struct {
		R     int       `xml:"r,attr"`
		Cells []xmlCell `xml:"c"`
	}
	var doc struct {
		Rows       []xmlRow `xml:"sheetData>row"`
		MergeCells []struct {
			Ref string `xml:"ref,attr"`
		} `xml:"mergeCells>mergeCell"`
	}
	_ = xml.Unmarshal(raw, &doc)

	maxRow, maxCol := -1, -1
	gridMap := map[[2]int]string{}
	totalRows := len(doc.Rows)

	for rSeq, row := range doc.Rows {
		rIdx := row.R - 1
		if rIdx < 0 {
			rIdx = rSeq
		}
		if rIdx >= maxExcelPreviewRows {
			continue
		}
		if rIdx > maxRow {
			maxRow = rIdx
		}
		for cSeq, cell := range row.Cells {
			_, cIdx, ok := splitCellRef(cell.R)
			if !ok {
				cIdx = cSeq
			}
			if cIdx >= maxExcelPreviewCols {
				continue
			}
			if cIdx > maxCol {
				maxCol = cIdx
			}
			val := cell.V
			switch cell.T {
			case "s":
				if idx, err := strconv.Atoi(val); err == nil && idx >= 0 && idx < len(shared) {
					val = shared[idx]
				}
			case "inlineStr":
				val = cell.IS.T
			case "b":
				if val == "1" {
					val = "TRUE"
				} else if val == "0" {
					val = "FALSE"
				}
			}
			if val != "" {
				gridMap[[2]int{rIdx, cIdx}] = val
			}
		}
	}

	sh := XlsxSheet{
		Name:      name,
		TotalRows: totalRows,
		Truncated: totalRows > maxExcelPreviewRows,
	}
	if maxRow < 0 || maxCol < 0 {
		return sh
	}

	sh.Columns = make([]string, maxCol+1)
	for c := 0; c <= maxCol; c++ {
		sh.Columns[c] = colIndexToName(c)
	}
	sh.Rows = make([][]string, maxRow+1)
	for r := 0; r <= maxRow; r++ {
		rowSlice := make([]string, maxCol+1)
		for c := 0; c <= maxCol; c++ {
			rowSlice[c] = gridMap[[2]int{r, c}]
		}
		sh.Rows[r] = rowSlice
	}

	for _, mc := range doc.MergeCells {
		parts := strings.Split(mc.Ref, ":")
		if len(parts) != 2 {
			continue
		}
		r1, c1, ok1 := splitCellRef(parts[0])
		r2, c2, ok2 := splitCellRef(parts[1])
		if !ok1 || !ok2 || r2 < r1 || c2 < c1 || r1 > maxRow || c1 > maxCol {
			continue
		}
		sh.Merges = append(sh.Merges, CellMerge{
			Row:     r1,
			Col:     c1,
			RowSpan: r2 - r1 + 1,
			ColSpan: c2 - c1 + 1,
		})
	}
	return sh
}

// ============================================================================
// 3. PowerPoint (.pptx) 解析
// ============================================================================

type PptxDeck struct {
	WidthEMU  int64       `json:"width_emu"`
	HeightEMU int64       `json:"height_emu"`
	Slides    []PptxSlide `json:"slides"`
}

type PptxSlide struct {
	Index    int           `json:"index"`
	Title    string        `json:"title,omitempty"`
	Elements []PptxElement `json:"elements"`
}

type PptxElement struct {
	Type     string       `json:"type"` // "text" | "image" | "table"
	IsTitle  bool         `json:"is_title,omitempty"`
	X        float64      `json:"x"` // 百分比 0~100
	Y        float64      `json:"y"`
	W        float64      `json:"w"`
	H        float64      `json:"h"`
	Paras    []PptxPara   `json:"paras,omitempty"`
	ImageURL string       `json:"image_url,omitempty"`
	Rows     [][]string   `json:"rows,omitempty"`
}

type PptxPara struct {
	Align  string    `json:"align,omitempty"`
	Bullet bool      `json:"bullet,omitempty"`
	Runs   []DocxRun `json:"runs"`
}

var slideFileRe = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)

func parsePptx(zr *zip.Reader) (*PptxDeck, error) {
	deck := &PptxDeck{
		WidthEMU:  9144000, // 默认 16:9 (10 inch)
		HeightEMU: 5143500, // 5.625 inch
	}
	if presRaw, err := readZipFile(zr, "ppt/presentation.xml"); err == nil {
		var pres struct {
			SldSz struct {
				Cx int64 `xml:"cx,attr"`
				Cy int64 `xml:"cy,attr"`
			} `xml:"sldSz"`
		}
		if xml.Unmarshal(presRaw, &pres) == nil && pres.SldSz.Cx > 0 && pres.SldSz.Cy > 0 {
			deck.WidthEMU = pres.SldSz.Cx
			deck.HeightEMU = pres.SldSz.Cy
		}
	}

	type slideEntry struct {
		num  int
		path string
	}
	var slideFiles []slideEntry
	for _, f := range zr.File {
		if m := slideFileRe.FindStringSubmatch(f.Name); m != nil {
			n, _ := strconv.Atoi(m[1])
			slideFiles = append(slideFiles, slideEntry{num: n, path: f.Name})
		}
	}
	if len(slideFiles) == 0 {
		return nil, fmt.Errorf("%w: 未找到 ppt/slides/slide*.xml", ErrCorruptedFile)
	}
	sort.Slice(slideFiles, func(i, j int) bool { return slideFiles[i].num < slideFiles[j].num })

	for idx, sf := range slideFiles {
		raw, err := readZipFile(zr, sf.path)
		if err != nil {
			continue
		}
		relsPath := fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", sf.num)
		rels := parseRels(zr, relsPath)
		sl := decodePptxSlide(idx+1, raw, deck.WidthEMU, deck.HeightEMU, zr, rels)
		deck.Slides = append(deck.Slides, sl)
	}
	return deck, nil
}

func toPct(val, total int64) float64 {
	if total <= 0 {
		return 0
	}
	p := float64(val) * 100.0 / float64(total)
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return float64(int(p*10+0.5)) / 10.0
}

func decodePptxSlide(index int, raw []byte, slideW, slideH int64, zr *zip.Reader, rels map[string]string) PptxSlide {
	sl := PptxSlide{Index: index}
	dec := xml.NewDecoder(bytes.NewReader(raw))

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "sp":
			el := decodePptxShape(dec, slideW, slideH)
			if len(el.Paras) > 0 {
				if sl.Title == "" && el.IsTitle {
					for _, p := range el.Paras {
						for _, r := range p.Runs {
							sl.Title += r.Text
						}
					}
					sl.Title = strings.TrimSpace(sl.Title)
				}
				sl.Elements = append(sl.Elements, el)
			}
		case "pic":
			el := decodePptxPic(dec, slideW, slideH, zr, rels)
			if el.ImageURL != "" {
				sl.Elements = append(sl.Elements, el)
			}
		case "graphicFrame":
			el := decodePptxGraphicFrame(dec, slideW, slideH)
			if len(el.Rows) > 0 {
				sl.Elements = append(sl.Elements, el)
			}
		}
	}
	// 若未显式标 title placeholder，取第一个非空文本作为幻灯片导航标题
	if sl.Title == "" && len(sl.Elements) > 0 {
		for _, el := range sl.Elements {
			if el.Type == "text" && len(el.Paras) > 0 {
				var sb strings.Builder
				for _, r := range el.Paras[0].Runs {
					sb.WriteString(r.Text)
				}
				if s := strings.TrimSpace(sb.String()); s != "" {
					sl.Title = s
					break
				}
			}
		}
	}
	return sl
}

func decodePptxShape(dec *xml.Decoder, slideW, slideH int64) PptxElement {
	el := PptxElement{Type: "text"}
	depth := 1
	var inPara, inRun bool
	var curPara PptxPara
	var curRun DocxRun

	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "ph":
				phType := attrVal(t.Attr, "type")
				if phType == "title" || phType == "ctrTitle" {
					el.IsTitle = true
				}
			case "off":
				x, _ := strconv.ParseInt(attrVal(t.Attr, "x"), 10, 64)
				y, _ := strconv.ParseInt(attrVal(t.Attr, "y"), 10, 64)
				el.X = toPct(x, slideW)
				el.Y = toPct(y, slideH)
			case "ext":
				cx, _ := strconv.ParseInt(attrVal(t.Attr, "cx"), 10, 64)
				cy, _ := strconv.ParseInt(attrVal(t.Attr, "cy"), 10, 64)
				if el.W == 0 && cx > 0 {
					el.W = toPct(cx, slideW)
					el.H = toPct(cy, slideH)
				}
			case "p":
				inPara = true
				curPara = PptxPara{}
			case "pPr":
				if inPara {
					switch attrVal(t.Attr, "algn") {
					case "ctr":
						curPara.Align = "center"
					case "r":
						curPara.Align = "right"
					}
				}
			case "buChar", "buAutoNum":
				if inPara {
					curPara.Bullet = true
				}
			case "r":
				if inPara {
					inRun = true
					curRun = DocxRun{}
				}
			case "rPr":
				if inRun {
					if attrVal(t.Attr, "b") == "1" {
						curRun.Bold = true
					}
					if attrVal(t.Attr, "i") == "1" {
						curRun.Italic = true
					}
					if sz, err := strconv.Atoi(attrVal(t.Attr, "sz")); err == nil && sz > 0 {
						curRun.FontSize = sz / 100
					}
				}
			case "srgbClr":
				if inRun {
					c := attrVal(t.Attr, "val")
					if len(c) == 6 {
						curRun.Color = "#" + c
					}
				}
			case "t":
				if inRun {
					var s string
					_ = dec.DecodeElement(&s, &t)
					depth--
					curRun.Text += s
				}
			}
		case xml.EndElement:
			if t.Name.Local == "r" && inRun {
				if curRun.Text != "" {
					curPara.Runs = append(curPara.Runs, curRun)
				}
				inRun = false
			} else if t.Name.Local == "p" && inPara {
				if len(curPara.Runs) > 0 {
					el.Paras = append(el.Paras, curPara)
				}
				inPara = false
			}
			depth--
		}
	}
	return el
}

func decodePptxPic(dec *xml.Decoder, slideW, slideH int64, zr *zip.Reader, rels map[string]string) PptxElement {
	el := PptxElement{Type: "image"}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "blip":
				if rId := attrVal(t.Attr, "embed"); rId != "" {
					if target, ok := rels[rId]; ok {
						el.ImageURL = loadMediaDataURL(zr, "ppt/slides", target)
					}
				}
			case "off":
				x, _ := strconv.ParseInt(attrVal(t.Attr, "x"), 10, 64)
				y, _ := strconv.ParseInt(attrVal(t.Attr, "y"), 10, 64)
				el.X = toPct(x, slideW)
				el.Y = toPct(y, slideH)
			case "ext":
				cx, _ := strconv.ParseInt(attrVal(t.Attr, "cx"), 10, 64)
				cy, _ := strconv.ParseInt(attrVal(t.Attr, "cy"), 10, 64)
				if el.W == 0 && cx > 0 {
					el.W = toPct(cx, slideW)
					el.H = toPct(cy, slideH)
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	return el
}

func decodePptxGraphicFrame(dec *xml.Decoder, slideW, slideH int64) PptxElement {
	el := PptxElement{Type: "table"}
	depth := 1
	var curRow []string
	var inCell bool
	var cellBuf strings.Builder

	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "off":
				x, _ := strconv.ParseInt(attrVal(t.Attr, "x"), 10, 64)
				y, _ := strconv.ParseInt(attrVal(t.Attr, "y"), 10, 64)
				el.X = toPct(x, slideW)
				el.Y = toPct(y, slideH)
			case "ext":
				cx, _ := strconv.ParseInt(attrVal(t.Attr, "cx"), 10, 64)
				cy, _ := strconv.ParseInt(attrVal(t.Attr, "cy"), 10, 64)
				if el.W == 0 && cx > 0 {
					el.W = toPct(cx, slideW)
					el.H = toPct(cy, slideH)
				}
			case "tr":
				curRow = nil
			case "tc":
				inCell = true
				cellBuf.Reset()
			case "t":
				if inCell {
					var s string
					_ = dec.DecodeElement(&s, &t)
					depth--
					cellBuf.WriteString(s)
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "tc":
				if inCell {
					curRow = append(curRow, cellBuf.String())
					inCell = false
				}
			case "tr":
				if len(curRow) > 0 {
					el.Rows = append(el.Rows, curRow)
				}
			}
			depth--
		}
	}
	return el
}
