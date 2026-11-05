// Package task 实现 M4.1 传输任务中心与跨源流式复制/移动调度引擎。
//
// 核心特性：
//   - 状态机：pending -> running -> paused / retrying -> success / failed / canceled；
//   - Worker Pool：全局并发控制 + 单源并发门控 + 可选全局带宽令牌桶限速；
//   - 零落盘流式管道：异构源间通过 io.Pipe + ProgressReader 边拉边推，内存恒定（~32KB）；
//   - 协议级直拷短路（Fast Path）：同源移动直接调 Writer.Rename，同源复制优先调 Copier.Copy（如 S3 CopyObject）；
//   - 移动事务补偿：目标端写入后强校验 Stat 大小，100% 一致才删除源端；中途失败自动清理目标端残留；
//   - 断点续传：目录/多文件任务逐项记录 completed_items，暂停恢复或失败重试时自动跳过已传完的文件；
//   - 持久化与实时推送：原子落盘 data/tasks.json，SSE 广播实时速率与进度。
package task

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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ufile/internal/driver"
)

type TaskType string

const (
	TypeCopy TaskType = "copy"
	TypeMove TaskType = "move"
)

type TaskStatus string

const (
	StatusPending  TaskStatus = "pending"
	StatusRunning  TaskStatus = "running"
	StatusPaused   TaskStatus = "paused"
	StatusRetrying TaskStatus = "retrying"
	StatusSuccess  TaskStatus = "success"
	StatusFailed   TaskStatus = "failed"
	StatusCanceled TaskStatus = "canceled"
)

var (
	ErrTaskNotFound   = errors.New("task: 任务不存在")
	ErrInvalidState   = errors.New("task: 当前任务状态不允许此操作")
	ErrSourceReadOnly = errors.New("task: 目标源不支持写操作，或移动时源端不支持删除")
	errPausedByUser   = errors.New("task: paused by user")
	errCanceledByUser = errors.New("task: canceled by user")
)

// Task 传输任务持久化与 API 视图模型。
type Task struct {
	ID               string          `json:"id"`
	Type             TaskType        `json:"type"`
	UserID           string          `json:"user_id"`
	SourceID         string          `json:"source_id"`
	TargetSourceID   string          `json:"target_source_id"`
	Paths            []string        `json:"paths"`
	SrcPath          string          `json:"src_path"`
	DstDir           string          `json:"dst_dir"`
	DstPath          string          `json:"dst_path"`
	TotalBytes       int64           `json:"total_bytes"`
	TransferredBytes int64           `json:"transferred_bytes"`
	SpeedBps         int64           `json:"speed_bps"`
	TotalItems       int             `json:"total_items"`
	DoneItems        int             `json:"done_items"`
	CurrentItem      string          `json:"current_item,omitempty"`
	Status           TaskStatus      `json:"status"`
	RetryCount       int             `json:"retry_count"`
	MaxRetries       int             `json:"max_retries"`
	ErrorMsg         string          `json:"error_msg,omitempty"`
	CompletedItems   map[string]bool `json:"completed_items,omitempty"`
	CreatedAt        int64           `json:"created_at"`
	UpdatedAt        int64           `json:"updated_at"`
}

// Clone 返回任务的深拷贝快照（并发安全外发）。
func (t *Task) Clone() *Task {
	cp := *t
	if t.Paths != nil {
		cp.Paths = make([]string, len(t.Paths))
		copy(cp.Paths, t.Paths)
	}
	if t.CompletedItems != nil {
		cp.CompletedItems = make(map[string]bool, len(t.CompletedItems))
		for k, v := range t.CompletedItems {
			cp.CompletedItems[k] = v
		}
	}
	return &cp
}

// Config 任务管理器配置。
type Config struct {
	Workers         int           // 全局并发 Worker 数（默认 4）
	MaxPerSource    int           // 单源最大并发任务数（默认 2）
	MaxRetries      int           // 默认最大重试次数（默认 3）
	RetryBaseDelay  time.Duration // 重试基础退避间隔（默认 500ms）
	MaxBandwidthBps int64         // 全局带宽上限（字节/秒，<=0 不限速）
}

// DriverResolver 根据源 ID 解析底层驱动实例。
type DriverResolver func(sourceID string) (driver.FSDriver, error)

// HookFunc 任务完成或变更时的回调钩子（如失效索引缓存、触发 Webhook、写审计）。
type HookFunc func(t *Task)

