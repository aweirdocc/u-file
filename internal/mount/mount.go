// Package mount 实现 M4.3 虚拟多挂载点树（VFS）架构。
//
// 核心能力（对齐 docs/06 5.1 节）：
//   - 打破“源即顶层目录”的单层映射限制，支持将任意源的任意子目录挂载到虚拟目录树的不同层级；
//   - 采用最长前缀匹配算法（Longest-Prefix Match）将虚拟路径路由到物理 (source_id, source_path)；
//   - 自动合成中间虚拟父目录（如挂载 /财务文档/合同 与 /财务文档/发票 时，自动合成 /财务文档 虚拟目录）；
//   - 完整实现 driver.FSDriver 与 driver.Writer 接口（虚拟源 ID 为 "@vfs"），无缝复用平台现有的浏览、预览、打包、上传与跨源流转能力。
package mount

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ufile/internal/driver"
)

const VirtualSourceID = "@vfs"

var (
	ErrMountNotFound   = errors.New("mount: 虚拟挂载点不存在")
	ErrMountConflict   = errors.New("mount: 挂载路径已存在")
	ErrMountReadOnly   = errors.New("mount: 该虚拟挂载点为只读模式，禁止修改或删除")
	ErrVirtualNavDir   = errors.New("mount: 当前处于虚拟导航目录，请进入具体挂载点目录后执行写操作")
	ErrInvalidMountDir = errors.New("mount: 非法挂载路径（不能为空或根目录，且不能包含 ..）")
)

// Mount 单条虚拟挂载点定义（持久化于 data/mounts.json）。
type Mount struct {
	ID         string    `json:"id"`
	MountPath  string    `json:"mount_path"`  // 以 / 开头的规范化虚拟路径，如 "/财务文档/合同"
	SourceID   string    `json:"source_id"`   // 物理文件源 ID
	SourcePath string    `json:"source_path"` // 源内相对路径，如 "2026/contracts"（"" 表示源根）
	Readonly   bool      `json:"readonly"`    // 是否只读挂载
	Remark     string    `json:"remark,omitempty"`
	CreatedAt  time.Time `json:"created_at"`

	cleanMount string // 内部无首尾 / 的路径，如 "财务文档/合同"
}

// RouteResult 虚拟路径路由解析结果。
type RouteResult struct {
	Mount        *Mount
	PhysicalPath string // 对应物理源内的相对路径
	SubPath      string // 挂载点内的相对子路径
}

// DriverResolver 按源 ID 获取底层驱动实例。
type DriverResolver func(sourceID string) (driver.FSDriver, error)

// Manager 管理虚拟挂载表，并直接实现 driver.FSDriver 与 driver.Writer。
type Manager struct {
	mu         sync.RWMutex
	file       string
	resolveDrv DriverResolver
	mounts     map[string]*Mount
	order      []string
}

func NewManager(dataDir string, resolveDrv DriverResolver) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		file:       filepath.Join(dataDir, "mounts.json"),
		resolveDrv: resolveDrv,
		mounts:     map[string]*Mount{},
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// NormalizeMountPath 规范化虚拟挂载路径，返回内部无首尾 / 的相对路径与带前导 / 的展示路径。
func NormalizeMountPath(raw string) (cleanRel string, displayPath string, err error) {
	s := strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return "", "", ErrInvalidMountDir
		}
	}
	cleaned := path.Clean("/" + s)
	rel := strings.TrimPrefix(cleaned, "/")
	if rel == "" || rel == "." {
		return "", "", ErrInvalidMountDir
	}
	return rel, "/" + rel, nil
}

type UpsertOptions struct {
	MountPath  string `json:"mount_path"`
	SourceID   string `json:"source_id"`
	SourcePath string `json:"source_path"`
	Readonly   bool   `json:"readonly"`
	Remark     string `json:"remark"`
}

