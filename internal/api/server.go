package api

import (
	"context"
	"embed"
	"errors"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/driver"
	"ufile/internal/source"
)

//go:embed static/index.html
var staticFS embed.FS

// textLikeExtensions 视为文本预览的扩展名（前端按此判断，后端兜底嗅探）。
var textLikeExtensions = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".csv": true, ".log": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
	".xml": true, ".html": true, ".htm": true, ".css": true,
	".go": true, ".js": true, ".ts": true, ".py": true, ".java": true,
	".c": true, ".h": true, ".cpp": true, ".rs": true, ".sh": true,
	".sql": true, ".bat": true, ".ps1": true, ".diff": true, ".env": true,
}

// previewKind 返回文件可预览的类型；"" 表示不支持预览。
func previewKind(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".bmp", ".ico", ".avif":
		return "image"
	case ".mp4", ".webm", ".mov", ".mkv", ".m4v":
		return "video"
	case ".mp3", ".wav", ".ogg", ".flac", ".m4a", ".aac":
		return "audio"
	case ".pdf":
		return "pdf"
	case ".md", ".markdown":
		return "markdown"
	}
	if textLikeExtensions[strings.ToLower(path.Ext(name))] {
		return "text"
	}
	return ""
}

// mediaTypeByExt 由扩展名推断 MIME；未知返回 ""（交给内容嗅探）。
// 不直接信任客户端/存储上报的 Content-Type（x-amz-content-type 可被污染）。
func mediaTypeByExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		// 预览场景强制按纯文本直出：脚本失效（配合 CSP 双保险），内容仍可读
		return "text/plain; charset=utf-8"
	}
	if textLikeExtensions[ext] {
		return "text/plain; charset=utf-8"
	}
	if mt := mime.TypeByExtension(ext); mt != "" {
		return mt
	}
	return ""
}

type Server struct {
	engine  *gin.Engine
	sources *source.Manager
}

func New(cfgSources *source.Manager) (*Server, error) {
	gin.SetMode(gin.ReleaseMode)
	s := &Server{engine: gin.New(), sources: cfgSources}
	s.engine.Use(gin.Logger(), gin.Recovery())

	s.engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	grp := s.engine.Group("/api/v1")
	grp.GET("/sources", s.listSources)
	grp.POST("/sources", s.addSource)
	grp.GET("/sources/:id", s.getSource)
	grp.DELETE("/sources/:id", s.removeSource)
	grp.POST("/sources/test", s.testSource) // 放在 :id 之前注册不冲突；路径字面量优先
	grp.GET("/sources/:id/files", s.listFiles)
	grp.GET("/sources/:id/download", s.download)
	grp.GET("/sources/:id/raw", s.raw)
	grp.GET("/sources/:id/presign", s.presign)

	indexHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return nil, err
	}
	s.engine.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})
	s.engine.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": "接口不存在"})
	})
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.engine }

func (s *Server) resolveSource(c *gin.Context) (*source.Source, bool) {
	src, err := s.sources.Get(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "文件源不存在"})
		return nil, false
	}
	return src, true
}

// —— 源管理 ——

func (s *Server) listSources(c *gin.Context) {
	c.JSON(http.StatusOK, s.sources.List())
}

func (s *Server) getSource(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	out := *src
	out.Config = nil // 详情页不回显配置（含打码值），M1 的编辑功能再定策略
	c.JSON(http.StatusOK, out)
}