// Manager 传输任务调度中心。
type Manager struct {
	mu          sync.RWMutex
	file        string
	cfg         Config
	resolveDrv  DriverResolver
	onCompleted HookFunc

	tasks      map[string]*Task
	order      []string
	runningCtx map[string]context.CancelCauseFunc
	srcSem     map[string]chan struct{}

	queue  chan string
	stopCh chan struct{}
	wg     sync.WaitGroup
	closed atomic.Bool

	// SSE 订阅管理
	subMu sync.Mutex
	subs  map[chan []*Task]string // chan -> username ("" 表示不过滤或管理员)

	// 全局带宽令牌桶
	rateMu     sync.Mutex
	rateTokens float64
	rateLast   time.Time
}

// NewManager 初始化任务中心并恢复持久化任务。
func NewManager(dataDir string, cfg Config, resolveDrv DriverResolver, onCompleted HookFunc) (*Manager, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.MaxPerSource <= 0 {
		cfg.MaxPerSource = 2
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = 500 * time.Millisecond
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		file:        filepath.Join(dataDir, "tasks.json"),
		cfg:         cfg,
		resolveDrv:  resolveDrv,
		onCompleted: onCompleted,
		tasks:       map[string]*Task{},
		runningCtx:  map[string]context.CancelCauseFunc{},
		srcSem:      map[string]chan struct{}{},
		queue:       make(chan string, 1024),
		stopCh:      make(chan struct{}),
		subs:        map[chan []*Task]string{},
		rateLast:    time.Now(),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	for range m.cfg.Workers {
		m.wg.Add(1)
		go m.worker()
	}
	// 将启动时处于 pending 的任务重新入队
	m.mu.RLock()
	for _, id := range m.order {
		if t := m.tasks[id]; t != nil && t.Status == StatusPending {
			m.enqueue(id)
		}
	}
	m.mu.RUnlock()
	return m, nil
}

// Stop 优雅停机：取消执行中任务并将其标记为 paused 以便下次启动恢复。
func (m *Manager) Stop() {
	if !m.closed.CompareAndSwap(false, true) {
		return
	}
	close(m.stopCh)
	m.mu.Lock()
	for _, cancel := range m.runningCtx {
		cancel(errPausedByUser)
	}
	m.mu.Unlock()
	m.wg.Wait()

	m.mu.Lock()
	for _, t := range m.tasks {
		if t.Status == StatusRunning || t.Status == StatusRetrying {
			t.Status = StatusPaused
			t.SpeedBps = 0
			t.UpdatedAt = time.Now().Unix()
		}
	}
	_ = m.saveLocked()
	m.mu.Unlock()
}

// TransferRequest 创建跨源/同源复制或移动任务的请求参数。
type TransferRequest struct {
	Type           TaskType
	UserID         string
	SourceID       string
	TargetSourceID string
	Paths          []string
	DstDir         string
	MaxRetries     int
}

// Submit 提交一个新的复制或移动任务。
func (m *Manager) Submit(req TransferRequest) (*Task, error) {
	if req.Type != TypeCopy && req.Type != TypeMove {
		return nil, fmt.Errorf("task: 不支持的任务类型 %q", req.Type)
	}
	if req.SourceID == "" || req.TargetSourceID == "" {
		return nil, errors.New("task: 缺少源或目标源 ID")
	}
	cleanPaths := make([]string, 0, len(req.Paths))
	for _, p := range req.Paths {
		cp, err := driver.CleanPath(p)
		if err != nil || cp == "" {
			return nil, fmt.Errorf("task: 非法源路径 %q", p)
		}
		cleanPaths = append(cleanPaths, cp)
	}
	if len(cleanPaths) == 0 {
		return nil, errors.New("task: 至少需要一个文件或目录路径")
	}
	dstDir, err := driver.CleanPath(req.DstDir)
	if err != nil {
		return nil, fmt.Errorf("task: 非法目标目录 %q", req.DstDir)
	}

	// 校验源与目标源能力
	srcDrv, err := m.resolveDrv(req.SourceID)
	if err != nil {
		return nil, fmt.Errorf("task: 源 %s 不可用: %w", req.SourceID, err)
	}
	dstDrv, err := m.resolveDrv(req.TargetSourceID)
	if err != nil {
		return nil, fmt.Errorf("task: 目标源 %s 不可用: %w", req.TargetSourceID, err)
	}
	if _, ok := dstDrv.(driver.Writer); !ok {
		return nil, ErrSourceReadOnly
	}
	if req.Type == TypeMove {
		if _, ok := srcDrv.(driver.Writer); !ok {
			return nil, ErrSourceReadOnly
		}
	}

	// 同源同目录自覆盖或把目录移入自身子目录保护
	if req.SourceID == req.TargetSourceID {
		for _, sp := range cleanPaths {
			targetPath := joinRel(dstDir, path.Base(sp))
			if sp == targetPath {
				return nil, fmt.Errorf("task: 源路径与目标路径相同 (%s)", sp)
			}
			if strings.HasPrefix(targetPath, sp+"/") {
				return nil, fmt.Errorf("task: 不能将目录 %q 复制或移动到其自身子目录 %q 下", sp, targetPath)
			}
		}
	}

	srcLabel := cleanPaths[0]
	dstLabel := joinRel(dstDir, path.Base(cleanPaths[0]))
	if len(cleanPaths) > 1 {
		srcLabel = fmt.Sprintf("%s 等 %d 项", path.Base(cleanPaths[0]), len(cleanPaths))
		if dstDir == "" {
			dstLabel = "/"
		} else {
			dstLabel = dstDir + "/"
		}
	}

	maxRetries := m.cfg.MaxRetries
	if req.MaxRetries > 0 {
		maxRetries = req.MaxRetries
	}
	now := time.Now().Unix()
	t := &Task{
		ID:             newTaskID(),
		Type:           req.Type,
		UserID:         req.UserID,
		SourceID:       req.SourceID,
		TargetSourceID: req.TargetSourceID,
		Paths:          cleanPaths,
		SrcPath:        srcLabel,
		DstDir:         dstDir,
		DstPath:        dstLabel,
		Status:         StatusPending,
		MaxRetries:     maxRetries,
		CompletedItems: map[string]bool{},
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	m.mu.Lock()
	m.tasks[t.ID] = t
	m.order = append([]string{t.ID}, m.order...) // 最新任务排最前
	_ = m.saveLocked()
	cp := t.Clone()
	m.mu.Unlock()

	m.broadcast()
	m.enqueue(t.ID)
	return cp, nil
}

func (m *Manager) enqueue(id string) {
	if m.closed.Load() {
		return
	}
	select {
	case m.queue <- id:
	default:
		go func() {
			select {
			case m.queue <- id:
			case <-m.stopCh:
			}
		}()
	}
}

// List 返回任务列表（userFilter 非空且 isAdmin=false 时仅返回该用户的任务）。
func (m *Manager) List(userFilter string, isAdmin bool) []*Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Task, 0, len(m.order))
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil {
			continue
		}
		if !isAdmin && userFilter != "" && t.UserID != userFilter {
			continue
		}
		out = append(out, t.Clone())
	}
	return out
}

