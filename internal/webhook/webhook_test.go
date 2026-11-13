package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookDeliverySignatureAndRetry(t *testing.T) {
	var attempts atomic.Int32
	var mu sync.Mutex // 保护以下捕获变量（handler goroutine 写、测试主体读）
	var gotSig, gotEvent string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			// 前两次模拟 502 失败，第三次返回 200 OK
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		mu.Lock()
		gotSig = r.Header.Get("X-UFile-Signature")
		gotEvent = r.Header.Get("X-UFile-Event")
		gotBody, _ = io.ReadAll(r.Body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr, err := NewManager(t.TempDir(), Config{
		MaxRetries:     4,
		RetryBaseDelay: 10 * time.Millisecond,
		HTTPTimeout:    2 * time.Second,
		AllowPrivateTargets: true, // httptest 监听 127.0.0.1
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()

	ep, err := mgr.Add(UpsertOptions{
		Name:    "Ops Hook",
		URL:     srv.URL,
		Secret:  "my-super-secret",
		Events:  []string{EventFileUploaded},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ep.Secret != "******" || !ep.HasSecret {
		t.Fatalf("expected redacted secret, got %+v", ep)
	}

	mgr.Emit(EventFileUploaded, "oss-prod", map[string]any{"path": "demo.txt", "size": 42})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if attempts.Load() >= 3 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)

	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts (2 retries), got %d", attempts.Load())
	}
	mu.Lock()
	sig, event, body := gotSig, gotEvent, gotBody
	mu.Unlock()
	if event != EventFileUploaded {
		t.Fatalf("expected event %s, got %s", EventFileUploaded, event)
	}
	wantSig := "sha256=" + SignPayload(body, "my-super-secret")
	if sig != wantSig {
		t.Fatalf("signature mismatch: got %s, want %s", sig, wantSig)
	}

	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.SourceID != "oss-prod" {
		t.Fatalf("unexpected payload: %v %+v", err, ev)
	}

	// 测试 Ping
	tr, err := mgr.Test(context.Background(), ep.ID)
	if err != nil || !tr.OK || tr.StatusCode != 200 {
		t.Fatalf("ping failed: %v %+v", err, tr)
	}
}
