package driver

import (
	"context"
	"fmt"
	"time"
)

// WatchdogGrace kill 触发后等待 fn goroutine 退出的宽限期。
// 极端场景（kill 自身被对端挂起）下超时放弃等待：泄漏一个阻塞的
// goroutine 而不是让调用方永久阻塞。
const WatchdogGrace = 5 * time.Second

// AbortError 看门狗中止：kill 已触发、底层连接已作废，调用方应重拨/重取连接。
type AbortError struct {
	Timeout time.Duration // >0 = 墙上时钟超时；0 = ctx 取消
	Cause   error         // ctx 取消时的原因
}

func (e *AbortError) Error() string {
	if e.Timeout > 0 {
		return fmt.Sprintf("driver: 操作超过 %s 无响应，已强制断开连接", e.Timeout)
	}
	return fmt.Sprintf("driver: 操作已取消（%v），已强制断开连接", e.Cause)
}

func (e *AbortError) Unwrap() error { return e.Cause }

// RunWatchdog 带看门狗地执行一次阻塞式协议操作（FTP/SFTP 等未提供
// 按操作超时的客户端库）。fn 在独立 goroutine 中运行；若 ctx 先取消或
// timeout 先到，则调用 kill 强制关闭底层连接以解除阻塞中的网络读写，
// 然后等待 fn goroutine 退出（带宽限期），保证不阻塞调用方、不悬挂。
//
//   - timeout <= 0：不设墙上时钟上限，仅感知 ctx 取消（适用于时长由
//     数据量决定的传输操作）；
//   - kill 必须能解除 fn 内部的阻塞（如 conn.Close / client.Close），
//     允许异步执行（如 FTP 的 Quit 需要先写命令）。
func RunWatchdog(ctx context.Context, timeout time.Duration, kill func(), fn func() error) error {
	errCh := make(chan error, 1)
	go func() { errCh <- fn() }()

	var timerC <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timerC = t.C
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		kill()
		waitGrace(errCh)
		return &AbortError{Cause: context.Cause(ctx)}
	case <-timerC:
		kill()
		waitGrace(errCh)
		return &AbortError{Timeout: timeout}
	}
}

func waitGrace(errCh <-chan error) {
	select {
	case <-errCh:
	case <-time.After(WatchdogGrace):
	}
}
