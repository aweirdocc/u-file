package driver

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Factory 由源配置（config 段，map 形式）实例化驱动。
type Factory func(cfg map[string]any) (FSDriver, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册驱动工厂（驱动包 init 时调用；重复注册视为编程错误）。
func Register(scheme string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[scheme]; dup {
		panic("driver: duplicate registration for scheme " + scheme)
	}
	factories[scheme] = f
}

// Schemes 返回已注册的源类型（按字母序）。
func Schemes() []string {
	mu.RLock()
	defer mu.RUnlock()
	list := make([]string, 0, len(factories))
	for s := range factories {
		list = append(list, s)
	}
	sort.Strings(list)
	return list
}

// New 按源类型实例化驱动。
func New(scheme string, cfg map[string]any) (FSDriver, error) {
	mu.RLock()
	f, ok := factories[scheme]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q (registered: %v)", ErrUnknownType, scheme, Schemes())
	}
	return f(cfg)
}

// CleanPath 规范化相对路径：拒绝反斜杠、空字节与 .. 段；
// 折叠空段与 "."，返回以 / 分隔的干净相对路径（根目录为 ""）。
func CleanPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.ContainsRune(p, 0) || strings.ContainsRune(p, '\\') {
		return "", ErrInvalidPath
	}
	var b strings.Builder
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return "", ErrInvalidPath
		default:
			b.WriteByte('/')
			b.WriteString(seg)
		}
	}
	return strings.TrimPrefix(b.String(), "/"), nil
}

// —— 源配置取值辅助（yaml.v3 解出的数值为 int，JSON 为 float64，需统一兼容）——

func Str(cfg map[string]any, key, def string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return def
}

func RequiredStr(cfg map[string]any, key string) (string, error) {
	v, ok := cfg[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("config: missing required field %q", key)
	}
	return v, nil
}

func Bool(cfg map[string]any, key string, def bool) bool {
	if v, ok := cfg[key].(bool); ok {
		return v
	}
	return def
}

func Int(cfg map[string]any, key string, def int64) int64 {
	switch v := cfg[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
