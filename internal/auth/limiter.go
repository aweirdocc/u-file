package auth

import (
	"sync"
	"time"
)

// limiter 登录失败限速：按 key（用户名+IP）记录连续失败次数，
// 连续失败达到阈值后进入惩罚窗口，窗口随失败次数指数拉长。
// 内存态即可（单实例部署），重启即清零是可接受的。
type limiter struct {
	mu      sync.Mutex
	fails   map[string]*failRecord
	base    time.Duration // 首次惩罚窗口
	max     time.Duration // 惩罚窗口上限
	allowed int           // 允许的连续失败次数
}

type failRecord struct {
	count    int
	lastFail time.Time
	until    time.Time // 惩罚截止；零值=未惩罚
}

func newLimiter(allowed int) *limiter {
	return &limiter{
		fails:   map[string]*failRecord{},
		base:    5 * time.Second,
		max:     15 * time.Minute,
		allowed: allowed,
	}
}

// Allow 判断该 key 当前是否允许尝试登录。
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
		// 指数退避：base * 2^(count-allowed)，封顶 max
		backoff := l.base << uint(min(r.count-l.allowed, 20))
		if backoff > l.max {
			backoff = l.max
		}
		r.until = time.Now().Add(backoff)
		return true
	}
	return false
}

// Reset 登录成功后清零该 key 的记录。
func (l *limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// RetryAfter 返回被限速的 key 还需等待的时长（未限速返回 0）。
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
