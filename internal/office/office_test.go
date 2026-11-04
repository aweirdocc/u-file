package office

import (
	"archive/zip"
	"bytes"
	"testing"
)

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseDocx(t *testing.T) {
	raw := makeZip(t, map[string]string{
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p>
      <w:pPr><w:pStyle w:val="Heading1"/><w:jc w:val="center"/></w:pPr>
      <w:r><w:rPr><w:b/></w:rPr><w:t>年度项目报告</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>第一段正文内容。</w:t></w:r>
    </w:p>
    <w:tbl>
      <w:tr>
        <w:tc><w:p><w:r><w:t>表头A</w:t></w:r></w:p></w:tc>
        <w:tc><w:p><w:r><w:t>表头B</w:t></w:r></w:p></w:tc>
      </w:tr>
      <w:tr>
        <w:tc><w:p><w:r><w:t>数据1</w:t></w:r></w:p></w:tc>
        <w:tc><w:p><w:r><w:t>数据2</w:t></w:r></w:p></w:tc>
      </w:tr>
    </w:tbl>
  </w:body>
</w:document>`,
	})

	res, err := Parse(bytes.NewReader(raw), "report.docx")
	if err != nil {
		t.Fatalf("解析 docx 失败: %v", err)
	}
	if res.Kind != "docx" || res.Docx == nil || len(res.Docx.Blocks) != 3 {
		t.Fatalf("docx 块数量不符: %+v", res)
	}
	b0 := res.Docx.Blocks[0]
	if b0.Heading != 1 || b0.Align != "center" || len(b0.Runs) != 1 || b0.Runs[0].Text != "年度项目报告" || !b0.Runs[0].Bold {
		t.Fatalf("标题块解析不符: %+v", b0)
	}
	b2 := res.Docx.Blocks[2]
	if b2.Type != "table" || len(b2.Rows) != 2 || b2.Rows[0][0].Text != "表头A" || b2.Rows[1][1].Text != "数据2" {
		t.Fatalf("表格块解析不符: %+v", b2)
	}
}

func TestParseXlsx(t *testing.T) {
	raw := makeZip(t, map[string]string{
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets>
    <sheet name="营收统计" sheetId="1" r:id="rId1"/>
  </sheets>
</workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Target="worksheets/sheet1.xml"/>
</Relationships>`,
		"xl/sharedStrings.xml": `<?xml version="1.0" encoding="UTF-8"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <si><t>季度</t></si>
  <si><t>销售额</t></si>
  <si><t>Q1</t></si>
</sst>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData>
    <row r="1">
      <c r="A1" t="s"><v>0</v></c>
      <c r="B1" t="s"><v>1</v></c>
    </row>
    <row r="2">
      <c r="A2" t="s"><v>2</v></c>
      <c r="B2"><v>128000</v></c>
    </row>
  </sheetData>
  <mergeCells count="1">
    <mergeCell ref="A3:B3"/>
  </mergeCells>
</worksheet>`,
	})

	res, err := Parse(bytes.NewReader(raw), "finance.xlsx")
	if err != nil {
		t.Fatalf("解析 xlsx 失败: %v", err)
	}
	if res.Kind != "xlsx" || res.Xlsx == nil || len(res.Xlsx.Sheets) != 1 {
		t.Fatalf("xlsx 解析不符: %+v", res)
	}
	sh := res.Xlsx.Sheets[0]
	if sh.Name != "营收统计" || len(sh.Rows) != 2 || sh.Rows[0][0] != "季度" || sh.Rows[1][1] != "128000" {
		t.Fatalf("xlsx sheet 内容不符: %+v", sh)
	}
}

func TestParsePptx(t *testing.T) {
	raw := makeZip(t, map[string]string{
		"ppt/presentation.xml": `<?xml version="1.0" encoding="UTF-8"?>
<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:sldSz cx="9144000" cy="5143500"/>
</p:presentation>`,
		"ppt/slides/slide1.xml": `<?xml version="1.0" encoding="UTF-8"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <p:cSld>
    <p:spTree>
      <p:sp>
        <p:nvSpPr><p:nvPr><p:ph type="ctrTitle"/></p:nvPr></p:nvSpPr>
        <p:spPr>
          <a:xfrm><a:off x="914400" y="1000000"/><a:ext cx="7315200" cy="1200000"/></a:xfrm>
        </p:spPr>
        <p:txBody>
          <a:p><a:r><a:rPr b="1" sz="4400"/><a:t>u-file 架构演进</a:t></a:r></a:p>
        </p:txBody>
      </p:sp>
    </p:spTree>
  </p:cSld>
</p:sld>`,
	})

	res, err := Parse(bytes.NewReader(raw), "slides.pptx")
	if err != nil {
		t.Fatalf("解析 pptx 失败: %v", err)
	}
	if res.Kind != "pptx" || res.Pptx == nil || len(res.Pptx.Slides) != 1 {
		t.Fatalf("pptx 解析不符: %+v", res)
	}
	sl := res.Pptx.Slides[0]
	if sl.Title != "u-file 架构演进" || len(sl.Elements) != 1 || !sl.Elements[0].IsTitle {
		t.Fatalf("slide 内容不符: %+v", sl)
	}
}
