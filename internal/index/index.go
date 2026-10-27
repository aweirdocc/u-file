// Package index M1 元数据索引缓存：目录列表 TTL 缓存 + 后台定时全量同步。
//
// 职责（对应 docs/02 P0.4）：
//   - 目录懒加载缓存：List 命中缓存直接返回（<100ms），未命中回源驱动并写缓存；
//   - 后台同步：定时把每个源的目录树刷成快照（滚动全量，逐目录刷新），
//     同步任务状态可见（上次成功时间、失败原因、正在同步哪个目录）；
//   - 快照持久化：data/index.json，重启后缓存仍在（浏览秒开），后台再刷新保新鲜。
//
// 一致性策略：缓存仅加速"浏览"路径；下载/预览/Stat 永远回源（不做缓存），
// 保证字节级正确性。前端手动刷新目录时带 ?refresh=1 绕过缓存。
package index

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ufile/internal/driver"
)

// Config 索引配置。
type Config struct {
	TTL         time.Duration // 目录缓存存活时间；0 = 默认 10 分钟
	SyncEvery   time.Duration // 后台全量同步周期；0 = 默认 1 小时，<0 = 禁用后台同步
	Snapshot    bool          // 是否持久化快照到 data/index.json
	MaxEntries  int           // 缓存目录条目上限（超出不缓存，防止单目录百万级撑爆内存）
}

