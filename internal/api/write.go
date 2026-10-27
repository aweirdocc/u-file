package api

// M2 写操作与打包下载：上传、建目录、删除（批量）、重命名、zip 流式打包。
// 写操作仅 admin 且驱动实现 Writer；打包走流式 zip（不落盘、不整载内存）。

import (
	"archive/zip"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"ufile/internal/driver"
	"ufile/internal/source"
)

// writerOf 探测驱动写能力。
func writerOf(src *source.Source) (driver.Writer, bool) {
	w, ok := src.Driver().(driver.Writer)
	return w, ok
}

// upload multipart 上传。表单字段：file（可多个，同名字段重复）+ path（目标目录，默认根）。
// 流式转发：gin 的 FormFile 会先把小文件落内存/临时文件（multipart 语义所限），
// 单文件上限由 MaxMultipartMemory 控制（默认 32MB 内存阈值，超出写临时文件，不整载）。
func (s *Server) upload(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	w, ok := writerOf(src)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": "NOT_SUPPORTED", "message": "该源不支持上传"})
		return
	}
	dir := c.PostForm("path") // 目标目录
	if dir == "" {
		dir = c.Query("path")
	}
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": err.Error()})
		return
	}
	fhs := form.File["file"]
	if len(fhs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 file 字段"})
		return
	}
	results := make([]gin.H, 0, len(fhs))
	okCount, failCount := 0, 0
	for _, fh := range fhs {
		name := path.Base(fh.Filename)
		// Windows 客户端有时带完整路径，取最后一段
		if i := strings.LastIndexAny(name, `/\`); i >= 0 {
			name = name[i+1:]
		}
		target := path.Join(dir, name)
		f, err := fh.Open()
		if err != nil {
			results = append(results, gin.H{"file": name, "ok": false, "error": err.Error()})
			failCount++
			continue
		}
		err = w.Upload(c.Request.Context(), target, f, fh.Size)
		f.Close()
		if err != nil {
			results = append(results, gin.H{"file": name, "ok": false, "error": err.Error()})
			failCount++
			continue
		}
		results = append(results, gin.H{"file": name, "ok": true, "path": target})
		okCount++
	}
	s.auditAction(c, "file.upload", gin.H{"source": src.ID, "dir": dir, "ok": okCount, "failed": failCount})
	// 上传后立即失效该目录缓存，浏览即时可见
	if s.index != nil {
		s.index.Invalidate(src.ID)
	}
	status := http.StatusOK
	if okCount == 0 && failCount > 0 {
		status = http.StatusBadGateway
	}
	c.JSON(status, gin.H{"ok": failCount == 0, "uploaded": okCount, "failed": failCount, "items": results})
}

// mkdir 新建目录。请求体：{"path": "a/b/c"}
func (s *Server) mkdir(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	w, ok := writerOf(src)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": "NOT_SUPPORTED", "message": "该源不支持写操作"})
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path"})
		return
	}
	if err := w.Mkdir(c.Request.Context(), req.Path); err != nil {
		s.fail(c, err)
		return
	}
	s.auditAction(c, "file.mkdir", gin.H{"source": src.ID, "path": req.Path})
	if s.index != nil {
		s.index.Invalidate(src.ID)
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true, "path": req.Path})
}

// deleteEntries 批量删除。请求体：{"paths": ["a", "b/c"]}（目录递归删）。
func (s *Server) deleteEntries(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	w, ok := writerOf(src)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": "NOT_SUPPORTED", "message": "该源不支持写操作"})
		return
	}
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Paths) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 paths"})
		return
	}
	results := make([]gin.H, 0, len(req.Paths))
	okCount, failCount := 0, 0
	for _, p := range req.Paths {
		err := w.Delete(c.Request.Context(), p)
		if errors.Is(err, driver.ErrInvalidPath) {
			results = append(results, gin.H{"path": p, "ok": false, "error": "非法路径或不可删除"})
			failCount++
			continue
		}
		if err != nil {
			results = append(results, gin.H{"path": p, "ok": false, "error": err.Error()})
			failCount++
			continue
		}
		results = append(results, gin.H{"path": p, "ok": true})
		okCount++
	}
	s.auditAction(c, "file.delete", gin.H{"source": src.ID, "ok": okCount, "failed": failCount})
	if s.index != nil {
		s.index.Invalidate(src.ID)
	}
	c.JSON(http.StatusOK, gin.H{"ok": failCount == 0, "deleted": okCount, "failed": failCount, "items": results})
}

// renameEntry 重命名/移动。请求体：{"from": "a/old.txt", "to": "b/new.txt"}
func (s *Server) renameEntry(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	w, ok := writerOf(src)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": "NOT_SUPPORTED", "message": "该源不支持写操作"})
		return
	}
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.From == "" || req.To == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "需要 from 与 to"})
		return
	}
	if err := w.Rename(c.Request.Context(), req.From, req.To); err != nil {
		s.fail(c, err)
		return
	}
	s.auditAction(c, "file.rename", gin.H{"source": src.ID, "from": req.From, "to": req.To})
	if s.index != nil {
		s.index.Invalidate(src.ID)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// archive zip 流式打包下载。
// GET /archive?paths=a.txt&paths=docs/&name=backup —— paths 可多个；
// 以 / 结尾或 Stat 为目录的条目递归打包（目录递归上限 10000 文件/2GB，防失控）。
func (s *Server) archive(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	paths := c.QueryArray("paths")
	if len(paths) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 paths"})
		return
	}
	name := strings.TrimSuffix(path.Base(c.Query("name")), ".zip")
	if name == "" {
		name = src.ID
	}
	if name == "" || name == "." {
		name = "download"
	}
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
	c.Header("X-Content-Type-Options", "nosniff")

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()
	counter := &entryCounter{}
	for _, p := range paths {
		if err := s.archiveEntry(c, zw, src, p, counter); err != nil {
			// 已开始输出 zip 字节流，无法改状态码；截断即客户端解压报错（zip 规范如此）
			break
		}
	}
	s.auditAction(c, "file.archive", gin.H{"source": src.ID, "paths": paths, "files": counter.n})
}

type entryCounter struct {
	n     int
	bytes int64
}

const (
	archiveMaxFiles      = 10000
	archiveMaxTotalBytes = 2 << 30 // 2GB
)

func (s *Server) archiveEntry(c *gin.Context, zw *zip.Writer, src *source.Source, p string, counter *entryCounter) error {
	st, err := src.Driver().Stat(c.Request.Context(), p)
	if err != nil {
		return err
	}
	if !st.IsDir {
		return zipFile(c, zw, src, st.Path, st.Name, counter)
	}
	// 目录：BFS 递归
	queue := []string{st.Path}
	for len(queue) > 0 {
		if counter.n >= archiveMaxFiles || counter.bytes > archiveMaxTotalBytes {
			return errors.New("打包内容超出上限（10000 文件 / 2GB）")
		}
		dir := queue[0]
		queue = queue[1:]
		res, err := src.Driver().List(c.Request.Context(), dir, driver.ListOptions{})
		if err != nil {
			continue // 单目录失败跳过，尽量把其余内容打出来
		}
		for _, e := range res.Entries {
			if e.IsDir {
				queue = append(queue, e.Path)
				continue
			}
			if err := zipFile(c, zw, src, e.Path, strings.TrimPrefix(e.Path, st.Path+"/"), counter); err != nil {
				return err
			}
		}
	}
	return nil
}

func zipFile(c *gin.Context, zw *zip.Writer, src *source.Source, p, arcname string, counter *entryCounter) error {
	if counter.n >= archiveMaxFiles || counter.bytes > archiveMaxTotalBytes {
		return errors.New("打包内容超出上限（10000 文件 / 2GB）")
	}
	rc, err := src.Driver().Open(c.Request.Context(), p, driver.OpenOptions{})
	if err != nil {
		return err // 单文件失败终止（保持 zip 一致性简单化：M2 边界）
	}
	defer rc.Close()
	hdr := &zip.FileHeader{Name: arcname, Method: zip.Store} // Store 不二次压缩：媒体/已压缩内容为主，快
	hdr.SetMode(0o644)
	hdr.SetModTime(time.Now())
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	n, err := io.Copy(w, rc)
	counter.n++
	counter.bytes += n
	return err
}
