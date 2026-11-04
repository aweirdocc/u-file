// Package webdav 实现标准 WebDAV（RFC 4918）文件源驱动，
// 原生兼容坚果云（https://dav.jianguoyun.com/dav/）、Nextcloud、ownCloud、群晖/威联通 NAS 及 OpenList WebDAV。
package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"ufile/internal/driver"
	"ufile/internal/driver/httputil"
)

func init() { driver.Register("webdav", New) }

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:displayname/>
    <D:getcontentlength/>
    <D:getlastmodified/>
    <D:getetag/>
    <D:resourcetype/>
  </D:prop>
</D:propfind>`

// Driver WebDAV 驱动实例。
type Driver struct {
	baseURL  *url.URL
	basePath string // endpoint URL 中的路径部分，去除首尾 /
	root     string // 用户配置的 root 子路径，去除首尾 /
	username string
	password string
	client   *http.Client
}

// New 构造 WebDAV 驱动。
// 配置项：
//   - endpoint（必填）：如 https://dav.jianguoyun.com/dav/我的坚果云
//   - username（可选）：账号
//   - password（可选）：密码或应用专用密码
//   - root（可选）：挂载子目录前缀
//   - insecure_tls（可选）：是否跳过证书校验
//   - proxy_url（可选）：HTTP/SOCKS5 代理
func New(cfg map[string]any) (driver.FSDriver, error) {
	ep, err := driver.RequiredStr(cfg, "endpoint")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimSpace(ep))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("webdav: 无效的 endpoint %q（需以 http:// 或 https:// 开头）", ep)
	}
	cli, err := httputil.NewClient(httputil.Options{
		ProxyURL:    driver.Str(cfg, "proxy_url", ""),
		InsecureTLS: driver.Bool(cfg, "insecure_tls", false),
	})
	if err != nil {
		return nil, err
	}
	basePath := strings.Trim(u.Path, "/")
	root := strings.Trim(driver.Str(cfg, "root", ""), "/")
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""

	return &Driver{
		baseURL:  u,
		basePath: basePath,
		root:     root,
		username: driver.Str(cfg, "username", ""),
		password: driver.Str(cfg, "password", ""),
		client:   cli,
	}, nil
}

func (d *Driver) Scheme() string { return "webdav" }

func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		List:     true,
		Stat:     true,
		Download: true,
		Write:    true,
	}
}

// joinedPrefix 返回 basePath + root 的组合路径（不含首尾 /）。
func (d *Driver) joinedPrefix() string {
	switch {
	case d.basePath != "" && d.root != "":
		return d.basePath + "/" + d.root
	case d.basePath != "":
		return d.basePath
	default:
		return d.root
	}
}

// buildURL 构造目标相对路径的完整 URL；trailingSlash=true 时末尾加 /（集合/目录操作友好）。
func (d *Driver) buildURL(rawRelPath string, trailingSlash bool) (string, string, error) {
	clean, err := driver.CleanPath(rawRelPath)
	if err != nil {
		return "", "", err
	}
	prefix := d.joinedPrefix()
	full := clean
	if prefix != "" {
		if clean != "" {
			full = prefix + "/" + clean
		} else {
			full = prefix
		}
	}
	var esc strings.Builder
	esc.WriteString(d.baseURL.String())
	if full == "" {
		esc.WriteByte('/')
		return esc.String(), clean, nil
	}
	for seg := range strings.SplitSeq(full, "/") {
		if seg == "" {
			continue
		}
		esc.WriteByte('/')
		esc.WriteString(url.PathEscape(seg))
	}
	if trailingSlash {
		esc.WriteByte('/')
	}
	return esc.String(), clean, nil
}

func (d *Driver) newRequest(ctx context.Context, method, targetURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, targetURL, body)
	if err != nil {
		return nil, err
	}
	if d.username != "" || d.password != "" {
		req.SetBasicAuth(d.username, d.password)
	}
	return req, nil
}

func mapStatusError(op string, statusCode int, body []byte) error {
	switch statusCode {
	case http.StatusNotFound, http.StatusGone:
		return driver.ErrNotFound
	case http.StatusUnauthorized:
		return fmt.Errorf("webdav: %s 认证失败（401）：请检查用户名或应用专用密码", op)
	case http.StatusForbidden:
		return fmt.Errorf("webdav: %s 权限不足（403）：坚果云请注意不要直接操作 /dav/ 根目录，建议挂载子目录（如 /dav/我的坚果云）", op)
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return fmt.Errorf("webdav: %s 请求过于频繁或服务暂不可用（%d）", op, statusCode)
	default:
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		if msg != "" {
			return fmt.Errorf("webdav: %s 返回 HTTP %d: %s", op, statusCode, msg)
		}
		return fmt.Errorf("webdav: %s 返回 HTTP %d", op, statusCode)
	}
}

// —— XML 解析结构 ——

type multiStatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string     `xml:"href"`
	PropStat []propStat `xml:"propstat"`
}

type propStat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	DisplayName   string       `xml:"displayname"`
	ContentLength string       `xml:"getcontentlength"`
	LastModified  string       `xml:"getlastmodified"`
	ETag          string       `xml:"getetag"`
	ResourceType  resourceType `xml:"resourcetype"`
}

type resourceType struct {
	Collection *struct{} `xml:"collection"`
}

func (r davResponse) okProp() (davProp, bool) {
	for _, ps := range r.PropStat {
		if strings.Contains(ps.Status, " 200 ") || ps.Status == "" {
			return ps.Prop, true
		}
	}
	if len(r.PropStat) > 0 {
		return r.PropStat[0].Prop, true
	}
	return davProp{}, false
}

func parseModTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		http.TimeFormat,
		time.RFC1123,
		time.RFC1123Z,
		time.RFC3339,
		time.RFC850,
		time.ANSIC,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// decodeHrefPath 提取 href 中的解码后路径（不含首尾 /）。
// 注意：url.Parse 成功后 u.Path 已经完成一次百分号解码，切勿再次调用 url.PathUnescape，
// 否则文件名中含有字面量 '%'（如 "100%25bar.txt" -> "100%bar.txt"）会被二次解码破坏。
func decodeHrefPath(href string) string {
	href = strings.TrimSpace(href)
	if u, err := url.Parse(href); err == nil && (u.Scheme != "" || u.Host != "" || u.Path != "") {
		return strings.Trim(u.Path, "/")
	}
	if unesc, err := url.PathUnescape(href); err == nil {
		href = unesc
	}
	return strings.Trim(href, "/")
}

func (d *Driver) propfind(ctx context.Context, rawRelPath string, depth string, trailingSlash bool) ([]davResponse, string, error) {
	targetURL, clean, err := d.buildURL(rawRelPath, trailingSlash)
	if err != nil {
		return nil, "", err
	}
	req, err := d.newRequest(ctx, "PROPFIND", targetURL, strings.NewReader(propfindBody))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("webdav: PROPFIND %s: %w", clean, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusMultiStatus && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return nil, "", mapStatusError("PROPFIND", resp.StatusCode, body)
	}
	var ms multiStatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return nil, "", fmt.Errorf("webdav: 解析 PROPFIND XML 失败: %w", err)
	}
	return ms.Responses, clean, nil
}

// List 列取指定目录下的直接子项。
func (d *Driver) List(ctx context.Context, dir string, opt driver.ListOptions) (*driver.ListResult, error) {
	resps, cleanDir, err := d.propfind(ctx, dir, "1", true)
	if err != nil {
		return nil, err
	}
	reqFull := d.joinedPrefix()
	if cleanDir != "" {
		if reqFull != "" {
			reqFull += "/" + cleanDir
		} else {
			reqFull = cleanDir
		}
	}
	reqFull = strings.Trim(reqFull, "/")

	entries := make([]driver.Entry, 0, max(0, len(resps)-1))
	for _, r := range resps {
		p, ok := r.okProp()
		if !ok {
			continue
		}
		hrefPath := decodeHrefPath(r.Href)
		// 跳过目录自身
		if hrefPath == reqFull {
			continue
		}
		name := path.Base(hrefPath)
		if name == "" || name == "." || name == "/" {
			continue
		}
		relPath := name
		if cleanDir != "" {
			relPath = cleanDir + "/" + name
		}
		isDir := p.ResourceType.Collection != nil || strings.HasSuffix(strings.TrimSpace(r.Href), "/")
		var sz int64
		if !isDir && p.ContentLength != "" {
			sz, _ = strconv.ParseInt(strings.TrimSpace(p.ContentLength), 10, 64)
		}
		entries = append(entries, driver.Entry{
			Path:    relPath,
			Name:    name,
			IsDir:   isDir,
			Size:    sz,
			ModTime: parseModTime(p.LastModified),
			ETag:    strings.Trim(strings.TrimSpace(p.ETag), `"`),
		})
	}
	return &driver.ListResult{Entries: entries}, nil
}

