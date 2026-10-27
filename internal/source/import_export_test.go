package source

import (
	"encoding/json"

	"testing"

	// 注册内建驱动
	_ "ufile/internal/driver/all"
)

func TestExportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, []Seed{
		{ID: "s1", Name: "本地", Type: "local", Config: map[string]any{"root": "."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("s2", "对象", "s3", map[string]any{
		"bucket": "b", "endpoint": "http://x", "access_key": "AK", "secret_key": "SK",
	}); err != nil {
		t.Fatal(err)
	}

	// 全量导出 → JSON 往返
	full := m.Export(false)
	raw, _ := json.MarshalIndent(full, "", "  ")
	var back ExportFile
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Version != 1 || back.Redacted || len(back.Sources) != 2 {
		t.Fatalf("导出结构不符: %+v", back)
	}
	var s2 *stored
	for i := range back.Sources {
		if back.Sources[i].ID == "s2" {
			s2 = &back.Sources[i]
		}
	}
	if s2 == nil || s2.Config["secret_key"] != "SK" {
		t.Fatalf("全量导出应含明文凭据: %+v", s2)
	}

	// 脱敏导出
	red := m.Export(true)
	if !red.Redacted {
		t.Fatal("Redacted 应为 true")
	}
	for _, s := range red.Sources {
		if s.ID == "s2" && (s.Config["secret_key"] != redactedMark || s.Config["access_key"] != redactedMark) {
			t.Fatalf("脱敏导出应打码凭据: %+v", s.Config)
		}
	}
}

func TestImportAddSkipOverwrite(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, []Seed{{ID: "exist", Name: "旧", Type: "local", Config: map[string]any{"root": "."}}})
	if err != nil {
		t.Fatal(err)
	}
	entries := []stored{
		{ID: "new1", Name: "新1", Type: "local", Config: map[string]any{"root": ".."}},
		{ID: "exist", Name: "同名", Type: "local", Config: map[string]any{"root": ".."}},
		{ID: "bad id", Type: "local", Config: map[string]any{"root": "."}},
		{ID: "ghost", Type: "nosuch", Config: map[string]any{}},
	}

	// 不覆盖：exist 跳过
	rep := m.Import(entries, false)
	if rep.Total != 4 || rep.Added != 1 || rep.Skipped != 1 || rep.Failed != 2 {
		t.Fatalf("报告不符: %+v", rep)
	}
	got, _ := m.Get("exist")
	if got.Name != "旧" {
		t.Fatalf("不覆盖模式 exist 不应被修改: %+v", got.Name)
	}

	// 覆盖：exist 更新；new1 已在上一轮导入过，也按更新计
	rep = m.Import(entries, true)
	if rep.Updated != 2 || rep.Skipped != 0 {
		t.Fatalf("覆盖模式报告不符: %+v", rep)
	}
	got, _ = m.Get("exist")
	if got.Name != "同名" {
		t.Fatalf("覆盖后 exist 名称应更新: %+v", got.Name)
	}
}

func TestImportRawBothFormats(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	// ExportFile 格式
	doc := ExportFile{Version: 1, Sources: []stored{
		{ID: "a", Type: "local", Config: map[string]any{"root": "."}},
	}}
	raw, _ := json.Marshal(doc)
	rep, err := m.ImportRaw(raw, false)
	if err != nil || rep.Added != 1 {
		t.Fatalf("ExportFile 导入失败: rep=%+v err=%v", rep, err)
	}

	// 裸数组格式
	rep, err = m.ImportRaw([]byte(`[{"id":"b","type":"local","config":{"root":"."}}]`), false)
	if err != nil || rep.Added != 1 {
		t.Fatalf("裸数组导入失败: rep=%+v err=%v", rep, err)
	}

	// 非法 JSON
	if _, err := m.ImportRaw([]byte(`not json`), false); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	// 空列表
	if _, err := m.ImportRaw([]byte(`[]`), false); err == nil {
		t.Fatal("空数组应报错")
	}
}

func TestImportRedactedWarning(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"version":1,"credentials_redacted":true,"sources":[{"id":"r","type":"s3","config":{"bucket":"b","endpoint":"http://x","access_key":"******","secret_key":"******"}}]}`)
	rep, err := m.ImportRaw(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Redacted != 1 {
		t.Fatalf("应报告 1 条脱敏凭据: %+v", rep)
	}
	// 脱敏标记作为凭据写入会在 Open 时失败——确认 Add 是接受的（存储层无法区分），
	// 但 Test 连接会失败，UI 据此提示。这里只验证导入计数正确。
	s, err := m.Get("r")
	if err != nil {
		t.Fatal(err)
	}
	if s.Config["secret_key"] != redactedMark {
		t.Fatalf("脱敏值应原样写入: %+v", s.Config)
	}
}

func TestExportImportPersistence(t *testing.T) {
	dir := t.TempDir()
	m1, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m1.Add("p1", "持久", "local", map[string]any{"root": "."}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m1.Export(false))
	// 另一个实例导入
	m2, err := NewManager(dir+"/other", nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := m2.ImportRaw(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 1 {
		t.Fatalf("应导入 1 条: %+v", rep)
	}
	if len(m2.List()) != len(m1.List()) {
		t.Fatalf("导入后数量不符: %d vs %d", len(m2.List()), len(m1.List()))
	}
}