// Add 新增虚拟挂载点。
func (m *Manager) Add(opt UpsertOptions) (*Mount, error) {
	cleanMount, displayMount, err := NormalizeMountPath(opt.MountPath)
	if err != nil {
		return nil, err
	}
	srcID := strings.TrimSpace(opt.SourceID)
	if srcID == "" || srcID == VirtualSourceID {
		return nil, errors.New("mount: 请选择有效的物理文件源")
	}
	srcPath, err := driver.CleanPath(strings.Trim(strings.TrimSpace(opt.SourcePath), "/"))
	if err != nil {
		return nil, fmt.Errorf("mount: 源内子路径非法: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.mounts {
		if existing.cleanMount == cleanMount {
			return nil, fmt.Errorf("%w: %s", ErrMountConflict, displayMount)
		}
	}

	var idBytes [6]byte
	_, _ = rand.Read(idBytes[:])
	mnt := &Mount{
		ID:         "mnt_" + hex.EncodeToString(idBytes[:]),
		MountPath:  displayMount,
		SourceID:   srcID,
		SourcePath: srcPath,
		Readonly:   opt.Readonly,
		Remark:     strings.TrimSpace(opt.Remark),
		CreatedAt:  time.Now(),
		cleanMount: cleanMount,
	}
	m.mounts[mnt.ID] = mnt
	m.order = append(m.order, mnt.ID)
	if err := m.saveLocked(); err != nil {
		delete(m.mounts, mnt.ID)
		m.order = m.order[:len(m.order)-1]
		return nil, err
	}
	cp := *mnt
	return &cp, nil
}

// Update 更新已有虚拟挂载点。
func (m *Manager) Update(id string, opt UpsertOptions) (*Mount, error) {
	cleanMount, displayMount, err := NormalizeMountPath(opt.MountPath)
	if err != nil {
		return nil, err
	}
	srcID := strings.TrimSpace(opt.SourceID)
	if srcID == "" || srcID == VirtualSourceID {
		return nil, errors.New("mount: 请选择有效的物理文件源")
	}
	srcPath, err := driver.CleanPath(strings.Trim(strings.TrimSpace(opt.SourcePath), "/"))
	if err != nil {
		return nil, fmt.Errorf("mount: 源内子路径非法: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.mounts[id]
	if !ok {
		return nil, ErrMountNotFound
	}
	for otherID, existing := range m.mounts {
		if otherID != id && existing.cleanMount == cleanMount {
			return nil, fmt.Errorf("%w: %s", ErrMountConflict, displayMount)
		}
	}

	target.MountPath = displayMount
	target.SourceID = srcID
	target.SourcePath = srcPath
	target.Readonly = opt.Readonly
	target.Remark = strings.TrimSpace(opt.Remark)
	target.cleanMount = cleanMount

	if err := m.saveLocked(); err != nil {
		return nil, err
	}
	cp := *target
	return &cp, nil
}

// Remove 删除挂载点。
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.mounts[id]; !ok {
		return ErrMountNotFound
	}
	delete(m.mounts, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return m.saveLocked()
}

// RemoveBySource 当某个物理源被删除时，级联清理关联的挂载点。
func (m *Manager) RemoveBySource(sourceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	kept := make([]string, 0, len(m.order))
	for _, id := range m.order {
		mnt := m.mounts[id]
		if mnt != nil && mnt.SourceID == sourceID {
			delete(m.mounts, id)
			changed = true
		} else {
			kept = append(kept, id)
		}
	}
	if changed {
		m.order = kept
		_ = m.saveLocked()
	}
}

// ListMounts 返回全部虚拟挂载点。
func (m *Manager) ListMounts() []*Mount {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Mount, 0, len(m.order))
	for _, id := range m.order {
		if mnt := m.mounts[id]; mnt != nil {
			cp := *mnt
			out = append(out, &cp)
		}
	}
	return out
}

// Count 返回当前配置的挂载点总数。
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.mounts)
}

// Resolve 采用前缀最长匹配算法（Longest-Prefix Match）解析虚拟路径。
func (m *Manager) Resolve(virtualPath string) (*RouteResult, error) {
	clean, err := driver.CleanPath(strings.TrimPrefix(strings.TrimSpace(virtualPath), "/"))
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	var best *Mount
	for _, id := range m.order {
		mnt := m.mounts[id]
		if mnt == nil {
			continue
		}
		if clean == mnt.cleanMount || strings.HasPrefix(clean, mnt.cleanMount+"/") {
			if best == nil || len(mnt.cleanMount) > len(best.cleanMount) {
				best = mnt
			}
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	sub := strings.TrimPrefix(clean, best.cleanMount)
	sub = strings.TrimPrefix(sub, "/")
	phys := joinRel(best.SourcePath, sub)
	return &RouteResult{
		Mount:        &cp,
		PhysicalPath: phys,
		SubPath:      sub,
	}, nil
}

// —— 实现 driver.FSDriver 与 driver.Writer（虚拟文件系统桥接层）——

func (m *Manager) Scheme() string { return "vfs" }

func (m *Manager) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		List:     true,
		Stat:     true,
		Download: true,
		Write:    true,
	}
}

