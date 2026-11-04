// Package pathid 为“以 file_id 寻址”的云盘驱动（如 Google Drive、阿里云盘）
// 提供统一的 POSIX 相对路径 → 节点 ID 解析与带 TTL 的并发安全缓存。
package pathid

import (
	"context"
	"path"
	"strings"
	"sync"
	"time"

	"ufile/internal/driver"
)

// DefaultTTL 默认缓存有效期。
const DefaultTTL = 5 * time.Minute

// Node 表示云盘中的一个文件或目录节点元数据。
type Node struct {
	ID      string
	Name    string
	IsDir   bool
	Size    int64
	ModTime time.Time
	ETag    string
}

// ToEntry 转换为 driver.Entry（relPath 为相对源根的规范化路径）。
func (n Node) ToEntry(relPath string) driver.Entry {
	return driver.Entry{
		Path:    relPath,
		Name:    n.Name,
		IsDir:   n.IsDir,
		Size:    n.Size,
		ModTime: n.ModTime,
		ETag:    n.ETag,
	}
}

// LookupFunc 由具体云盘驱动实现：在 parentID 目录下按名称查找单个直接子节点。
// 若不存在必须返回 driver.ErrNotFound。
type LookupFunc func(ctx context.Context, parentID, name string) (Node, error)

type cachedEntry struct {
	node     Node
	expireAt time.Time
}

// Resolver 维护 cleanPath -> Node 的并发安全缓存，支持按层级解析与子树失效。
type Resolver struct {
	mu     sync.RWMutex
	rootID string
	ttl    time.Duration
	lookup LookupFunc
	items  map[string]cachedEntry
}

// NewResolver 创建路径解析器。rootID 为源根目录对应的云盘节点 ID（如 "root"）。
func NewResolver(rootID string, ttl time.Duration, lookup LookupFunc) *Resolver {
	if rootID == "" {
		rootID = "root"
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Resolver{
		rootID: rootID,
		ttl:    ttl,
		lookup: lookup,
		items:  make(map[string]cachedEntry),
	}
}

// RootID 返回根文件夹 ID。
func (r *Resolver) RootID() string { return r.rootID }

// Get 尝试从缓存读取未过期的节点。
func (r *Resolver) Get(cleanPath string) (Node, bool) {
	if cleanPath == "" {
		return Node{ID: r.rootID, Name: "", IsDir: true}, true
	}
	now := time.Now()
	r.mu.RLock()
	ce, ok := r.items[cleanPath]
	r.mu.RUnlock()
	if !ok || now.After(ce.expireAt) {
		return Node{}, false
	}
	return ce.node, true
}

// Put 将节点写入缓存。
func (r *Resolver) Put(cleanPath string, n Node) {
	if cleanPath == "" {
		return
	}
	r.mu.Lock()
	r.items[cleanPath] = cachedEntry{
		node:     n,
		expireAt: time.Now().Add(r.ttl),
	}
	r.mu.Unlock()
}

// PutChildren 批量写入 dirPath 目录下的直接子节点（供 List 顺带预热缓存）。
func (r *Resolver) PutChildren(dirCleanPath string, children []Node) {
	now := time.Now().Add(r.ttl)
	r.mu.Lock()
	for _, ch := range children {
		if ch.Name == "" {
			continue
		}
		p := ch.Name
		if dirCleanPath != "" {
			p = dirCleanPath + "/" + ch.Name
		}
		r.items[p] = cachedEntry{node: ch, expireAt: now}
	}
	r.mu.Unlock()
}

// Invalidate 失效指定路径及其所有子孙路径的缓存（供 Delete/Rename/Upload/Mkdir 调用）。
func (r *Resolver) Invalidate(cleanPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cleanPath == "" {
		clear(r.items)
		return
	}
	delete(r.items, cleanPath)
	prefix := cleanPath + "/"
	for k := range r.items {
		if strings.HasPrefix(k, prefix) {
			delete(r.items, k)
		}
	}
}

// Resolve 将任意相对路径规范化并解析为云盘 Node。
func (r *Resolver) Resolve(ctx context.Context, rawPath string) (string, Node, error) {
	clean, err := driver.CleanPath(rawPath)
	if err != nil {
		return "", Node{}, err
	}
	if clean == "" {
		return "", Node{ID: r.rootID, Name: "", IsDir: true}, nil
	}
	if n, ok := r.Get(clean); ok {
		return clean, n, nil
	}

	segs := strings.Split(clean, "/")
	curID := r.rootID
	curPath := ""

	for i, seg := range segs {
		if i > 0 {
			curPath += "/" + seg
		} else {
			curPath = seg
		}
		if n, ok := r.Get(curPath); ok {
			if i < len(segs)-1 && !n.IsDir {
				return "", Node{}, driver.ErrNotFound
			}
			curID = n.ID
			if i == len(segs)-1 {
				return clean, n, nil
			}
			continue
		}
		n, err := r.lookup(ctx, curID, seg)
		if err != nil {
			return "", Node{}, err
		}
		r.Put(curPath, n)
		if i < len(segs)-1 && !n.IsDir {
			return "", Node{}, driver.ErrNotFound
		}
		curID = n.ID
		if i == len(segs)-1 {
			return clean, n, nil
		}
	}
	return "", Node{}, driver.ErrNotFound
}

// SplitParent 拆分规范化路径为 (parentCleanPath, baseName, error)。
func SplitParent(rawPath string) (string, string, error) {
	clean, err := driver.CleanPath(rawPath)
	if err != nil {
		return "", "", err
	}
	if clean == "" {
		return "", "", driver.ErrInvalidPath
	}
	dir := path.Dir(clean)
	if dir == "." {
		dir = ""
	}
	return dir, path.Base(clean), nil
}
