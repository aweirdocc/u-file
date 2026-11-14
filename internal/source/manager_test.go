package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"ufile/internal/driver"

	// 触发内建驱动注册（与 cmd/u-file 同样的机制）
	_ "ufile/internal/driver/all"
)

func newTestManager(t *testing.T, seeds []Seed) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func seedLocal(id string) Seed {
	return Seed{ID: id, Name: id, Type: "local", Config: map[string]any{"root": "."}}
}

func TestAddAndList(t *testing.T) {
	m := newTestManager(t, nil)
	if _, err := m.Add("a", "源A", "local", map[string]any{"root": "."}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("b", "源B", "local", map[string]any{"root": ".."}); err != nil {
		t.Fatal(err)
	}
	if got := len(m.List()); got != 2 {
		t.Fatalf("应有 2 个源，得到 %d", got)
	}
	// 列表响应必须脱敏
	s, _ := m.Add("c", "", "s3", map[string]any{"bucket": "b", "secret_key": "topsecret"})
	if s.Config["secret_key"] != "******" {
		t.Fatalf("Add 返回值应脱敏 secret_key，得到 %v", s.Config["secret_key"])
	}
	// 内部 Get 不脱敏
	raw, err := m.Get("c")
	if err != nil {
		t.Fatal(err)
	}
	if raw.Config["secret_key"] != "topsecret" {
		t.Fatalf("Get 应保留原始凭据（内部使用），得到 %v", raw.Config["secret_key"])
	}
	if raw.Driver() == nil {
		t.Fatal("Get 应携带驱动实例")
	}
}

func TestAddValidation(t *testing.T) {
	m := newTestManager(t, nil)
	cases := []struct {
		id, typ string
		cfg     map[string]any
		want    error
	}{
		{"ok-id_1", "local", map[string]any{"root": "."}, nil},
		{"bad id", "local", map[string]any{"root": "."}, ErrBadID},       // 含空格
		{"", "local", map[string]any{"root": "."}, ErrBadID},             // 空
		{"-x", "local", map[string]any{"root": "."}, ErrBadID},           // 非字母数字开头
		{"dup", "local", map[string]any{"root": "."}, nil},               // 先建一个 dup
		{"dup", "local", map[string]any{"root": ".."}, ErrDuplicate},     // 重复 id
		{"ghost", "nosuchtype", map[string]any{}, driver.ErrUnknownType}, // 未知类型（哨兵在 driver 包）
	}
	for i, tc := range cases {
		_, err := m.Add(tc.id, "", tc.typ, tc.cfg)
		if tc.want == nil {
			if err != nil {
				t.Fatalf("case %d: 意外错误 %v", i, err)
			}
		} else if !errors.Is(err, tc.want) {
			t.Fatalf("case %d: 期望 %v，得到 %v", i, tc.want, err)
		}
	}
	// 配置缺失：必须报错（错误为 fmt.Errorf，无哨兵，仅断言失败发生）
	if _, err := m.Add("badcfg", "", "local", map[string]any{}); err == nil {
		t.Fatal("local 源缺少 root 配置应报错")
	}
	// dup 的第二次 Add 失败后不应产生第三个源
	if got := len(m.List()); got != 2 { // ok-id_1 + dup
		t.Fatalf("应有 2 个源，得到 %d", got)
	}
}

func TestRemove(t *testing.T) {
	m := newTestManager(t, []Seed{seedLocal("a"), seedLocal("b")})
	if err := m.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get("a"); err != ErrNotFound {
		t.Fatalf("删除后 Get 应 ErrNotFound，得到 %v", err)
	}
	if err := m.Remove("a"); err != ErrNotFound {
		t.Fatalf("重复删除应 ErrNotFound，得到 %v", err)
	}
	list := m.List()
	if len(list) != 1 || list[0].ID != "b" {
		t.Fatalf("删除后应只剩 b：%v", list)
	}
}

func TestPersistenceAndSeedOnce(t *testing.T) {
	dir := t.TempDir()
	seeds := []Seed{seedLocal("seeded")}

	m1, err := NewManager(dir, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m1.Add("added", "", "local", map[string]any{"root": ".."}); err != nil {
		t.Fatal(err)
	}
	// 持久化文件应已生成且含两个源
	raw, err := os.ReadFile(filepath.Join(dir, "sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("sources.json 不应为空")
	}

	// 重启：seed 不再导入（避免复活已删除的源），added 仍在
	m2, err := NewManager(dir, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	list := m2.List()
	if len(list) != 2 {
		t.Fatalf("重启后应有 2 个源，得到 %d：%v", len(list), list)
	}
	if _, err := m2.Get("seeded"); err != nil {
		t.Fatalf("seeded 应从持久化文件恢复: %v", err)
	}
	if _, err := m2.Get("added"); err != nil {
		t.Fatalf("added 应从持久化文件恢复: %v", err)
	}

	// 删除 seeded 后重启，不应被 seed 复活
	if err := m2.Remove("seeded"); err != nil {
		t.Fatal(err)
	}
	m3, err := NewManager(dir, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m3.Get("seeded"); err != ErrNotFound {
		t.Fatalf("已删除的源不应被 seed 复活，得到 %v", err)
	}
}

func TestRemovingLastSourcePersistsEmptyConfiguration(t *testing.T) {
	dir := t.TempDir()
	seeds := []Seed{seedLocal("only")}
	m1, err := NewManager(dir, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m1.Remove("only"); err != nil {
		t.Fatal(err)
	}

	// sources.json 的空数组代表用户明确清空配置，重启时不能再次导入 seed。
	m2, err := NewManager(dir, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.List(); len(got) != 0 {
		t.Fatalf("删除最后一个源后重启应保持空列表，得到 %v", got)
	}
}

func TestLoadSkipsBrokenSource(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sources.json")
	bad := `[{"id":"ghost","name":"g","type":"nosuchtype","config":{}},{"id":"ok","name":"o","type":"local","config":{"root":"."}}]`
	if err := os.WriteFile(file, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(dir, nil, nil)
	if err != nil {
		t.Fatalf("单个坏源不应阻断启动: %v", err)
	}
	list := m.List()
	if len(list) != 1 || list[0].ID != "ok" {
		t.Fatalf("应只加载 ok 源：%v", list)
	}
}

func TestConcurrentAccess(t *testing.T) {
	m := newTestManager(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			_, _ = m.Add(string(rune('a'+i))+"src", "", "local", map[string]any{"root": "."})
		}(i)
		go func() { defer wg.Done(); _ = m.List() }()
		go func() { defer wg.Done(); _ = m.Test(context.Background(), "local", map[string]any{"root": "."}) }()
	}
	wg.Wait()
	if got := len(m.List()); got != 8 {
		t.Fatalf("并发新增后应有 8 个源，得到 %d", got)
	}
}