// Get 获取单个任务详情。
func (m *Manager) Get(id string) (*Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return t.Clone(), nil
}

// Pause 暂停正在运行或排队的任务。
func (m *Manager) Pause(id string) (*Task, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrTaskNotFound
	}
	if t.Status != StatusRunning && t.Status != StatusPending && t.Status != StatusRetrying {
		m.mu.Unlock()
		return nil, ErrInvalidState
	}
	t.Status = StatusPaused
	t.SpeedBps = 0
	t.UpdatedAt = time.Now().Unix()
	if cancel, ok := m.runningCtx[id]; ok {
		cancel(errPausedByUser)
	}
	_ = m.saveLocked()
	cp := t.Clone()
	m.mu.Unlock()

	m.broadcast()
	return cp, nil
}

// Resume 恢复已暂停的任务。
func (m *Manager) Resume(id string) (*Task, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrTaskNotFound
	}
	if t.Status != StatusPaused {
		m.mu.Unlock()
		return nil, ErrInvalidState
	}
	t.Status = StatusPending
	t.ErrorMsg = ""
	t.UpdatedAt = time.Now().Unix()
	_ = m.saveLocked()
	cp := t.Clone()
	m.mu.Unlock()

	m.broadcast()
	m.enqueue(id)
	return cp, nil
}

// Cancel 取消未完成的任务。
func (m *Manager) Cancel(id string) (*Task, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrTaskNotFound
	}
	if t.Status == StatusSuccess || t.Status == StatusCanceled {
		m.mu.Unlock()
		return nil, ErrInvalidState
	}
	t.Status = StatusCanceled
	t.SpeedBps = 0
	t.UpdatedAt = time.Now().Unix()
	if cancel, ok := m.runningCtx[id]; ok {
		cancel(errCanceledByUser)
	}
	_ = m.saveLocked()
	cp := t.Clone()
	m.mu.Unlock()

	m.broadcast()
	return cp, nil
}

// Retry 手动重试失败或已取消的任务（保留已完成的 completed_items 子项）。
func (m *Manager) Retry(id string) (*Task, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrTaskNotFound
	}
	if t.Status != StatusFailed && t.Status != StatusCanceled {
		m.mu.Unlock()
		return nil, ErrInvalidState
	}
	t.Status = StatusPending
	t.RetryCount = 0
	t.ErrorMsg = ""
	t.SpeedBps = 0
	t.UpdatedAt = time.Now().Unix()
	_ = m.saveLocked()
	cp := t.Clone()
	m.mu.Unlock()

	m.broadcast()
	m.enqueue(id)
	return cp, nil
}

