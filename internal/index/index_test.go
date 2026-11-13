package index

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ufile/internal/driver"
)

// memDriver 单测用内存驱动：固定目录树，可注入失败。
type memDriver struct {
	mu      sync.Mutex
	tree    map[string][]driver.Entry // dir → entries
	failOn  map[string]error          // dir → 强制失败
	calls   map[string]int
}

func newMemDriver() *memDriver {
	return &memDriver{
		tree:   map[string][]driver.Entry{"": {}},
		failOn: map[string]error{},
		calls:  map[string]int{},
	}
}

func (m *memDriver) addDir(dir string, entries ...driver.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tree[dir] = entries
}

func (m *memDriver) fail(dir string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failOn[dir] = err
}

// callCount 读取驱动被调用次数（与后台同步 goroutine 并发安全）。
func (m *memDriver) callCount(dir string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[dir]
}

func (m *memDriver) Scheme() string { return "mem" }
func (m *memDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{List: true, Stat: true, Download: true}
}
func (m *memDriver) Stat(ctx context.Context, p string) (*driver.Entry, error) {
	return &driver.Entry{Path: p, Name: p, IsDir: true}, nil
}
func (m *memDriver) Open(ctx context.Context, p string, opt driver.OpenOptions) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (m *memDriver) List(ctx context.Context, dir string, opt driver.ListOptions) (*driver.ListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[dir]++
	if err, ok := m.failOn[dir]; ok {
		return nil, err
	}
	entries, ok := m.tree[dir]
	if !ok {
		return nil, driver.ErrNotFound
	}
	return &driver.ListResult{Entries: entries}, nil
}

func getSrcOf(d *memDriver) (func() []string, func(string) (driver.FSDriver, bool)) {
	return func() []string { return []string{"s1", "s2", "gone"} },
		func(id string) (driver.FSDriver, bool) {
			if id == "gone" {
				return nil, false
			}
			return d, true
		}
}

func TestListCacheHitAndRefresh(t *testing.T) {
	d := newMemDriver()
	d.addDir("", driver.Entry{Path: "a.txt", Name: "a.txt"}, driver.Entry{Path: "sub", Name: "sub", IsDir: true})
	ids, src := getSrcOf(d)
	c := New(t.TempDir(), Config{TTL: time.Minute}, ids, src)

	res, err := c.List(context.Background(), "s1", d, "", driver.ListOptions{}, false)
	if err != nil || len(res.Entries) != 2 {
		t.Fatalf("首次 List: %v", err)
	}
	// 命中缓存：驱动不再被调用
	c.List(context.Background(), "s1", d, "", driver.ListOptions{}, false)
	if d.callCount("") != 1 {
		t.Fatalf("第二次应命中缓存，驱动调用 %d 次", d.callCount(""))
	}
	// refresh=1 绕过缓存
	c.List(context.Background(), "s1", d, "", driver.ListOptions{}, true)
	if d.callCount("") != 2 {
		t.Fatalf("refresh 应回源，驱动调用 %d 次", d.callCount(""))
	}
	// TTL 过期回源
	c.dirs["s1\x00"].At = time.Now().Add(-2 * time.Minute)
	c.List(context.Background(), "s1", d, "", driver.ListOptions{}, false)
	if d.callCount("") != 3 {
		t.Fatal("TTL 过期应回源")
	}
	// 游标请求永不缓存
	d2calls := d.callCount("")
	c.List(context.Background(), "s1", d, "", driver.ListOptions{Cursor: "tok"}, false)
	c.List(context.Background(), "s1", d, "", driver.ListOptions{Cursor: "tok"}, false)
	if d.callCount("") != d2calls+2 {
		t.Fatal("游标请求应每次回源")
	}
}

func TestSyncAllWalksTree(t *testing.T) {
	d := newMemDriver()
	d.addDir("", driver.Entry{Path: "docs", Name: "docs", IsDir: true}, driver.Entry{Path: "root.txt", Name: "root.txt"})
	d.addDir("docs", driver.Entry{Path: "docs/a", Name: "a", IsDir: true})
	d.addDir("docs/a", driver.Entry{Path: "docs/a/f.txt", Name: "f.txt"})
	ids, src := getSrcOf(d)
	c := New(t.TempDir(), Config{SyncEvery: -1}, ids, src) // 禁用自动循环，手动 SyncAll

	c.SyncAll(context.Background())
	st := c.StatusOf("s1")
	if st.LastSuccess == nil || st.LastError != "" {
		t.Fatalf("同步应成功: %+v", st)
	}
	if st.Dirs != 3 || st.SyncedDirs != 3 {
		t.Fatalf("应遍历 3 个目录: %+v", st)
	}
	// 缓存已预热：后续 List 不再打驱动
	before := d.callCount("docs/a")
	c.List(context.Background(), "s1", d, "docs/a", driver.ListOptions{}, false)
	if d.callCount("docs/a") != before {
		t.Fatal("同步后的目录应命中缓存")
	}
}

