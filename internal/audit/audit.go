// Package audit 操作审计日志：追加写 JSONL（data/audit.log），
// 记录登录、源增删改、导入导出等敏感操作。文件人可读，也便于接采集器。
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry 一条审计记录。
type Entry struct {
	Time   time.Time `json:"time"`
	User   string    `json:"user"`
	IP     string    `json:"ip"`
	Action string    `json:"action"` // auth.login / source.add / source.delete / ...
	Detail any       `json:"detail,omitempty"`
}

// Log 追加写审计日志。写失败不阻断业务（只丢审计不丢功能），
// 但错误会回调给 logger 以便暴露问题。
type Log struct {
	mu     sync.Mutex
	file   string
	onErr  func(error)
}

// Open 打开（或创建）审计日志。dataDir 为数据目录。
func Open(dataDir string, onErr func(error)) (*Log, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	return &Log{file: filepath.Join(dataDir, "audit.log"), onErr: onErr}, nil
}

// Record 追加一条记录（自动带时间戳兜底）。
func (l *Log) Record(e Entry) {
	if l == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		if l.onErr != nil {
			l.onErr(err)
		}
		return
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil && l.onErr != nil {
		l.onErr(err)
	}
}

// Read 尾读最近 n 条（倒序返回，供管理界面查看）。
func (l *Log) Read(n int) ([]Entry, error) {
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	raw, err := os.ReadFile(l.file)
	if os.IsNotExist(err) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, line := range splitLines(raw) {
		var e Entry
		if json.Unmarshal(line, &e) == nil {
			entries = append(entries, e)
		}
	}
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	// 倒序（最新在前）
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