// Remove 删除单条任务（若正在运行则先取消）。
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	_, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return ErrTaskNotFound
	}
	if cancel, running := m.runningCtx[id]; running {
		cancel(errCanceledByUser)
	}
	delete(m.tasks, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	_ = m.saveLocked()
	m.mu.Unlock()

	m.broadcast()
	return nil
}

// ClearFinished 清理全部已结束（success / failed / canceled）的任务。
func (m *Manager) ClearFinished(userFilter string, isAdmin bool) int {
	m.mu.Lock()
	removed := 0
	kept := make([]string, 0, len(m.order))
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil {
			continue
		}
		if !isAdmin && userFilter != "" && t.UserID != userFilter {
			kept = append(kept, id)
			continue
		}
		if t.Status == StatusSuccess || t.Status == StatusFailed || t.Status == StatusCanceled {
			delete(m.tasks, id)
			removed++
		} else {
			kept = append(kept, id)
		}
	}
	m.order = kept
	if removed > 0 {
		_ = m.saveLocked()
	}
	m.mu.Unlock()
	if removed > 0 {
		m.broadcast()
	}
	return removed
}

// —— Worker 与任务执行逻辑 ——

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case id := <-m.queue:
			m.runTask(id)
		}
	}
}

func (m *Manager) acquireSourceLocks(srcID, dstID string) func() {
	first, second := srcID, dstID
	if first > second {
		first, second = second, first
	}
	s1 := m.getSem(first)
	s1 <- struct{}{}
	if second != first {
		s2 := m.getSem(second)
		s2 <- struct{}{}
		return func() {
			<-s2
			<-s1
		}
	}
	return func() { <-s1 }
}

func (m *Manager) getSem(sourceID string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.srcSem[sourceID]
	if !ok {
		ch = make(chan struct{}, m.cfg.MaxPerSource)
		m.srcSem[sourceID] = ch
	}
	return ch
}

type transferItem struct {
	srcPath string
	dstPath string
	isDir   bool
	size    int64
}

func (m *Manager) runTask(id string) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || t.Status != StatusPending {
		m.mu.Unlock()
		return
	}
	srcID, dstID := t.SourceID, t.TargetSourceID
	m.mu.Unlock()

	release := m.acquireSourceLocks(srcID, dstID)
	defer release()

	for {
		if m.closed.Load() {
			return
		}
		m.mu.Lock()
		t, ok = m.tasks[id]
		if !ok || (t.Status != StatusPending && t.Status != StatusRetrying) {
			m.mu.Unlock()
			return
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		m.runningCtx[id] = cancel
		t.Status = StatusRunning
		t.ErrorMsg = ""
		t.UpdatedAt = time.Now().Unix()
		if t.CompletedItems == nil {
			t.CompletedItems = map[string]bool{}
		}
		_ = m.saveLocked()
		m.mu.Unlock()
		m.broadcast()

		err := m.executeTransfer(ctx, id)
		cancel(nil)

		m.mu.Lock()
		delete(m.runningCtx, id)
		t, ok = m.tasks[id]
		if !ok {
			m.mu.Unlock()
			return
		}
		t.SpeedBps = 0
		t.CurrentItem = ""
		t.UpdatedAt = time.Now().Unix()

		if err == nil {
			t.Status = StatusSuccess
			t.TransferredBytes = t.TotalBytes
			t.DoneItems = t.TotalItems
			t.ErrorMsg = ""
			_ = m.saveLocked()
			cp := t.Clone()
			m.mu.Unlock()
			m.broadcast()
			if m.onCompleted != nil {
				m.onCompleted(cp)
			}
			return
		}

		cause := context.Cause(ctx)
		if errors.Is(cause, errPausedByUser) || errors.Is(err, errPausedByUser) || t.Status == StatusPaused {
			t.Status = StatusPaused
			_ = m.saveLocked()
			m.mu.Unlock()
			m.broadcast()
			return
		}
		if errors.Is(cause, errCanceledByUser) || errors.Is(err, errCanceledByUser) || t.Status == StatusCanceled {
			t.Status = StatusCanceled
			_ = m.saveLocked()
			m.mu.Unlock()
			m.broadcast()
			return
		}

		// 检查是否可自动重试
		if t.RetryCount < t.MaxRetries {
			t.RetryCount++
			t.Status = StatusRetrying
			t.ErrorMsg = fmt.Sprintf("第 %d/%d 次重试中 (%v)", t.RetryCount, t.MaxRetries, err)
			backoff := m.cfg.RetryBaseDelay * time.Duration(1<<(t.RetryCount-1))
			if backoff > 10*time.Second {
				backoff = 10 *time.Second
			}
			_ = m.saveLocked()
			m.mu.Unlock()
			m.broadcast()

			select {
			case <-time.After(backoff):
				continue
			case <-m.stopCh:
				return
			}
		}

		t.Status = StatusFailed
		t.ErrorMsg = err.Error()
		_ = m.saveLocked()
		m.mu.Unlock()
		m.broadcast()
		return
	}
}

