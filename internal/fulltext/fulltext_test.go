package fulltext

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ufile/internal/driver/local"
)

func buildMinimalDocx(text string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body>
</w:document>`))
	_ = zw.Close()
	return buf.Bytes()
}

func TestFullTextExtractIndexAndSearch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("# 架构会议纪要\n决定采用 io.Pipe 实现跨源零落盘流式传输。"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "contract.docx"), buildMinimalDocx("甲方：星辰科技有限公司，乙方：云海存储服务商，年度采购金额伍佰万元整。"), 0o644); err != nil {
		t.Fatal(err)
	}
	pdfContent := "%PDF-1.4\n1 0 obj\n<< /Length 44 >>\nstream\nBT /F1 12 Tf (Project Milestone M4 Fulltext Ready) Tj ET\nendstream\nendobj\n"
	if err := os.WriteFile(filepath.Join(root, "spec.pdf"), []byte(pdfContent), 0o644); err != nil {
		t.Fatal(err)
	}

	drv, err := local.New(map[string]any{"root": root})
	if err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	eng, err := NewEngine(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	eng.IndexSource(context.Background(), "local-docs", drv)
	st := eng.Status()
	if st.IndexedDocs != 3 {
		t.Fatalf("expected 3 indexed docs, got %d", st.IndexedDocs)
	}

	// 1. 搜索 Markdown 正文词条
	hits := eng.Search("零落盘流式", "", 10)
	if len(hits) != 1 || hits[0].Path != "notes.md" || !strings.Contains(hits[0].Snippet, "零落盘流式") {
		t.Fatalf("unexpected markdown search hits: %+v", hits)
	}

	// 2. 搜索 Word (.docx) 正文词条
	hits = eng.Search("星辰科技", "", 10)
	if len(hits) != 1 || hits[0].Path != "contract.docx" || !strings.Contains(hits[0].Snippet, "星辰科技") {
		t.Fatalf("unexpected docx search hits: %+v", hits)
	}

	// 3. 搜索 PDF 正文词条
	hits = eng.Search("Milestone M4", "", 10)
	if len(hits) != 1 || hits[0].Path != "spec.pdf" {
		t.Fatalf("unexpected pdf search hits: %+v", hits)
	}

	// 4. 重启恢复持久化索引验证
	eng2, err := NewEngine(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(eng2.Search("伍佰万元", "", 10)) != 1 {
		t.Fatalf("expected persisted index to match docx content after reload")
	}
}