// Status 单源同步状态。
type Status struct {
	SourceID    string     `json:"source_id"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	Running     bool       `json:"running"`
	Current     string     `json:"current_dir,omitempty"` // 正在同步的目录
	Dirs        int        `json:"dirs"`                  // 已知目录数
	Entries     int        `json:"entries"`               // 已知条目数（文件）
	SyncedDirs  int        `json:"synced_dirs"`           // 本轮已刷目录数
}

// Cache 索引缓存。并发安全；单个源损坏不影响其他源同步。
type Cache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	maxEnt  int
	cfg     Config
	dirs    map[string]*dirCache // key: sourceID + "\x00" + dir
	status  map[string]*Status   // sourceID → 同步状态
	file    string               // data/index.json；空 = 不持久化
	listIDs func() []string                  // 枚举全部源 ID（装配时接 source.Manager）
	getSrc  func(id string) (driver.FSDriver, bool)
	cancel  context.CancelFunc
	started bool
}

type dirCache struct {
	At  time.Time          `json:"at"`
	Res *driver.ListResult `json:"res"`
}

// New 构造缓存。getSrc 用于按源 ID 取驱动（解耦 source.Manager）。
func New(dataDir string, cfg Config, listIDs func() []string, getSrc func(id string) (driver.FSDriver, bool)) *Cache {
	if cfg.TTL <= 0 {
		cfg.TTL = 10 * time.Minute
	}
	c := &Cache{
		ttl:    cfg.TTL,
		maxEnt: cfg.MaxEntries,
		dirs:   map[string]*dirCache{},
		status: map[string]*Status{},
		listIDs: listIDs,
		getSrc:  getSrc,
	}
	if cfg.Snapshot {
		c.file = filepath.Join(dataDir, "index.json")
		c.loadSnapshot()
	}
	c.cfg = cfg
	return c
}

// Start 启动后台同步循环（幂等）。返回即开始第一轮同步（异步）。
func (c *Cache) Start(ctx context.Context) {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	interval := c.cfg.SyncEvery
	if interval == 0 {
		interval = time.Hour
	}
	c.mu.Unlock()
	ctx2, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	go func() {
		c.SyncAll(ctx2) // 启动先刷一轮
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx2.Done():
				return
			case <-t.C:
				c.SyncAll(ctx2)
			}
		}
	}()
}

// Stop 停止后台同步。
func (c *Cache) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

// List 带缓存的目录列表。refresh=true 绕过缓存回源并刷新缓存。
func (c *Cache) List(ctx context.Context, sourceID string, drv driver.FSDriver, dir string, opt driver.ListOptions, refresh bool) (*driver.ListResult, error) {
	// 游标分页请求不缓存（S3 continuation token 与快照不兼容，回源直查）
	if opt.Cursor != "" {
		return drv.List(ctx, dir, opt)
	}
	key := sourceID + "\x00" + dir
	if !refresh {
		c.mu.RLock()
		hit, ok := c.dirs[key]
		c.mu.RUnlock()
		if ok && time.Since(hit.At) < c.ttl {
			return hit.Res, nil
		}
	}
	res, err := drv.List(ctx, dir, opt)
	if err != nil {
		return nil, err
	}
	if c.maxEnt <= 0 || len(res.Entries) <= c.maxEnt {
		c.mu.Lock()
		c.dirs[key] = &dirCache{At: time.Now(), Res: res}
		c.mu.Unlock()
	}
	return res, nil
}

// Invalidate 清除某源（或全部，sourceID=""）的缓存。
func (c *Cache) Invalidate(sourceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.dirs {
		if sourceID == "" || strings.HasPrefix(k, sourceID+"\x00") {
			delete(c.dirs, k)
		}
	}
}

// —— 后台同步 ——

// SyncSource 立即同步单个源（手动触发入口）。
func (c *Cache) SyncSource(ctx context.Context, sourceID string, drv driver.FSDriver) {
	c.syncSource(ctx, sourceID, drv)
	c.saveSnapshot()
}

// SyncAll 对全部源做一轮滚动全量同步（逐目录 List 刷新缓存）。
// 单源失败记录状态并继续其余源。
func (c *Cache) SyncAll(ctx context.Context) {
	for _, id := range c.sourceIDs() {
		if ctx.Err() != nil {
			return
		}
		drv, ok := c.getSrc(id)
		if !ok {
			continue // 源已删除
		}
		c.syncSource(ctx, id, drv)
	}
	c.saveSnapshot()
}

// sourceIDs 返回当前需要同步的源（从已知状态 + 缓存键推导，含新源）。
func (c *Cache) sourceIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, id := range c.listIDs() {
		seen[id] = true
		out = append(out, id)
	}
	for id := range c.status {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for k := range c.dirs {
		id := strings.SplitN(k, "\x00", 2)[0]
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Touch 注册一个源参与后台同步（源新增/首次浏览时调用）。
func (c *Cache) Touch(sourceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.status[sourceID]; !ok {
		c.status[sourceID] = &Status{SourceID: sourceID}
	}
}

// Forget 移除源的缓存与状态（源删除时调用）。
func (c *Cache) Forget(sourceID string) {
	c.Invalidate(sourceID)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.status, sourceID)
}

// syncSource 广度优先遍历目录树，逐目录刷新缓存。限制：单目录 1000 条、
// 总目录 10000 个、总耗时 10 分钟（防坏源/巨型树拖死后台）。
func (c *Cache) syncSource(ctx context.Context, sourceID string, drv driver.FSDriver) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	c.mu.Lock()
	st, ok := c.status[sourceID]
	if !ok {
		st = &Status{SourceID: sourceID}
		c.status[sourceID] = st
	}
	st.Running = true
	st.Current = ""
	st.SyncedDirs = 0
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		st.Running = false
		st.Current = ""
		c.mu.Unlock()
	}()

	queue := []string{""}
	visited := map[string]bool{"": true}
	for len(queue) > 0 && ctx.Err() == nil {
		dir := queue[0]
		queue = queue[1:]
		if len(visited) > 10000 {
			break
		}
		c.mu.Lock()
		st.Current = dir
		c.mu.Unlock()

		res, err := c.List(ctx, sourceID, drv, dir, driver.ListOptions{Limit: 1000}, true)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return // 停机中，不记为失败
			}
			c.mu.Lock()
			now := time.Now()
			st.LastError = err.Error()
			st.LastErrorAt = &now
			c.mu.Unlock()
			log.Printf("[warn] 索引同步 %s/%s: %v", sourceID, dir, err)
			return // 本轮该源中止，下轮重试
		}
		visited[dir] = true
		for _, e := range res.Entries {
			if e.IsDir && !visited[e.Path] {
				queue = append(queue, e.Path)
			}
		}
		c.mu.Lock()
		st.SyncedDirs = len(visited)
		c.mu.Unlock()
	}
	if ctx.Err() == nil {
		c.mu.Lock()
		now := time.Now()
		st.LastSuccess = &now
		st.LastError = ""
		st.Dirs = len(visited)
		c.mu.Unlock()
	}
}

// StatusOf 返回某源同步状态（无记录返回零值状态）。
func (c *Cache) StatusOf(sourceID string) Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if st, ok := c.status[sourceID]; ok {
		return *st
	}
	return Status{SourceID: sourceID}
}

// AllStatus 返回全部源的同步状态。
func (c *Cache) AllStatus() []Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Status, 0, len(c.status))
	for _, id := range c.sortedStatusIDs() {
		out = append(out, *c.status[id])
	}
	return out
}

func (c *Cache) sortedStatusIDs() []string {
	ids := make([]string, 0, len(c.status))
	for id := range c.status {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// —— 快照持久化 ——

type snapshot struct {
	Version int                  `json:"version"`
	Saved   time.Time            `json:"saved_at"`
	Dirs    map[string]*dirCache `json:"dirs"`
}

func (c *Cache) loadSnapshot() {
	raw, err := os.ReadFile(c.file)
	if err != nil {
		return
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		log.Printf("[warn] 索引快照损坏，忽略: %v", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range snap.Dirs {
		if v != nil && v.Res != nil {
			c.dirs[k] = v
		}
	}
	log.Printf("[info] 已加载索引快照：%d 个目录", len(snap.Dirs))
}

func (c *Cache) saveSnapshot() {
	if c.file == "" {
		return
	}
	c.mu.RLock()
	snap := snapshot{Version: 1, Saved: time.Now(), Dirs: c.dirs}
	raw, err := json.Marshal(&snap)
	c.mu.RUnlock()
	if err != nil {
		return
	}
	tmp := c.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[warn] 索引快照写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, c.file); err != nil {
		log.Printf("[warn] 索引快照替换失败: %v", err)
	}
}