func (m *Manager) executeTransfer(ctx context.Context, id string) error {
	m.mu.RLock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.RUnlock()
		return ErrTaskNotFound
	}
	taskCopy := t.Clone()
	m.mu.RUnlock()

	srcDrv, err := m.resolveDrv(taskCopy.SourceID)
	if err != nil {
		return fmt.Errorf("解析源 %s 失败: %w", taskCopy.SourceID, err)
	}
	dstDrv, err := m.resolveDrv(taskCopy.TargetSourceID)
	if err != nil {
		return fmt.Errorf("解析目标源 %s 失败: %w", taskCopy.TargetSourceID, err)
	}
	dstWriter, ok := dstDrv.(driver.Writer)
	if !ok {
		return ErrSourceReadOnly
	}
	var srcWriter driver.Writer
	if taskCopy.Type == TypeMove {
		srcWriter, ok = srcDrv.(driver.Writer)
		if !ok {
			return ErrSourceReadOnly
		}
	}

	// Fast Path 1：同源移动 -> 直接逐路径调用 Rename
	if taskCopy.SourceID == taskCopy.TargetSourceID && taskCopy.Type == TypeMove {
		return m.execSameSourceMove(ctx, id, srcDrv, dstWriter, taskCopy)
	}

	// Fast Path 2：同源复制且驱动实现了 driver.Copier -> 直接逐路径调用 Copy
	if taskCopy.SourceID == taskCopy.TargetSourceID && taskCopy.Type == TypeCopy {
		if copier, ok := srcDrv.(driver.Copier); ok {
			return m.execSameSourceCopy(ctx, id, srcDrv, copier, taskCopy)
		}
	}

	// 标准流式传输：展开目录树 -> 统计总字节与文件清单 -> 流式管道拷贝 -> 移动校验后清理源
	items, totalBytes, err := m.collectItems(ctx, srcDrv, taskCopy.Paths, taskCopy.DstDir)
	if err != nil {
		return err
	}

	// 计算已完成项的字节与计数基线（支持断点续传）
	var alreadyBytes int64
	var alreadyDone int
	for _, it := range items {
		if taskCopy.CompletedItems[it.srcPath] {
			alreadyDone++
			if !it.isDir && it.size > 0 {
				alreadyBytes += it.size
			}
		}
	}

	m.mu.Lock()
	if cur, exists := m.tasks[id]; exists {
		cur.TotalBytes = totalBytes
		cur.TotalItems = len(items)
		cur.TransferredBytes = alreadyBytes
		cur.DoneItems = alreadyDone
	}
	m.mu.Unlock()
	m.broadcast()

	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		m.mu.RLock()
		done := m.tasks[id] != nil && m.tasks[id].CompletedItems[it.srcPath]
		m.mu.RUnlock()
		if done {
			continue
		}

		m.mu.Lock()
		if cur, exists := m.tasks[id]; exists {
			cur.CurrentItem = it.srcPath
		}
		m.mu.Unlock()
		m.broadcast()

		if it.isDir {
			if err := dstWriter.Mkdir(ctx, it.dstPath); err != nil {
				return fmt.Errorf("创建目标目录 %s 失败: %w", it.dstPath, err)
			}
		} else {
			if err := m.streamSingleFile(ctx, id, srcDrv, dstDrv, dstWriter, it); err != nil {
				return err
			}
			// 若为跨源移动且该文件是独立顶层文件（非目录子项），校验通过后立刻删源端文件，避免重传
			if taskCopy.Type == TypeMove && isTopLevelPath(it.srcPath, taskCopy.Paths) {
				if err := srcWriter.Delete(ctx, it.srcPath); err != nil && !errors.Is(err, driver.ErrNotFound) {
					return fmt.Errorf("移动完成但删除源文件 %s 失败: %w", it.srcPath, err)
				}
			}
		}

		m.mu.Lock()
		if cur, exists := m.tasks[id]; exists {
			cur.CompletedItems[it.srcPath] = true
			cur.DoneItems++
			_ = m.saveLocked()
		}
		m.mu.Unlock()
		m.broadcast()
	}

	// 跨源移动：所有子文件全部写入并校验完毕后，删除源端顶层目录
	if taskCopy.Type == TypeMove {
		for _, rootPath := range taskCopy.Paths {
			if err := srcWriter.Delete(ctx, rootPath); err != nil && !errors.Is(err, driver.ErrNotFound) {
				return fmt.Errorf("移动完成但清理源路径 %s 失败: %w", rootPath, err)
			}
		}
	}
	return nil
}