// Stat 查询单个文件或目录元数据。
func (d *Driver) Stat(ctx context.Context, p string) (*driver.Entry, error) {
	clean, err := driver.CleanPath(p)
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return &driver.Entry{Path: "", Name: "", IsDir: true}, nil
	}
	resps, _, err := d.propfind(ctx, clean, "0", false)
	if err != nil {
		return nil, err
	}
	if len(resps) == 0 {
		return nil, driver.ErrNotFound
	}
	prop, ok := resps[0].okProp()
	if !ok {
		return nil, driver.ErrNotFound
	}
	isDir := prop.ResourceType.Collection != nil
	var sz int64
	if !isDir && prop.ContentLength != "" {
		sz, _ = strconv.ParseInt(strings.TrimSpace(prop.ContentLength), 10, 64)
	}
	return &driver.Entry{
		Path:    clean,
		Name:    path.Base(clean),
		IsDir:   isDir,
		Size:    sz,
		ModTime: parseModTime(prop.LastModified),
		ETag:    strings.Trim(strings.TrimSpace(prop.ETag), `"`),
	}, nil
}

// Open 打开文件流（支持 HTTP Range 读取）。
func (d *Driver) Open(ctx context.Context, p string, opt driver.OpenOptions) (io.ReadCloser, error) {
	targetURL, clean, err := d.buildURL(p, false)
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return nil, driver.ErrInvalidPath
	}
	req, err := d.newRequest(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	if rng := httputil.FormatRangeHeader(opt); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdav: GET %s: %w", clean, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, mapStatusError("GET", resp.StatusCode, body)
	}
	return resp.Body, nil
}

