package api

// 图片动态缩略图与 Office 文档（docx/xlsx/pptx）在线结构化预览 API。

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"ufile/internal/auth"
	"ufile/internal/driver"
	"ufile/internal/office"
	"ufile/internal/source"
	"ufile/internal/thumb"
)

// thumb 登录态动态图片缩略图：GET /api/v1/sources/:id/thumb?path=...&w=200&h=200&q=78
func (s *Server) thumb(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	p := c.Query("path")
	if p == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path 参数"})
		return
	}
	if !s.checkPathAccess(c, src.ID, p, auth.ActionRead) {
		return
	}
	s.serveThumb(c, src, p)
}

// shareThumb 分享公开页动态图片缩略图：GET /api/v1/s/:token/thumb?w=200&h=200&q=78
func (s *Server) shareThumb(c *gin.Context) {
	sh, ok := s.resolveShare(c)
	if !ok {
		return
	}
	if sh.HasPassword() && !s.grantOK(c, sh.Token) {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "PASSWORD_REQUIRED", "message": "请先输入提取码"})
		return
	}
	if sh.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"code": "IS_DIR", "message": "目录不支持缩略图"})
		return
	}
	src, err := s.sources.Get(sh.SourceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "文件源已不可用"})
		return
	}
	s.serveThumb(c, src, sh.Path)
}

func (s *Server) serveThumb(c *gin.Context, src *source.Source, p string) {
	if !thumb.IsSupportedExt(p) {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "UNSUPPORTED_FORMAT", "message": "该格式不支持生成缩略图"})
		return
	}
	w, _ := strconv.Atoi(c.Query("w"))
	h, _ := strconv.Atoi(c.Query("h"))
	q, _ := strconv.Atoi(c.Query("q"))
	opts := thumb.Options{MaxWidth: w, MaxHeight: h, Quality: q}

	st, err := src.Driver().Stat(c.Request.Context(), p)
	if err != nil {
		s.fail(c, err)
		return
	}
	if st.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"code": "IS_DIR", "message": "目录不支持缩略图"})
		return
	}
	if st.Size > thumb.MaxSourceBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": "IMAGE_TOO_LARGE", "message": "原图超过 32MB，无法动态生成缩略图"})
		return
	}

	key, etag := thumb.MakeKey(src.ID, p, st.ModTime.UnixNano(), st.Size, w, h, q)
	if inm := strings.TrimSpace(c.GetHeader("If-None-Match")); inm != "" && inm == etag {
		c.Header("ETag", etag)
		c.Status(http.StatusNotModified)
		return
	}

	if s.thumbs != nil {
		if data, cachedEtag, ok := s.thumbs.Get(key); ok {
			writeThumbResponse(c, data, cachedEtag)
			return
		}
	}

	rc, err := src.Driver().Open(c.Request.Context(), p, driver.OpenOptions{})
	if err != nil {
		s.fail(c, err)
		return
	}
	defer rc.Close()

	data, err := thumb.Generate(rc, opts)
	if err != nil {
		switch {
		case errors.Is(err, thumb.ErrImageTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": "IMAGE_TOO_LARGE", "message": err.Error()})
		case errors.Is(err, thumb.ErrUnsupportedImage):
			c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "UNSUPPORTED_FORMAT", "message": err.Error()})
		default:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"code": "THUMB_FAILED", "message": "图片解码失败: " + err.Error()})
		}
		return
	}

	if s.thumbs != nil {
		s.thumbs.Put(key, etag, data)
	}
	writeThumbResponse(c, data, etag)
}

func writeThumbResponse(c *gin.Context, data []byte, etag string) {
	c.Header("ETag", etag)
	c.Header("Cache-Control", "private, max-age=86400")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self' data:")
	c.Data(http.StatusOK, "image/jpeg", data)
}

// officePreview 登录态 Office 文档结构化预览：GET /api/v1/sources/:id/office?path=...
func (s *Server) officePreview(c *gin.Context) {
	src, ok := s.resolveSource(c)
	if !ok {
		return
	}
	p := c.Query("path")
	if p == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 path 参数"})
		return
	}
	if !s.checkPathAccess(c, src.ID, p, auth.ActionRead) {
		return
	}
	s.serveOffice(c, src, p)
}

// shareOffice 分享公开页 Office 文档结构化预览：GET /api/v1/s/:token/office
func (s *Server) shareOffice(c *gin.Context) {
	sh, ok := s.resolveShare(c)
	if !ok {
		return
	}
	if sh.HasPassword() && !s.grantOK(c, sh.Token) {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "PASSWORD_REQUIRED", "message": "请先输入提取码"})
		return
	}
	if sh.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"code": "IS_DIR", "message": "目录不支持文档预览"})
		return
	}
	src, err := s.sources.Get(sh.SourceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "SOURCE_NOT_FOUND", "message": "文件源已不可用"})
		return
	}
	s.serveOffice(c, src, sh.Path)
}

func (s *Server) serveOffice(c *gin.Context, src *source.Source, p string) {
	if office.IsOfficeExt(p) == "" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "UNSUPPORTED_FORMAT", "message": "仅支持 .docx / .xlsx / .pptx 在线查看"})
		return
	}
	st, err := src.Driver().Stat(c.Request.Context(), p)
	if err != nil {
		s.fail(c, err)
		return
	}
	if st.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"code": "IS_DIR", "message": "目录不支持文档预览"})
		return
	}
	if st.Size > office.MaxOfficeBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": "OFFICE_TOO_LARGE", "message": "文档超过 25MB，请下载到本地查看"})
		return
	}

	rc, err := src.Driver().Open(c.Request.Context(), p, driver.OpenOptions{})
	if err != nil {
		s.fail(c, err)
		return
	}
	defer rc.Close()

	res, err := office.Parse(rc, p)
	if err != nil {
		switch {
		case errors.Is(err, office.ErrFileTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": "OFFICE_TOO_LARGE", "message": err.Error()})
		case errors.Is(err, office.ErrUnsupportedFormat):
			c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "UNSUPPORTED_FORMAT", "message": err.Error()})
		default:
			c.JSON(http.StatusUnprocessableEntity, gin.H{"code": "OFFICE_PARSE_ERROR", "message": "文档解析失败: " + err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, res)
}