func isTopLevelPath(p string, roots []string) bool {
	for _, r := range roots {
		if p == r {
			return true
		}
	}
	return false
}

func (m *Manager) execSameSourceMove(ctx context.Context, id string, drv driver.FSDriver, w driver.Writer, tc *Task) error {
	m.mu.Lock()
	if cur, exists := m.tasks[id]; exists {
		cur.TotalItems = len(tc.Paths)
	}
	m.mu.Unlock()

	for _, sp := range tc.Paths {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if tc.CompletedItems[sp] {
			continue
		}
		st, err := drv.Stat(ctx, sp)
		if err != nil {
			return err
		}
		dstPath := joinRel(tc.DstDir, path.Base(sp))
		m.mu.Lock()
		if cur, exists := m.tasks[id]; exists {
			cur.CurrentItem = sp
			if !st.IsDir && st.Size > 0 {
				cur.TotalBytes += st.Size
			}
		}
		m.mu.Unlock()
		m.broadcast()

		if err := w.Rename(ctx, sp, dstPath); err != nil {
			return err
		}
		m.mu.Lock()
		if cur, exists := m.tasks[id]; exists {
			cur.CompletedItems[sp] = true
			cur.DoneItems++
			if !st.IsDir && st.Size > 0 {
				cur.TransferredBytes += st.Size
			}
			_ = m.saveLocked()
		}
		m.mu.Unlock()
		m.broadcast()
	}
	return nil
}

func (m *Manager) execSameSourceCopy(ctx context.Context, id string, drv driver.FSDriver, copier driver.Copier, tc *Task) error {
	m.mu.Lock()
	if cur, exists := m.tasks[id]; exists {
		cur.TotalItems = len(tc.Paths)
	}
	m.mu.Unlock()

	for _, sp := range tc.Paths {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if tc.CompletedItems[sp] {
			continue
		}
		st, err := drv.Stat(ctx, sp)
		if err != nil {
			return err
		}
		dstPath := joinRel(tc.DstDir, path.Base(sp))
		m.mu.Lock()
		if cur, exists := m.tasks[id]; exists {
			cur.CurrentItem = sp
			if !st.IsDir && st.Size > 0 {
				cur.TotalBytes += st.Size
			}
		}
		m.mu.Unlock()
		m.broadcast()

		if err := copier.Copy(ctx, sp, dstPath); err != nil {
			return err
		}
		m.mu.Lock()
		if cur, exists := m.tasks[id]; exists {
			cur.CompletedItems[sp] = true
			cur.DoneItems++
			if !st.IsDir && st.Size > 0 {
				cur.TransferredBytes += st.Size
			}
			_ = m.saveLocked()
		}
		m.mu.Unlock()
		m.broadcast()
	}
	return nil
}

// collectItems 递归展开待传输路径列表，计算总文件大小与顺序清单。
func (m *Manager) collectItems(ctx context.Context, srcDrv driver.FSDriver, rootPaths []string, dstDir string) ([]transferItem, int64, error) {
	var items []transferItem
	var totalBytes int64
	const maxItems = 20000

	for _, rp := range rootPaths {
		if ctx.Err() != nil {
			return nil, 0, context.Cause(ctx)
		}
		st, err := srcDrv.Stat(ctx, rp)
		if err != nil {
			return nil, 0, fmt.Errorf("读取源路径 %s 元数据失败: %w", rp, err)
		}
		targetRoot := joinRel(dstDir, path.Base(rp))
		if !st.IsDir {
			items = append(items, transferItem{
				srcPath: rp,
				dstPath: targetRoot,
				isDir:   false,
				size:    st.Size,
			})
			if st.Size > 0 {
				totalBytes += st.Size
			}
			continue
		}

		// 目录：先建顶层目录，再广度优先遍历子项
		items = append(items, transferItem{
			srcPath: rp,
			dstPath: targetRoot,
			isDir:   true,
		})
		type dirPair struct {
			src string
			dst string
		}
		queue := []dirPair{{src: rp, dst: targetRoot}}
		for len(queue) > 0 {
			if ctx.Err() != nil {
				return nil, 0, context.Cause(ctx)
			}
			if len(items) > maxItems {
				return nil, 0, fmt.Errorf("单次传输条目数超过上限 (%d)", maxItems)
			}
			cur := queue[0]
			queue = queue[1:]

			var cursor string
			for {
				res, err := srcDrv.List(ctx, cur.src, driver.ListOptions{Cursor: cursor, Limit: 1000})
				if err != nil {
					return nil, 0, fmt.Errorf("列取源目录 %s 失败: %w", cur.src, err)
				}
				for _, e := range res.Entries {
					childDst := joinRel(cur.dst, e.Name)
					if e.IsDir {
						items = append(items, transferItem{
							srcPath: e.Path,
							dstPath: childDst,
							isDir:   true,
						})
						queue = append(queue, dirPair{src: e.Path, dst: childDst})
					} else {
						items = append(items, transferItem{
							srcPath: e.Path,
							dstPath: childDst,
							isDir:   false,
							size:    e.Size,
						})
						if e.Size > 0 {
							totalBytes += e.Size
						}
					}
				}
				if res.NextCursor == "" {
					break
				}
				cursor = res.NextCursor
			}
		}
	}
	return items, totalBytes, nil
}

