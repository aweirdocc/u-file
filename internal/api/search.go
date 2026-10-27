package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// search 文件名搜索（基于索引缓存/快照）。
// GET /api/v1/search?q=关键词&source=源ID(可选)&limit=50
func (s *Server) search(c *gin.Context) {
	q := c.Query("q")
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 q 参数"})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	hits := s.index.Search(q, c.Query("source"), limit)
	// 附加源显示名
	names := map[string]string{}
	for _, src := range s.sources.List() {
		names[src.ID] = src.Name
	}
	out := make([]gin.H, 0, len(hits))
	for _, h := range hits {
		out = append(out, gin.H{
			"source_id":   h.SourceID,
			"source_name": names[h.SourceID],
			"path":        h.Path,
			"name":        h.Name,
			"is_dir":      h.IsDir,
			"size":        h.Size,
			"mod_time":    h.ModTime,
		})
	}
	c.JSON(http.StatusOK, gin.H{"query": q, "count": len(out), "hits": out})
}
