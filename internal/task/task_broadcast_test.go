package task

import (
	"sync"
	"testing"
	"time"
)

// 回归测试：broadcast 与 unsubscribe 的竞态。
// 旧实现先在锁外拷贝订阅快照再发送，退订方 delete+close 后广播方仍持有过期快照，
// 会向已关闭 channel 发送导致 panic。现要求广播全程持有 subMu 与退订串行。
func TestBroadcastUnsubscribeRace(t *testing.T) {
	m, err := NewManager(t.TempDir(), Config{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 模拟任务进度高频广播
		for {
			select {
			case <-stop:
				return
			default:
				m.broadcast()
			}
		}
	}()

	// 模拟 SSE 客户端反复连接/断开
	const rounds = 300
	for i := 0; i < rounds; i++ {
		ch, unsub := m.Subscribe("u", false)
		select {
		case <-ch: // 初始快照
		case <-time.After(5 * time.Second):
			close(stop)
			wg.Wait()
			t.Fatalf("round %d: 未收到初始快照", i)
		}
		unsub()
	}

	close(stop)
	wg.Wait()
}
