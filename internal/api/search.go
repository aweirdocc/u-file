package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"ufile/internal/apitoken"
	"ufile/internal/auth"
)

// search 文件名搜索或全文内容检索（mode=filename|fulltext）。
// GET /api/v1/search?q=关键词&source=源ID(可选)&mode=filename|fulltext&limit=50
func (s *Server) search(c *gin.Context) {
	q := c.Query("q")
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PARAM", "message": "缺少 q 参数"})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = 50
	}
	srcFilter := c.Query("source")
	if srcFilter != "" && !s.checkSourceAccess(c, srcFilter, auth.ActionRead) {
		return
	}

	var apiTok *apitoken.Token
	if v, ok := c.Get("api_token"); ok {
		apiTok, _ = v.(*apitoken.Token)
	}

	names := map[string]string{}
	for _, src := range s.sources.List() {
		names[src.ID] = src.Name
	}
	user := s.currentUsername(c)
	mode := c.Query("mode")

	if mode == "fulltext" {
		if s.fulltext == nil {
			c.JSON(http.StatusOK, gin.H{"query": q, "mode": "fulltext", "count": 0, "hits": []gin.H{}})
			return
		}
		hits := s.fulltext.Search(q, srcFilter, limit*5)
		out := make([]gin.H, 0, len(hits))
		for _, h := range hits {
			if apiTok != nil && !apiTok.AllowsSource(h.SourceID) {
				continue
			}
			if s.auth != nil && !s.auth.CanAccess(user, h.SourceID, h.Path, auth.ActionRead) {
				continue
			}
			out = append(out, gin.H{
				"source_id":   h.SourceID,
				"source_name": names[h.SourceID],
				"path":        h.Path,
				"name":        h.Name,
				"is_dir":      h.IsDir,
				"size":        h.Size,
				"mod_time":    h.ModTime,
				"snippet":     h.Snippet,
			})
			if len(out) >= limit {
				break
			}
		}
		c.JSON(http.StatusOK, gin.H{"query": q, "mode": "fulltext", "count": len(out), "hits": out})
		return
	}

	if s.index == nil {
		c.JSON(http.StatusOK, gin.H{"query": q, "mode": "filename", "count": 0, "hits": []gin.H{}})
		return
	}
	hits := s.index.Search(q, srcFilter, limit*5)
	out := make([]gin.H, 0, len(hits))
	for _, h := range hits {
		if apiTok != nil && !apiTok.AllowsSource(h.SourceID) {
			continue
		}
		if s.auth != nil && !s.auth.CanAccess(user, h.SourceID, h.Path, auth.ActionRead) {
			continue
		}
		out = append(out, gin.H{
			"source_id":   h.SourceID,
			"source_name": names[h.SourceID],
			"path":        h.Path,
			"name":        h.Name,
			"is_dir":      h.IsDir,
			"size":        h.Size,
			"mod_time":    h.ModTime,
		})
		if len(out) >= limit {
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{"query": q, "mode": "filename", "count": len(out), "hits": out})
}
