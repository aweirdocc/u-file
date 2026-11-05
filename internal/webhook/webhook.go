// Package webhook 实现 M4.2 事件驱动与 Webhook 异步投递引擎。
//
// 核心特性（对齐 docs/06 4.2 节）：
//   - 标准 JSON 事件载荷：event_id / event_type / timestamp / source_id / data；
//   - HMAC-SHA256 防篡改签名：X-UFile-Signature: sha256=<hex>；
//   - 异步重试队列：投递失败按指数退避重试（最多 5 次），记录最近状态码、耗时与错误；
//   - 支持一键发送 webhook.ping 测试事件。
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	EventFileUploaded    = "file.uploaded"
	EventFileDeleted     = "file.deleted"
	EventFileRenamed     = "file.renamed"
	EventFileTransferred = "file.transferred"
	EventShareCreated    = "share.created"
	EventShareDeleted    = "share.deleted"
	EventSyncCompleted   = "sync.completed"
	EventSyncFailed      = "sync.failed"
	EventPing            = "webhook.ping"
)

var AllEvents = []string{
	EventFileUploaded,
	EventFileDeleted,
	EventFileRenamed,
	EventFileTransferred,
	EventShareCreated,
	EventShareDeleted,
	EventSyncCompleted,
	EventSyncFailed,
}

var ErrNotFound = errors.New("webhook: 订阅端点不存在")