func (m *Manager) List(ctx context.Context, dir string, opt driver.ListOptions) (*driver.ListResult, error) {
	cleanDir, err := driver.CleanPath(strings.TrimPrefix(strings.TrimSpace(dir), "/"))
	if err != nil {
		return nil, err
	}

	var entries []driver.Entry
	seenNames := map[string]bool{}
	var nextCursor string

	// 1. 若当前目录已命中某个挂载点，先列取该物理挂载源对应目录的内容
	route, err := m.Resolve(cleanDir)
	if err != nil {
		return nil, err
	}
	if route != nil {
		drv, err := m.resolveDrv(route.Mount.SourceID)
		if err != nil {
			return nil, fmt.Errorf("mount: 物理源 %s 不可用: %w", route.Mount.SourceID, err)
		}
		res, err := drv.List(ctx, route.PhysicalPath, opt)
		if err != nil {
			return nil, err
		}
		nextCursor = res.NextCursor
		for _, e := range res.Entries {
			vEntry := e
			vEntry.Path = joinRel(cleanDir, e.Name)
			entries = append(entries, vEntry)
			seenNames[e.Name] = true
		}
	}

	// 2. 合成位于 cleanDir 之下的子级虚拟挂载目录（支持多源层叠拼接）
	m.mu.RLock()
	var childMountNames []string
	childSet := map[string]bool{}
	prefix := ""
	if cleanDir != "" {
		prefix = cleanDir + "/"
	}
	for _, id := range m.order {
		mnt := m.mounts[id]
		if mnt == nil {
			continue
		}
		if cleanDir == "" || strings.HasPrefix(mnt.cleanMount, prefix) {
			rem := strings.TrimPrefix(mnt.cleanMount, prefix)
			if rem == "" {
				continue
			}
			firstSeg := strings.SplitN(rem, "/", 2)[0]
			if firstSeg != "" && !seenNames[firstSeg] && !childSet[firstSeg] {
				childSet[firstSeg] = true
				childMountNames = append(childMountNames, firstSeg)
			}
		}
	}
	m.mu.RUnlock()

	sort.Strings(childMountNames)
	for _, seg := range childMountNames {
		entries = append(entries, driver.Entry{
			Path:  joinRel(cleanDir, seg),
			Name:  seg,
			IsDir: true,
		})
	}

	if cleanDir != "" && route == nil && len(entries) == 0 {
		return nil, driver.ErrNotFound
	}
	return &driver.ListResult{Entries: entries, NextCursor: nextCursor}, nil
}

func (m *Manager) Stat(ctx context.Context, p string) (*driver.Entry, error) {
	clean, err := driver.CleanPath(strings.TrimPrefix(strings.TrimSpace(p), "/"))
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return &driver.Entry{Path: "", Name: "", IsDir: true}, nil
	}
	route, err := m.Resolve(clean)
	if err != nil {
		return nil, err
	}
	if route != nil {
		drv, err := m.resolveDrv(route.Mount.SourceID)
		if err != nil {
			return nil, err
		}
		if route.PhysicalPath == "" {
			return &driver.Entry{Path: clean, Name: path.Base(clean), IsDir: true}, nil
		}
		st, err := drv.Stat(ctx, route.PhysicalPath)
		if err != nil {
			return nil, err
		}
		cp := *st
		cp.Path = clean
		cp.Name = path.Base(clean)
		return &cp, nil
	}

	// 检查是否为某个挂载点的虚拟祖先目录
	m.mu.RLock()
	isAncestor := false
	prefix := clean + "/"
	for _, mnt := range m.mounts {
		if mnt != nil && strings.HasPrefix(mnt.cleanMount, prefix) {
			isAncestor = true
			break
		}
	}
	m.mu.RUnlock()
	if isAncestor {
		return &driver.Entry{Path: clean, Name: path.Base(clean), IsDir: true}, nil
	}
	return nil, driver.ErrNotFound
}