// Mkdir 递归创建目录（按层级发送 MKCOL）。
func (d *Driver) Mkdir(ctx context.Context, p string) error {
	clean, err := driver.CleanPath(p)
	if err != nil {
		return err
	}
	if clean == "" {
		return nil
	}
	segs := strings.Split(clean, "/")
	cur := ""
	for i, seg := range segs {
		if i == 0 {
			cur = seg
		} else {
			cur += "/" + seg
		}
		u, _, err := d.buildURL(cur, true)
		if err != nil {
			return err
		}
		req, err := d.newRequest(ctx, "MKCOL", u, nil)
		if err != nil {
			return err
		}
		resp, err := d.client.Do(req)
		if err != nil {
			return fmt.Errorf("webdav: MKCOL %s: %w", cur, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		// 201 Created 成功；405 Method Not Allowed 在 WebDAV 标准中表示集合已存在
		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusOK {
			continue
		}
		// 如果中间目录已存在但部分服务端返回 409/301，可通过 Stat 二次确认
		if st, stErr := d.Stat(ctx, cur); stErr == nil && st.IsDir {
			continue
		}
		return mapStatusError("MKCOL", resp.StatusCode, body)
	}
	return nil
}

// Upload 流式上传文件（PUT）。若父目录不存在会自动递归创建。
func (d *Driver) Upload(ctx context.Context, p string, r io.Reader, size int64) error {
	targetURL, clean, err := d.buildURL(p, false)
	if err != nil {
		return err
	}
	if clean == "" {
		return driver.ErrInvalidPath
	}
	if parent := path.Dir(clean); parent != "." && parent != "" {
		if err := d.Mkdir(ctx, parent); err != nil {
			return err
		}
	}
	if r == nil {
		r = bytes.NewReader(nil)
	}
	req, err := d.newRequest(ctx, http.MethodPut, targetURL, r)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav: PUT %s: %w", clean, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapStatusError("PUT", resp.StatusCode, body)
	}
	return nil
}

// Delete 删除文件或目录（DELETE）。
func (d *Driver) Delete(ctx context.Context, p string) error {
	targetURL, clean, err := d.buildURL(p, false)
	if err != nil {
		return err
	}
	if clean == "" {
		return driver.ErrInvalidPath
	}
	req, err := d.newRequest(ctx, http.MethodDelete, targetURL, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav: DELETE %s: %w", clean, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return driver.ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapStatusError("DELETE", resp.StatusCode, body)
	}
	return nil
}

// Rename 同一 WebDAV 源内移动或重命名（MOVE）。
func (d *Driver) Rename(ctx context.Context, from, to string) error {
	srcURL, cleanFrom, err := d.buildURL(from, false)
	if err != nil {
		return err
	}
	dstURL, cleanTo, err := d.buildURL(to, false)
	if err != nil {
		return err
	}
	if cleanFrom == "" || cleanTo == "" {
		return driver.ErrInvalidPath
	}
	if parent := path.Dir(cleanTo); parent != "." && parent != "" {
		if err := d.Mkdir(ctx, parent); err != nil {
			return err
		}
	}
	req, err := d.newRequest(ctx, "MOVE", srcURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", dstURL)
	req.Header.Set("Overwrite", "T")

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav: MOVE %s -> %s: %w", cleanFrom, cleanTo, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapStatusError("MOVE", resp.StatusCode, body)
	}
	return nil
}