// Endpoint Webhook 订阅端点配置与状态。
type Endpoint struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	URL             string     `json:"url"`
	Secret          string     `json:"secret,omitempty"` // 列表脱敏时返回 "******" 或空
	HasSecret       bool       `json:"has_secret"`
	Events          []string   `json:"events"` // 支持 "*" 或具体事件名
	Enabled         bool       `json:"enabled"`
	FailCount       int        `json:"fail_count"`
	LastStatus      int        `json:"last_status,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// Sanitized 隐去明文 Secret，供前端展示。
func (e *Endpoint) Sanitized() *Endpoint {
	cp := *e
	cp.HasSecret = e.Secret != ""
	if e.Secret != "" {
		cp.Secret = "******"
	}
	if e.Events != nil {
		cp.Events = append([]string(nil), e.Events...)
	}
	return &cp
}

func (e *Endpoint) MatchesEvent(evType string) bool {
	if !e.Enabled {
		return false
	}
	for _, ev := range e.Events {
		if ev == "*" || ev == evType {
			return true
		}
	}
	return false
}

// Event 标准事件报文。
type Event struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	Timestamp int64  `json:"timestamp"`
	SourceID  string `json:"source_id,omitempty"`
	Data      any    `json:"data"`
}

// TestResult 测试投递结果。
type TestResult struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	LatencyMs  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// Config Webhook 管理器配置。
type Config struct {
	MaxRetries     int           // 最大重试次数（默认 5）
	RetryBaseDelay time.Duration // 初始退避延迟（默认 2s，测试可设为 10ms）
	HTTPTimeout    time.Duration // 单次 HTTP 投递超时（默认 10s）
}

type deliveryJob struct {
	endpointID string
	event      Event
}

// Manager 管理 Webhook 端点与异步投递。
type Manager struct {
	mu        sync.RWMutex
	file      string
	cfg       Config
	client    *http.Client
	endpoints map[string]*Endpoint
	order     []string

	queue  chan deliveryJob
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewManager(dataDir string, cfg Config) (*Manager, error) {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 5
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = 2 * time.Second
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		file:      filepath.Join(dataDir, "webhooks.json"),
		cfg:       cfg,
		client:    &http.Client{Timeout: cfg.HTTPTimeout},
		endpoints: map[string]*Endpoint{},
		queue:     make(chan deliveryJob, 512),
		stopCh:    make(chan struct{}),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	for range 2 {
		m.wg.Add(1)
		go m.worker()
	}
	return m, nil
}

func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
		return
	default:
		close(m.stopCh)
	}
	m.wg.Wait()
}

type UpsertOptions struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Secret  string   `json:"secret"`
	Events  []string `json:"events"`
	Enabled bool     `json:"enabled"`
}

func validateOptions(opt UpsertOptions) (string, string, []string, error) {
	name := strings.TrimSpace(opt.Name)
	if name == "" {
		name = "默认 Webhook"
	}
	rawURL := strings.TrimSpace(opt.URL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", nil, errors.New("webhook: URL 必须为合法的 http:// 或 https:// 地址")
	}
	if len(opt.Events) == 0 {
		return "", "", nil, errors.New("webhook: 至少需要订阅一个事件类型")
	}
	var evs []string
	seen := map[string]bool{}
	for _, e := range opt.Events {
		e = strings.TrimSpace(e)
		if e != "" && !seen[e] {
			seen[e] = true
			evs = append(evs, e)
		}
	}
	return name, rawURL, evs, nil
}

// Add 创建新的 Webhook 订阅端点。
func (m *Manager) Add(opt UpsertOptions) (*Endpoint, error) {
	name, u, evs, err := validateOptions(opt)
	if err != nil {
		return nil, err
	}
	var idBytes [6]byte
	_, _ = rand.Read(idBytes[:])
	ep := &Endpoint{
		ID:        "wh_" + hex.EncodeToString(idBytes[:]),
		Name:      name,
		URL:       u,
		Secret:    strings.TrimSpace(opt.Secret),
		Events:    evs,
		Enabled:   opt.Enabled,
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.endpoints[ep.ID] = ep
	m.order = append([]string{ep.ID}, m.order...)
	if err := m.saveLocked(); err != nil {
		delete(m.endpoints, ep.ID)
		m.order = m.order[1:]
		m.mu.Unlock()
		return nil, err
	}
	out := ep.Sanitized()
	m.mu.Unlock()
	return out, nil
}

// Update 更新已有 Webhook 端点（Secret 传 "******" 时保留原 Secret）。
func (m *Manager) Update(id string, opt UpsertOptions) (*Endpoint, error) {
	name, u, evs, err := validateOptions(opt)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ep, ok := m.endpoints[id]
	if !ok {
		return nil, ErrNotFound
	}
	ep.Name = name
	ep.URL = u
	ep.Events = evs
	ep.Enabled = opt.Enabled
	if opt.Secret != "******" {
		ep.Secret = strings.TrimSpace(opt.Secret)
	}
	if err := m.saveLocked(); err != nil {
		return nil, err
	}
	return ep.Sanitized(), nil
}

// Delete 删除 Webhook 端点。
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.endpoints[id]; !ok {
		return ErrNotFound
	}
	delete(m.endpoints, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return m.saveLocked()
}

// List 返回全部端点（Secret 已打码）。
func (m *Manager) List() []*Endpoint {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Endpoint, 0, len(m.order))
	for _, id := range m.order {
		if ep := m.endpoints[id]; ep != nil {
			out = append(out, ep.Sanitized())
		}
	}
	return out
}

// Emit 触发事件并异步投递给所有匹配的订阅端点。
func (m *Manager) Emit(eventType, sourceID string, data any) {
	m.mu.RLock()
	var matchedIDs []string
	for _, id := range m.order {
		if ep := m.endpoints[id]; ep != nil && ep.MatchesEvent(eventType) {
			matchedIDs = append(matchedIDs, id)
		}
	}
	m.mu.RUnlock()
	if len(matchedIDs) == 0 {
		return
	}

	ev := Event{
		EventID:   newEventID(),
		EventType: eventType,
		Timestamp: time.Now().Unix(),
		SourceID:  sourceID,
		Data:      data,
	}
	for _, epID := range matchedIDs {
		job := deliveryJob{endpointID: epID, event: ev}
		select {
		case m.queue <- job:
		default:
		}
	}
}

// Test 同步向指定端点发送一次 webhook.ping 测试报文。
func (m *Manager) Test(ctx context.Context, id string) (*TestResult, error) {
	m.mu.RLock()
	ep, ok := m.endpoints[id]
	if !ok {
		m.mu.RUnlock()
		return nil, ErrNotFound
	}
	urlStr, secret := ep.URL, ep.Secret
	m.mu.RUnlock()

	ev := Event{
		EventID:   newEventID(),
		EventType: EventPing,
		Timestamp: time.Now().Unix(),
		Data:      map[string]any{"message": "u-file webhook connectivity test"},
	}
	start := time.Now()
	code, err := m.sendOnce(ctx, urlStr, secret, ev)
	res := &TestResult{
		OK:         err == nil && code >= 200 && code < 300,
		StatusCode: code,
		LatencyMs:  time.Since(start).Milliseconds(),
	}
	if err != nil {
		res.Error = err.Error()
	} else if !res.OK {
		res.Error = fmt.Sprintf("HTTP %d", code)
	}
	return res, nil
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case job := <-m.queue:
			m.deliverWithRetry(job)
		}
	}
}

func (m *Manager) deliverWithRetry(job deliveryJob) {
	var lastCode int
	var lastErr error

	for attempt := 0; attempt < m.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := m.cfg.RetryBaseDelay * time.Duration(1<<(attempt-1))
			if backoff > 2*time.Minute {
				backoff = 2 * time.Minute
			}
			select {
			case <-time.After(backoff):
			case <-m.stopCh:
				return
			}
		}

		m.mu.RLock()
		ep, ok := m.endpoints[job.endpointID]
		if !ok || !ep.Enabled {
			m.mu.RUnlock()
			return
		}
		targetURL, secret := ep.URL, ep.Secret
		m.mu.RUnlock()

		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.HTTPTimeout)
		code, err := m.sendOnce(ctx, targetURL, secret, job.event)
		cancel()

		lastCode = code
		lastErr = err
		if err == nil && code >= 200 && code < 300 {
			now := time.Now()
			m.mu.Lock()
			if cur, exists := m.endpoints[job.endpointID]; exists {
				cur.FailCount = 0
				cur.LastStatus = code
				cur.LastError = ""
				cur.LastTriggeredAt = &now
				_ = m.saveLocked()
			}
			m.mu.Unlock()
			return
		}
	}

	now := time.Now()
	m.mu.Lock()
	if cur, exists := m.endpoints[job.endpointID]; exists {
		cur.FailCount++
		cur.LastStatus = lastCode
		if lastErr != nil {
			cur.LastError = lastErr.Error()
		} else {
			cur.LastError = fmt.Sprintf("HTTP %d", lastCode)
		}
		cur.LastTriggeredAt = &now
		_ = m.saveLocked()
	}
	m.mu.Unlock()
}

func (m *Manager) sendOnce(ctx context.Context, targetURL, secret string, ev Event) (int, error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "u-file-webhook/1.0")
	req.Header.Set("X-UFile-Event", ev.EventType)
	req.Header.Set("X-UFile-Delivery", ev.EventID)
	if secret != "" {
		req.Header.Set("X-UFile-Signature", "sha256="+SignPayload(body, secret))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// SignPayload 计算请求体的 HMAC-SHA256 十六进制签名。
func SignPayload(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *Manager) load() error {
	raw, err := os.ReadFile(m.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []*Endpoint
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("webhook: 解析 %s 失败: %w", m.file, err)
	}
	for _, ep := range list {
		if ep != nil && ep.ID != "" {
			m.endpoints[ep.ID] = ep
			m.order = append(m.order, ep.ID)
		}
	}
	return nil
}

func (m *Manager) saveLocked() error {
	list := make([]*Endpoint, 0, len(m.order))
	for _, id := range m.order {
		if ep := m.endpoints[id]; ep != nil {
			list = append(list, ep)
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

func newEventID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "evt_" + hex.EncodeToString(b[:])
}