// addSource 新增文件源。请求体：
// {"id":"r2-demo","name":"显示名","type":"s3","config":{...}}
func (s *Server) addSource(c *gin.Context) {
	var req struct {
		ID     string         `json:"id"`
		Name   string         `json:"name"`
		Type   string         `json:"type"`
		Config map[string]any `json:"config"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	src, err := s.sources.Add(req.ID, req.Name, req.Type, req.Config)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, src)
}

func (s *Server) removeSource(c *gin.Context) {
	if err := s.sources.Remove(c.Param("id")); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// —— 浏览 ——

func (s *Server) listFiles(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	res, err := src.Driver().List(c.Request.Context(), c.Query("path"), driver.ListOptions{
		Cursor: c.Query("cursor"),
		Limit:  limit,
	})
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// serveInline /raw 与 /download 共用的流式输出：disposition 决定 inline 还是 attachment。
func (s *Server) serveInline(c *gin.Context, src *source.Source, path, disposition string) {
	st, err := src.Driver().Stat(c.Request.Context(), path)
	if err != nil {
		s.fail(c, err)
		return
	}
	if st.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"code": "IS_DIR", "message": "目录不支持下载"})
		return
	}
	rc, err := src.Driver().Open(c.Request.Context(), path, driver.OpenOptions{})
	if err != nil {
		s.fail(c, err)
		return
	}
	defer rc.Close()

	name := sanitizeFilename(st.Name)
	mt := mediaTypeByExt(name)
	if mt != "" {
		c.Header("Content-Type", mt) // 明确类型则不依赖嗅探
	}
	c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	// 预览（inline）的 XSS 硬化：禁脚本执行/插件；图片/文本等被浏览器当文档渲染也不落地 cookie。
	if disposition == "inline" {
		c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
		c.Header("X-Content-Type-Options", "nosniff")
	}
	http.ServeContent(c.Writer, c.Request, name, st.ModTime, wrapReadSeeker(c.Request.Context(), src.Driver(), path, rc, st.Size))
}

func (s *Server) download(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path 参数"})
		return
	}

	// 直链模式：显式 ?direct=1 且驱动支持预签名 → 302 跳转，省网关带宽
	if c.Query("direct") == "1" {
		if p, ok := src.Driver().(driver.Presigner); ok {
			u, err := p.PresignGet(c.Request.Context(), path, 15*time.Minute)
			if err != nil {
				s.fail(c, err)
				return
			}
			c.Redirect(http.StatusFound, u)
			return
		}
	}

	s.serveInline(c, src, path, "attachment")
}

// testSource 连接测试（不落盘）。请求体与 addSource 相同（id 可省略）。
func (s *Server) testSource(c *gin.Context) {
	var req struct {
		Type   string         `json:"type"`
		Config map[string]any `json:"config"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	c.JSON(http.StatusOK, s.sources.Test(ctx, req.Type, req.Config))
}

// raw 内联预览输出：仅允许白名单内的可预览类型，杜绝任意 HTML/SVG 直出钓鱼。
func (s *Server) raw(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path 参数"})
		return
	}
	if previewKind(path) == "" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "NOT_PREVIEWABLE", "message": "该文件类型不支持预览"})
		return
	}
	s.serveInline(c, src, path, "inline")
}

func (s *Server) presign(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	p, ok := src.Driver().(driver.Presigner)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": "NOT_SUPPORTED", "message": "该源不支持直链"})
		return
	}
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path 参数"})
		return
	}
	ttl := 15 * time.Minute
	if v := c.Query("ttl"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ttl = time.Duration(n) * time.Second
		}
	}
	u, err := p.PresignGet(c.Request.Context(), path, ttl)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": u, "expires_in": int(ttl.Seconds())})
}

func (s *Server) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, source.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "文件源不存在"})
	case errors.Is(err, source.ErrDuplicate):
		c.JSON(http.StatusConflict, gin.H{"code": "SOURCE_DUPLICATE", "message": err.Error()})
	case errors.Is(err, source.ErrBadID):
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
	case errors.Is(err, driver.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"code": "NOT_FOUND", "message": "对象不存在"})
	case errors.Is(err, driver.ErrInvalidPath):
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PATH", "message": "非法路径"})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"code": "UPSTREAM_ERROR", "message": err.Error()})
	}
}

// sanitizeFilename 清理会破坏 HTTP 头的字符；非 ASCII 名由 FormatMediaType 转 RFC 5987 编码。
func sanitizeFilename(name string) string {
	if name == "" {
		return "file"
	}
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, name)
}
