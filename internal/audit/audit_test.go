package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRecordAndRead(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Record(Entry{User: "root", IP: "1.2.3.4", Action: "auth.login", Detail: map[string]string{"user": "root"}})
	l.Record(Entry{User: "root", IP: "1.2.3.4", Action: "source.add", Detail: map[string]string{"id": "s1"}})
	l.Record(Entry{User: "op", IP: "::1", Action: "source.delete"})

	entries, err := l.Read(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("应读回 3 条，实际 %d", len(entries))
	}
	if entries[0].Action != "source.delete" {
		t.Fatalf("Read 应倒序（最新在前），首条=%s", entries[0].Action)
	}
	if entries[2].Action != "auth.login" {
		t.Fatalf("最旧应为 login，实际=%s", entries[2].Action)
	}
	if entries[2].Time.IsZero() {
		t.Fatal("Record 应兜底填充时间")
	}

	// n 截断
	if entries, _ = l.Read(2); len(entries) != 2 || entries[0].Action != "source.delete" {
		t.Fatalf("Read(2) 应只返回最新 2 条: %+v", entries)
	}
}

func TestNilLogSafe(t *testing.T) {
	var l *Log
	l.Record(Entry{Action: "x"}) // 不应 panic
	if entries, err := l.Read(5); err != nil || len(entries) != 0 {
		t.Fatalf("nil Log Read 应返回空: %v %v", entries, err)
	}
}

func TestJSONLHumanReadable(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, nil)
	l.Record(Entry{User: "root", IP: "1.2.3.4", Action: "auth.login"})
	raw, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("每行应为合法 JSON: %v", err)
	}
}

var _ = sync.Mutex{} // 保留占位（mu 在 Log 中使用）
