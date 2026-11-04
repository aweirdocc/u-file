package pathid

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"ufile/internal/driver"
)

func TestResolverLookupAndCache(t *testing.T) {
	var calls atomic.Int32
	tree := map[string]map[string]Node{
		"root": {
			"docs": {ID: "id-docs", Name: "docs", IsDir: true},
		},
		"id-docs": {
			"sub":    {ID: "id-sub", Name: "sub", IsDir: true},
			"a.txt":  {ID: "id-a", Name: "a.txt", IsDir: false, Size: 12},
		},
		"id-sub": {
			"b.pdf": {ID: "id-b", Name: "b.pdf", IsDir: false, Size: 99},
		},
	}

	r := NewResolver("root", time.Minute, func(ctx context.Context, parentID, name string) (Node, error) {
		calls.Add(1)
		children, ok := tree[parentID]
		if !ok {
			return Node{}, driver.ErrNotFound
		}
		n, ok := children[name]
		if !ok {
			return Node{}, driver.ErrNotFound
		}
		return n, nil
	})

	// 1. 根目录不触发 lookup
	clean, rootNode, err := r.Resolve(context.Background(), "")
	if err != nil || clean != "" || rootNode.ID != "root" || !rootNode.IsDir {
		t.Fatalf("resolve root failed: %v, %+v", err, rootNode)
	}
	if calls.Load() != 0 {
		t.Fatalf("expected 0 calls for root, got %d", calls.Load())
	}

	// 2. 深层路径首次解析：逐级调用 3 次
	clean, n, err := r.Resolve(context.Background(), "docs/sub/b.pdf")
	if err != nil || clean != "docs/sub/b.pdf" || n.ID != "id-b" || n.Size != 99 {
		t.Fatalf("resolve deep path failed: %v, %+v", err, n)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 lookup calls, got %d", calls.Load())
	}

	// 3. 再次解析同一路径及祖先路径：完全命中缓存，0 次新增调用
	if _, n2, err := r.Resolve(context.Background(), "docs/sub"); err != nil || n2.ID != "id-sub" {
		t.Fatalf("resolve cached parent failed: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected cached hit without lookup, got %d", calls.Load())
	}

	// 4. 同级其它文件：复用已缓存的 docs，只需 1 次 lookup
	if _, na, err := r.Resolve(context.Background(), "docs/a.txt"); err != nil || na.ID != "id-a" {
		t.Fatalf("resolve sibling failed: %v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("expected 4 lookup calls, got %d", calls.Load())
	}

	// 5. 把普通文件当中间目录访问应返回 ErrNotFound
	if _, _, err := r.Resolve(context.Background(), "docs/a.txt/child"); err != driver.ErrNotFound {
		t.Fatalf("expected ErrNotFound when traversing file, got %v", err)
	}

	// 6. Invalidate 子树后重新触发解析
	r.Invalidate("docs/sub")
	if _, _, err := r.Resolve(context.Background(), "docs/sub/b.pdf"); err != nil {
		t.Fatalf("resolve after invalidate failed: %v", err)
	}
	if calls.Load() != 6 { // docs 仍在缓存，重新查 sub 和 b.pdf 共 2 次
		t.Fatalf("expected 6 lookup calls after invalidate, got %d", calls.Load())
	}
}
