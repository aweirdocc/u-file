// Package driver 定义所有文件源的统一抽象（FSDriver）与驱动注册表。
//
// M0（PoC）仅保留读路径：List / Stat / Open；写能力（上传/删除/重命名等）
// 在 M1 以可选能力接口的形式加入，避免早期接口反复变动。
package driver

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotSupported 表示当前驱动不支持该操作（未实现对应可选能力接口）。
	ErrNotSupported = errors.New("driver: operation not supported")
	// ErrNotFound 统一的“对象不存在”，便于上层映射 404。
	ErrNotFound = errors.New("driver: object not found")
	// ErrInvalidPath 路径非法（包含 ..、反斜杠、空字节等）。
	ErrInvalidPath = errors.New("driver: invalid path")
	// ErrUnknownType 未注册的源类型。
	ErrUnknownType = errors.New("driver: unknown source type")
)

// Capabilities 声明驱动支持的操作集合，上层据此决定 API 行为与前端 UI 显隐。
type Capabilities struct {
	List       bool `json:"list"`
	Stat       bool `json:"stat"`
	Download   bool `json:"download"`
	PresignURL bool `json:"presign_url"` // 可生成带签名直链（S3 协议族）
	ListCursor bool `json:"list_cursor"` // List 支持游标分页
	Write      bool `json:"write"`       // 实现了 Writer（上传/删除/建目录/重命名）
}

// Entry 文件或目录的元数据。Path 统一以 / 分隔、相对源根且不以 / 开头；根目录为 ""。
type Entry struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	ETag    string    `json:"etag,omitempty"`
}

// ListOptions 列目录选项。
type ListOptions struct {
	Cursor string // 上一页返回的 NextCursor；ListCursor=false 时忽略
	Limit  int    // 单页条数建议值；<=0 用驱动默认
}

// ListResult 目录列表结果。
type ListResult struct {
	Entries    []Entry `json:"entries"`
	NextCursor string  `json:"next_cursor,omitempty"` // 空 = 已到末尾
}

// OpenOptions 打开文件流选项。
type OpenOptions struct {
	Offset int64 // 起始字节，0 = 从头
	Length int64 // 读取长度；<=0 = 读到结尾
}

// FSDriver 统一文件系统抽象。实现约定：
//  1. Open 必须返回流，不得把整个文件读进内存；
//  2. 路径为相对源根的 / 分隔路径（"" 为根），驱动内部通过 CleanPath 拒绝非法路径；
//  3. 驱动无状态，连接池/会话由实现内部管理。
type FSDriver interface {
	Scheme() string
	Capabilities() Capabilities
	List(ctx context.Context, dir string, opt ListOptions) (*ListResult, error)
	Stat(ctx context.Context, path string) (*Entry, error)
	Open(ctx context.Context, path string, opt OpenOptions) (io.ReadCloser, error)
}

// Presigner 是可选能力：生成带签名的下载直链（S3 协议族驱动实现）。
// 上层通过类型断言探测：if p, ok := drv.(driver.Presigner); ok { ... }
type Presigner interface {
	PresignGet(ctx context.Context, path string, ttl time.Duration) (string, error)
}

// Writer 是可选写能力：上传、删除、新建目录、重命名。
// 约定：
//   - Upload 由调用方负责关闭 r；size 为已知大小（<=0 表示未知，驱动自行处理）；
//   - Delete 对文件与目录统一处理：目录语义为“删除该前缀下全部内容”（S3）/
//     “递归删除”（local/ftp/sftp）；不存在返回 ErrNotFound；
//   - Rename 同一源内移动/重命名；目标已存在时行为由驱动决定（覆盖），由 API 层先做存在性提示；
//   - Mkdir 创建目录（父目录不存在时一并创建，与主流对象存储前缀语义一致）。
type Writer interface {
	Upload(ctx context.Context, path string, r io.Reader, size int64) error
	Delete(ctx context.Context, path string) error
	Mkdir(ctx context.Context, path string) error
	Rename(ctx context.Context, from, to string) error
}