func (m *Manager) Open(ctx context.Context, p string, opt driver.OpenOptions) (io.ReadCloser, error) {
	route, err := m.Resolve(p)
	if err != nil {
		return nil, err
	}
	if route == nil {
		return nil, driver.ErrNotFound
	}
	drv, err := m.resolveDrv(route.Mount.SourceID)
	if err != nil {
		return nil, err
	}
	return drv.Open(ctx, route.PhysicalPath, opt)
}

func (m *Manager) resolveWritable(p string) (*RouteResult, driver.Writer, error) {
	route, err := m.Resolve(p)
	if err != nil {
		return nil, nil, err
	}
	if route == nil {
		return nil, nil, ErrVirtualNavDir
	}
	if route.Mount.Readonly {
		return nil, nil, ErrMountReadOnly
	}
	drv, err := m.resolveDrv(route.Mount.SourceID)
	if err != nil {
		return nil, nil, err
	}
	w, ok := drv.(driver.Writer)
	if !ok {
		return nil, nil, driver.ErrNotSupported
	}
	return route, w, nil
}

func (m *Manager) Upload(ctx context.Context, p string, r io.Reader, size int64) error {
	route, w, err := m.resolveWritable(p)
	if err != nil {
		return err
	}
	if route.SubPath == "" {
		return driver.ErrInvalidPath
	}
	return w.Upload(ctx, route.PhysicalPath, r, size)
}

func (m *Manager) Mkdir(ctx context.Context, p string) error {
	route, w, err := m.resolveWritable(p)
	if err != nil {
		return err
	}
	if route.SubPath == "" {
		return driver.ErrInvalidPath
	}
	return w.Mkdir(ctx, route.PhysicalPath)
}

func (m *Manager) Delete(ctx context.Context, p string) error {
	route, w, err := m.resolveWritable(p)
	if err != nil {
		return err
	}
	if route.SubPath == "" {
		return errors.New("mount: 不能在虚拟视图中直接删除挂载点根目录，请前往挂载管理移除")
	}
	return w.Delete(ctx, route.PhysicalPath)
}

func (m *Manager) Rename(ctx context.Context, from, to string) error {
	rFrom, wFrom, err := m.resolveWritable(from)
	if err != nil {
		return err
	}
	rTo, wTo, err := m.resolveWritable(to)
	if err != nil {
		return err
	}
	if rFrom.SubPath == "" || rTo.SubPath == "" {
		return driver.ErrInvalidPath
	}
	if rFrom.Mount.SourceID == rTo.Mount.SourceID {
		return wFrom.Rename(ctx, rFrom.PhysicalPath, rTo.PhysicalPath)
	}
	// 跨物理源的挂载点间移动：流式复制后删除源
	srcDrv, err := m.resolveDrv(rFrom.Mount.SourceID)
	if err != nil {
		return err
	}
	st, err := srcDrv.Stat(ctx, rFrom.PhysicalPath)
	if err != nil {
		return err
	}
	if st.IsDir {
		return errors.New("mount: 跨不同物理源的目录移动请使用「移动到…」传输任务")
	}
	rc, err := srcDrv.Open(ctx, rFrom.PhysicalPath, driver.OpenOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := wTo.Upload(ctx, rTo.PhysicalPath, rc, st.Size); err != nil {
		return err
	}
	return wFrom.Delete(ctx, rFrom.PhysicalPath)
}

func (m *Manager) load() error {
	raw, err := os.ReadFile(m.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []*Mount
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("mount: 解析 %s 失败: %w", m.file, err)
	}
	for _, mnt := range list {
		if mnt == nil || mnt.ID == "" {
			continue
		}
		clean, display, err := NormalizeMountPath(mnt.MountPath)
		if err != nil {
			continue
		}
		mnt.cleanMount = clean
		mnt.MountPath = display
		m.mounts[mnt.ID] = mnt
		m.order = append(m.order, mnt.ID)
	}
	return nil
}

func (m *Manager) saveLocked() error {
	list := make([]*Mount, 0, len(m.order))
	for _, id := range m.order {
		if mnt := m.mounts[id]; mnt != nil {
			list = append(list, mnt)
		}
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.file)
}

func joinRel(dir, name string) string {
	dir = strings.Trim(dir, "/")
	name = strings.Trim(name, "/")
	if dir == "" {
		return name
	}
	if name == "" {
		return dir
	}
	return dir + "/" + name
}