func TestSyncFailureRecorded(t *testing.T) {
	d := newMemDriver()
	d.addDir("", driver.Entry{Path: "sub", Name: "sub", IsDir: true})
	d.addDir("sub")
	d.fail("sub", errors.New("模拟网络错误"))
	ids, src := getSrcOf(d)
	c := New(t.TempDir(), Config{SyncEvery: -1}, ids, src)

	c.SyncAll(context.Background())
	st := c.StatusOf("s1")
	if st.LastError == "" {
		t.Fatal("应记录失败原因")
	}
	if st.LastErrorAt == nil {
		t.Fatal("应记录失败时间")
	}
	// 修复后下一轮成功清除错误
	delete(d.failOn, "sub")
	c.SyncAll(context.Background())
	st = c.StatusOf("s1")
	if st.LastError != "" || st.LastSuccess == nil {
		t.Fatalf("恢复后应清除错误: %+v", st)
	}
}

func TestForgetAndStatusLifecycle(t *testing.T) {
	d := newMemDriver()
	d.addDir("")
	ids, src := getSrcOf(d)
	c := New(t.TempDir(), Config{SyncEvery: -1}, ids, src)
	c.Touch("s1")
	c.Touch("s2")
	c.SyncAll(context.Background())
	if len(c.AllStatus()) != 2 {
		t.Fatalf("应有 2 个源状态: %d", len(c.AllStatus()))
	}
	c.Forget("s2")
	if len(c.AllStatus()) != 1 {
		t.Fatal("Forget 后应只剩 1 个")
	}
	// 已删除的源（getSrc 返回 false）同步时静默跳过
	c.Touch("gone")
	c.SyncAll(context.Background())
	if st := c.StatusOf("gone"); st.LastError != "" || st.Running {
		t.Fatalf("已删除源应被跳过: %+v", st)
	}
}

func TestSnapshotPersistence(t *testing.T) {
	dir := t.TempDir()
	d := newMemDriver()
	d.addDir("", driver.Entry{Path: "f.txt", Name: "f.txt"})
	ids, src := getSrcOf(d)
	c := New(dir, Config{SyncEvery: -1, Snapshot: true}, ids, src)
	c.SyncAll(context.Background())

	// 快照文件存在
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		t.Fatalf("快照应落盘: %v", err)
	}
	// 新实例加载快照 → 直接命中
	c2 := New(dir, Config{SyncEvery: -1, Snapshot: true}, ids, src)
	before := d.callCount("")
	res, err := c2.List(context.Background(), "s1", d, "", driver.ListOptions{}, false)
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("快照恢复后 List: %v", err)
	}
	if d.callCount("") != before {
		t.Fatal("快照恢复后应命中缓存不再回源")
	}
}

func TestInvalidateAll(t *testing.T) {
	d := newMemDriver()
	d.addDir("")
	ids, src := getSrcOf(d)
	c := New(t.TempDir(), Config{SyncEvery: -1}, ids, src)
	c.List(context.Background(), "s1", d, "", driver.ListOptions{}, false)
	c.List(context.Background(), "s2", d, "", driver.ListOptions{}, false)
	c.Invalidate("") // 清全部
	if len(c.dirs) != 0 {
		t.Fatalf("应清空缓存: %d", len(c.dirs))
	}
}

func TestStartStopLoop(t *testing.T) {
	d := newMemDriver()
	d.addDir("")
	ids, src := getSrcOf(d)
	c := New(t.TempDir(), Config{SyncEvery: 30 * time.Millisecond}, ids, src)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	// 等待至少两轮同步
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.callCount("") >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d.callCount("") < 2 {
		t.Fatalf("后台循环未运行: 调用 %d 次", d.callCount(""))
	}
	c.Stop()
}

func TestSearch(t *testing.T) {
	d := newMemDriver()
	d.addDir("", driver.Entry{Path: "报告.pdf", Name: "报告.pdf"}, driver.Entry{Path: "readme.txt", Name: "readme.txt"}, driver.Entry{Path: "sub", Name: "sub", IsDir: true})
	d.addDir("sub", driver.Entry{Path: "sub/notes.txt", Name: "notes.txt"})
	ids, src := getSrcOf(d)
	c := New(t.TempDir(), Config{SyncEvery: -1}, ids, src)
	c.SyncAll(context.Background())

	// 包含匹配（大小写不敏感）；s1/s2 共享同一驱动，每个源各命中一次
	hits := c.Search("README", "", 0)
	if len(hits) != 2 || hits[0].Name != "readme.txt" {
		t.Fatalf("README 搜索: %+v", hits)
	}
	// 中文（每源各一次）
	if hits := c.Search("报告", "", 0); len(hits) != 2 {
		t.Fatalf("中文搜索: %+v", hits)
	}
	// 按源过滤
	if hits := c.Search("txt", "other", 0); len(hits) != 0 {
		t.Fatalf("按源过滤应排除: %+v", hits)
	}
	if hits := c.Search("txt", "s1", 0); len(hits) != 2 {
		t.Fatalf("s1 内搜索: %+v", hits)
	}
	_ = c.Search("报告", "s1", 0)
	// limit 生效
	if hits := c.Search("txt", "", 1); len(hits) != 1 {
		t.Fatalf("limit: %+v", hits)
	}
	// 空查询
	if hits := c.Search("  ", "", 0); hits != nil {
		t.Fatal("空查询应返回 nil")
	}
	// 快照恢复后仍可搜
	dir := t.TempDir()
	c2 := New(dir, Config{SyncEvery: -1, Snapshot: true}, ids, src)
	c2.SyncAll(context.Background())
	c3 := New(dir, Config{SyncEvery: -1, Snapshot: true}, ids, src)
	if hits := c3.Search("notes", "s1", 0); len(hits) != 1 {
		t.Fatalf("快照恢复后搜索: %+v", hits)
	}
}