// streamSingleFile 使用 io.Pipe 零落盘流式传输单个文件，并执行完整性校验与失败回滚。
func (m *Manager) streamSingleFile(ctx context.Context, taskID string, srcDrv, dstDrv driver.FSDriver, dstWriter driver.Writer, it transferItem) error {
	rc, err := srcDrv.Open(ctx, it.srcPath, driver.OpenOptions{})
	if err != nil {
		return fmt.Errorf("打开源文件 %s 失败: %w", it.srcPath, err)
	}

	// 记录本文件开始前的已传字节基准，若该文件失败重试则回滚到基准值
	m.mu.RLock()
	var baseTransferred int64
	if cur, ok := m.tasks[taskID]; ok {
		baseTransferred = cur.TransferredBytes
	}
	m.mu.RUnlock()

	pr, pw := io.Pipe()
	progReader := &progressReader{
		ctx:           ctx,
		r:             rc,
		mgr:           m,
		taskID:        taskID,
		windowTime:    time.Now(),
		lastSyncBytes: time.Now(),
		lastPush:      time.Now(),
	}

	go func() {
		defer rc.Close()
		_, copyErr := io.Copy(pw, progReader)
		_ = pw.CloseWithError(copyErr)
	}()

	uploadErr := dstWriter.Upload(ctx, it.dstPath, pr, it.size)
	_ = pr.Close()

	if uploadErr != nil {
		// 失败回滚：清理目标端可能残留的半成品文件，并重置当前文件的已传字节计数
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = dstWriter.Delete(bgCtx, it.dstPath)
		cancel()

		m.mu.Lock()
		if cur, ok := m.tasks[taskID]; ok {
			cur.TransferredBytes = baseTransferred
			cur.SpeedBps = 0
		}
		m.mu.Unlock()

		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return fmt.Errorf("写入目标文件 %s 失败: %w", it.dstPath, uploadErr)
	}

	// 完整性校验：校验目标端文件存在且大小与源端一致
	st, statErr := dstDrv.Stat(ctx, it.dstPath)
	if statErr != nil || st.IsDir || (it.size >= 0 && st.Size != it.size) {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = dstWriter.Delete(bgCtx, it.dstPath)
		cancel()

		m.mu.Lock()
		if cur, ok := m.tasks[taskID]; ok {
			cur.TransferredBytes = baseTransferred
			cur.SpeedBps = 0
		}
		m.mu.Unlock()

		if statErr != nil {
			return fmt.Errorf("目标文件 %s 校验失败: %w", it.dstPath, statErr)
		}
		return fmt.Errorf("目标文件 %s 大小校验不一致 (期望 %d, 实际 %d)", it.dstPath, it.size, st.Size)
	}

	m.mu.Lock()
	if cur, ok := m.tasks[taskID]; ok && it.size >= 0 {
		cur.TransferredBytes = baseTransferred + it.size
	}
	m.mu.Unlock()
	return nil
}

// progressReader 包装 io.Reader，负责上下文中断检查、全局带宽限速与实时速率计算。
type progressReader struct {
	ctx           context.Context
	r             io.Reader
	mgr           *Manager
	taskID        string
	pendingBytes  int64
	windowBytes   int64
	windowTime    time.Time
	lastSyncBytes time.Time
	lastPush      time.Time
}

