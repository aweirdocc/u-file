package share

import (
	"sync"
	"time"
)

// limiter 提取码失败限速：按 key（token+IP）记录连续失败次数，
// 达到阈值后指数退避。内存态即可（单实例部署，重启清零可接受）。
// 与 auth/limiter 同模式，独立副本避免包间耦合。
type limiter struct {
	mu      sync.Mutex
	fails   map[string]*failRecord
	base    time.Duration
	max     time.Duration
	allowed int
}

type failRecord struct {
	count    int
	lastFail time.Time
	until    time.Time
}

func newLimiter() *limiter {
	return &limiter{
		fails:   map[string]*failRecord{},
		base:    5 * time.Second,
		max:     15 * time.Minute,
		allowed: 5,
	}
}

func (l *limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.fails[key]
	if !ok {
		return true
	}
	if !r.until.IsZero() && time.Now().Before(r.until) {
		return false
	}
	return true
}

// Fail 记录一次失败，必要时进入/延长惩罚窗口。返回当前是否已被限速。
func (l *limiter) Fail(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.fails[key]
	if !ok {
		r = &failRecord{}
		l.fails[key] = r
	}
	r.count++
	r.lastFail = time.Now()
	if r.count >= l.allowed {
		backoff := l.base << uint(minInt(r.count-l.allowed, 20))
		if backoff > l.max {
			backoff = l.max
		}
		r.until = time.Now().Add(backoff)
		return true
	}
	return false
}

func (l *limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

func (l *limiter) RetryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.fails[key]
	if !ok || r.until.IsZero() {
		return 0
	}
	d := time.Until(r.until)
	if d < 0 {
		return 0
	}
	return d
}

// 定期清理 1 小时无活动的记录，防止 map 无限增长。
func (l *limiter) gc() {
	t := time.NewTicker(time.Hour)
	go func() {
		for range t.C {
			l.mu.Lock()
			for k, r := range l.fails {
				if time.Since(r.lastFail) > time.Hour {
					delete(l.fails, k)
				}
			}
			l.mu.Unlock()
		}
	}()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