func (pr *progressReader) Read(p []byte) (int, error) {
	if err := pr.ctx.Err(); err != nil {
		pr.flushProgress(true)
		return 0, context.Cause(pr.ctx)
	}
	n, err := pr.r.Read(p)
	if n > 0 {
		if wErr := pr.mgr.waitBandwidth(pr.ctx, n); wErr != nil {
			pr.flushProgress(true)
			return n, wErr
		}
		now := time.Now()
		pr.pendingBytes += int64(n)
		pr.windowBytes += int64(n)

		// 降低锁争用：每 100ms 或累计满 256KB 同步一次 TransferredBytes，避免每个 32KB buffer 都竞争全局锁
		if pr.pendingBytes >= 256<<10 || now.Sub(pr.lastSyncBytes) >= 100*time.Millisecond {
			pr.flushProgress(false)
		}
	}
	if err != nil {
		pr.flushProgress(true)
	}
	return n, err
}

func (pr *progressReader) flushProgress(forcePush bool) {
	if pr.pendingBytes <= 0 && !forcePush {
		return
	}
	now := time.Now()
	elapsed := now.Sub(pr.windowTime)

	pr.mgr.mu.Lock()
	if t, ok := pr.mgr.tasks[pr.taskID]; ok {
		t.TransferredBytes += pr.pendingBytes
		if elapsed >= 200*time.Millisecond {
			t.SpeedBps = int64(float64(pr.windowBytes) / elapsed.Seconds())
			pr.windowBytes = 0
			pr.windowTime = now
		}
		t.UpdatedAt = now.Unix()
	}
	pr.mgr.mu.Unlock()

	pr.pendingBytes = 0
	pr.lastSyncBytes = now

	if forcePush || now.Sub(pr.lastPush) >= 250*time.Millisecond {
		pr.lastPush = now
		pr.mgr.broadcast()
	}
}

func (m *Manager) waitBandwidth(ctx context.Context, n int) error {
	if m.cfg.MaxBandwidthBps <= 0 {
		return nil
	}
	m.rateMu.Lock()
	now := time.Now()
	elapsed := now.Sub(m.rateLast).Seconds()
	m.rateLast = now
	limit := float64(m.cfg.MaxBandwidthBps)
	m.rateTokens += elapsed * limit
	if m.rateTokens > limit {
		m.rateTokens = limit
	}
	m.rateTokens -= float64(n)
	var wait time.Duration
	if m.rateTokens < 0 {
		wait = time.Duration((-m.rateTokens / limit) * float64(time.Second))
	}
	m.rateMu.Unlock()

	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	return nil
}

// —— SSE 实时订阅与广播 ——

// Subscribe 注册一个任务状态流订阅通道。返回只读 channel 与取消订阅函数。
func (m *Manager) Subscribe(username string, isAdmin bool) (<-chan []*Task, func()) {
	ch := make(chan []*Task, 8)
	filter := username
	if isAdmin {
		filter = ""
	}
	m.subMu.Lock()
	m.subs[ch] = filter
	m.subMu.Unlock()

	// 立即推送一次初始快照
	ch <- m.List(filter, isAdmin)

	unsubscribe := func() {
		m.subMu.Lock()
		if _, ok := m.subs[ch]; ok {
			delete(m.subs, ch)
			close(ch)
		}
		m.subMu.Unlock()
	}
	return ch, unsubscribe
}

func (m *Manager) broadcast() {
	m.subMu.Lock()
	if len(m.subs) == 0 {
		m.subMu.Unlock()
		return
	}
	targets := make(map[chan []*Task]string, len(m.subs))
	for ch, filter := range m.subs {
		targets[ch] = filter
	}
	m.subMu.Unlock()

	for ch, filter := range targets {
		snap := m.List(filter, filter == "")
		select {
		case ch <- snap:
		default:
			// 若消费端阻塞，丢弃旧帧再推新帧
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snap:
			default:
			}
		}
	}
}

// —— 持久化（data/tasks.json）——

func (m *Manager) load() error {
	raw, err := os.ReadFile(m.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("task: 读取 %s 失败: %w", m.file, err)
	}
	var list []*Task
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("task: 解析 %s 失败: %w", m.file, err)
	}
	for _, t := range list {
		if t == nil || t.ID == "" {
			continue
		}
		// 异常停机遗留的 running / retrying 任务自动转为 paused，供用户一键继续
		if t.Status == StatusRunning || t.Status == StatusRetrying {
			t.Status = StatusPaused
			t.SpeedBps = 0
		}
		if t.CompletedItems == nil {
			t.CompletedItems = map[string]bool{}
		}
		m.tasks[t.ID] = t
		m.order = append(m.order, t.ID)
	}
	return nil
}

func (m *Manager) saveLocked() error {
	list := make([]*Task, 0, len(m.order))
	for _, id := range m.order {
		if t := m.tasks[id]; t != nil {
			list = append(list, t)
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

func newTaskID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "task_" + hex.EncodeToString(b[:])
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
