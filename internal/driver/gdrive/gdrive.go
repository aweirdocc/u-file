// Package gdrive 实现 Google Drive API v3 文件源驱动。
//
// 支持认证方式：
//  1. OAuth2 Refresh Token（auth_type="oauth2"，默认）：配置 client_id + client_secret + refresh_token；
//  2. Service Account JSON（auth_type="service_account"）：配置 service_account_json（完整 JSON 密钥文本）。
//
// 特性：
//   - 基于 pathid.Resolver 将 POSIX 相对路径透明映射为 Google Drive fileId；
//   - 支持个人盘与共享云端硬盘（Shared Drive，配置 drive_id）；
//   - 流式下载与 Range 断点续传（alt=media）；
//   - Resumable 流式上传、建目录、改名/移动、删除（移入云盘回收站 trashed=true）；
//   - 支持配置 proxy_url（HTTP/HTTPS/SOCKS5 代理）。
package gdrive

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ufile/internal/driver"
	"ufile/internal/driver/httputil"
	"ufile/internal/driver/pathid"
)

func init() { driver.Register("gdrive", New) }

const (
	defaultAPIBase  = "https://www.googleapis.com"
	defaultTokenURL = "https://oauth2.googleapis.com/token"
	driveScope      = "https://www.googleapis.com/auth/drive"
	folderMimeType  = "application/vnd.google-apps.folder"
)

type serviceAccountKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// Driver Google Drive 驱动实例。
type Driver struct {
	authType     string // "oauth2" | "service_account"
	clientID     string
	clientSecret string
	refreshToken string
	saEmail      string
	saPrivKey    *rsa.PrivateKey
	tokenURL     string
	apiBase      string
	rootFolderID string
	driveID      string

	client   *http.Client
	resolver *pathid.Resolver
	cfg      map[string]any

	tokMu       sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// New 构造 Google Drive 驱动。
func New(cfg map[string]any) (driver.FSDriver, error) {
	authType := strings.ToLower(strings.TrimSpace(driver.Str(cfg, "auth_type", "oauth2")))
	if authType == "" {
		authType = "oauth2"
	}
	cli, err := httputil.NewClient(httputil.Options{
		ProxyURL:    driver.Str(cfg, "proxy_url", ""),
		InsecureTLS: driver.Bool(cfg, "insecure_tls", false),
	})
	if err != nil {
		return nil, err
	}

	rootID := strings.TrimSpace(driver.Str(cfg, "root_folder_id", "root"))
	if rootID == "" {
		rootID = "root"
	}

	d := &Driver{
		authType:     authType,
		apiBase:      strings.TrimRight(driver.Str(cfg, "api_base", defaultAPIBase), "/"),
		tokenURL:     driver.Str(cfg, "token_url", defaultTokenURL),
		rootFolderID: rootID,
		driveID:      strings.TrimSpace(driver.Str(cfg, "drive_id", "")),
		client:       cli,
		cfg:          cfg,
	}

	switch authType {
	case "oauth2":
		cid, err := driver.RequiredStr(cfg, "client_id")
		if err != nil {
			return nil, fmt.Errorf("gdrive: oauth2 模式缺少 client_id")
		}
		csec, err := driver.RequiredStr(cfg, "client_secret")
		if err != nil {
			return nil, fmt.Errorf("gdrive: oauth2 模式缺少 client_secret")
		}
		rtok, err := driver.RequiredStr(cfg, "refresh_token")
		if err != nil {
			return nil, fmt.Errorf("gdrive: oauth2 模式缺少 refresh_token")
		}
		d.clientID = strings.TrimSpace(cid)
		d.clientSecret = strings.TrimSpace(csec)
		d.refreshToken = strings.TrimSpace(rtok)

	case "service_account":
		rawJSON, err := driver.RequiredStr(cfg, "service_account_json")
		if err != nil {
			return nil, fmt.Errorf("gdrive: service_account 模式缺少 service_account_json")
		}
		var sa serviceAccountKey
		if err := json.Unmarshal([]byte(rawJSON), &sa); err != nil {
			return nil, fmt.Errorf("gdrive: 解析 service_account_json 失败: %w", err)
		}
		if sa.ClientEmail == "" || sa.PrivateKey == "" {
			return nil, errors.New("gdrive: service_account_json 缺少 client_email 或 private_key")
		}
		priv, err := parseRSAPrivateKey(sa.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("gdrive: 解析服务账号私钥失败: %w", err)
		}
		d.saEmail = sa.ClientEmail
		d.saPrivKey = priv
		if sa.TokenURI != "" && driver.Str(cfg, "token_url", "") == "" {
			d.tokenURL = sa.TokenURI
		}

	default:
		return nil, fmt.Errorf("gdrive: 不支持的 auth_type %q（可选 oauth2 或 service_account）", authType)
	}

	d.resolver = pathid.NewResolver(d.rootFolderID, pathid.DefaultTTL, d.lookupChild)
	return d, nil
}

func (d *Driver) Scheme() string { return "gdrive" }

func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		List:       true,
		Stat:       true,
		Download:   true,
		ListCursor: true,
		Write:      true,
	}
}

func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("无效的 PEM 格式")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("非 RSA 私钥")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// getAccessToken 获取（并在过期前自动刷新）OAuth2 access_token。
func (d *Driver) getAccessToken(ctx context.Context) (string, error) {
	d.tokMu.Lock()
	defer d.tokMu.Unlock()

	if d.accessToken != "" && time.Now().Add(30*time.Second).Before(d.tokenExpiry) {
		return d.accessToken, nil
	}

	form := url.Values{}
	if d.authType == "service_account" {
		assertion, err := d.buildJWTAssertion()
		if err != nil {
			return "", err
		}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
		form.Set("assertion", assertion)
	} else {
		form.Set("grant_type", "refresh_token")
		form.Set("client_id", d.clientID)
		form.Set("client_secret", d.clientSecret)
		form.Set("refresh_token", d.refreshToken)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gdrive: 刷新令牌请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gdrive: 刷新令牌失败 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokResp); err != nil || tokResp.AccessToken == "" {
		return "", fmt.Errorf("gdrive: 解析令牌响应失败: %w", err)
	}
	if tokResp.ExpiresIn <= 0 {
		tokResp.ExpiresIn = 3600
	}
	d.accessToken = tokResp.AccessToken
	d.tokenExpiry = time.Now().Add(time.Duration(tokResp.ExpiresIn) * time.Second)
	if tokResp.RefreshToken != "" && tokResp.RefreshToken != d.refreshToken {
		d.refreshToken = tokResp.RefreshToken
		driver.NotifyRefreshToken(d.cfg, tokResp.RefreshToken)
	}
	return d.accessToken, nil
}

func (d *Driver) buildJWTAssertion() (string, error) {
	now := time.Now().Unix()
	header := `{"alg":"RS256","typ":"JWT"}`
	claims, err := json.Marshal(map[string]any{
		"iss":   d.saEmail,
		"scope": driveScope,
		"aud":   d.tokenURL,
		"iat":   now,
		"exp":   now + 3600,
	})
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(header)) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	hash := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, d.saPrivKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (d *Driver) newAuthRequest(ctx context.Context, method, fullURL string, body io.Reader) (*http.Request, error) {
	tok, err := d.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return req, nil
}

// —— Google Drive 文件模型与查询辅助 ——

type gFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Size         string `json:"size"`
	ModifiedTime string `json:"modifiedTime"`
	MD5Checksum  string `json:"md5Checksum"`
}

func (f gFile) toNode() pathid.Node {
	isDir := f.MimeType == folderMimeType
	var sz int64
	if !isDir && f.Size != "" {
		sz, _ = strconv.ParseInt(f.Size, 10, 64)
	}
	var mod time.Time
	if f.ModifiedTime != "" {
		mod, _ = time.Parse(time.RFC3339, f.ModifiedTime)
	}
	return pathid.Node{
		ID:      f.ID,
		Name:    f.Name,
		IsDir:   isDir,
		Size:    sz,
		ModTime: mod,
		ETag:    f.MD5Checksum,
	}
}

// escapeQueryLiteral 转义 Google Drive q 参数中的反斜杠与单引号。
func escapeQueryLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

func (d *Driver) applyDriveQueryParams(q url.Values) {
	q.Set("supportsAllDrives", "true")
	q.Set("includeItemsFromAllDrives", "true")
	if d.driveID != "" {
		q.Set("corpora", "drive")
		q.Set("driveId", d.driveID)
	}
}

// lookupChild 在 parentID 下按名称查找单个直接子节点（供 pathid.Resolver 调用）。
func (d *Driver) lookupChild(ctx context.Context, parentID, name string) (pathid.Node, error) {
	q := url.Values{}
	d.applyDriveQueryParams(q)
	q.Set("q", fmt.Sprintf("'%s' in parents and name = '%s' and trashed = false",
		escapeQueryLiteral(parentID), escapeQueryLiteral(name)))
	q.Set("fields", "files(id,name,mimeType,size,modifiedTime,md5Checksum)")
	q.Set("orderBy", "modifiedTime desc")
	q.Set("pageSize", "5")

	req, err := d.newAuthRequest(ctx, http.MethodGet, d.apiBase+"/drive/v3/files?"+q.Encode(), nil)
	if err != nil {
		return pathid.Node{}, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return pathid.Node{}, fmt.Errorf("gdrive: lookup %s: %w", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return pathid.Node{}, driver.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return pathid.Node{}, fmt.Errorf("gdrive: lookup HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var listResp struct {
		Files []gFile `json:"files"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return pathid.Node{}, err
	}
	if len(listResp.Files) == 0 {
		return pathid.Node{}, driver.ErrNotFound
	}
	return listResp.Files[0].toNode(), nil
}

// List 列取目录内容，支持游标分页，并将结果批量预热到路径缓存。
func (d *Driver) List(ctx context.Context, dir string, opt driver.ListOptions) (*driver.ListResult, error) {
	cleanDir, dirNode, err := d.resolver.Resolve(ctx, dir)
	if err != nil {
		return nil, err
	}
	if !dirNode.IsDir {
		return nil, driver.ErrNotFound
	}

	limit := opt.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	q := url.Values{}
	d.applyDriveQueryParams(q)
	q.Set("q", fmt.Sprintf("'%s' in parents and trashed = false", escapeQueryLiteral(dirNode.ID)))
	q.Set("fields", "nextPageToken,files(id,name,mimeType,size,modifiedTime,md5Checksum)")
	q.Set("orderBy", "folder,name")
	q.Set("pageSize", strconv.Itoa(limit))
	if opt.Cursor != "" {
		q.Set("pageToken", opt.Cursor)
	}

	req, err := d.newAuthRequest(ctx, http.MethodGet, d.apiBase+"/drive/v3/files?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gdrive: List %s: %w", cleanDir, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, driver.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gdrive: List HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var listResp struct {
		NextPageToken string  `json:"nextPageToken"`
		Files         []gFile `json:"files"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, err
	}

	nodes := make([]pathid.Node, 0, len(listResp.Files))
	entries := make([]driver.Entry, 0, len(listResp.Files))
	for _, f := range listResp.Files {
		n := f.toNode()
		nodes = append(nodes, n)
		relPath := n.Name
		if cleanDir != "" {
			relPath = cleanDir + "/" + n.Name
		}
		entries = append(entries, n.ToEntry(relPath))
	}
	d.resolver.PutChildren(cleanDir, nodes)

	return &driver.ListResult{
		Entries:    entries,
		NextCursor: listResp.NextPageToken,
	}, nil
}

// Stat 查询文件或目录信息。
func (d *Driver) Stat(ctx context.Context, p string) (*driver.Entry, error) {
	clean, node, err := d.resolver.Resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	e := node.ToEntry(clean)
	return &e, nil
}

// Open 流式打开文件（支持 HTTP Range）。
func (d *Driver) Open(ctx context.Context, p string, opt driver.OpenOptions) (io.ReadCloser, error) {
	clean, node, err := d.resolver.Resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if clean == "" || node.IsDir {
		return nil, driver.ErrInvalidPath
	}

	q := url.Values{}
	q.Set("alt", "media")
	q.Set("supportsAllDrives", "true")
	dlURL := fmt.Sprintf("%s/drive/v3/files/%s?%s", d.apiBase, url.PathEscape(node.ID), q.Encode())

	req, err := d.newAuthRequest(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, err
	}
	if rng := httputil.FormatRangeHeader(opt); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gdrive: Open %s: %w", clean, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, driver.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("gdrive: Open HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

// ensureDir 确保相对目录路径存在，返回最终目录的 Node。
func (d *Driver) ensureDir(ctx context.Context, rawDir string) (pathid.Node, error) {
	clean, err := driver.CleanPath(rawDir)
	if err != nil {
		return pathid.Node{}, err
	}
	if clean == "" {
		return pathid.Node{ID: d.rootFolderID, IsDir: true}, nil
	}
	if n, ok := d.resolver.Get(clean); ok && n.IsDir {
		return n, nil
	}
	segs := strings.Split(clean, "/")
	curID := d.rootFolderID
	curPath := ""

	for i, seg := range segs {
		if i == 0 {
			curPath = seg
		} else {
			curPath += "/" + seg
		}
		if n, ok := d.resolver.Get(curPath); ok {
			if !n.IsDir {
				return pathid.Node{}, fmt.Errorf("gdrive: 路径 %q 已存在同名文件", curPath)
			}
			curID = n.ID
			continue
		}
		n, err := d.lookupChild(ctx, curID, seg)
		if err == nil {
			if !n.IsDir {
				return pathid.Node{}, fmt.Errorf("gdrive: 路径 %q 已存在同名文件", curPath)
			}
			d.resolver.Put(curPath, n)
			curID = n.ID
			continue
		}
		if !errors.Is(err, driver.ErrNotFound) {
			return pathid.Node{}, err
		}
		// 创建文件夹
		created, err := d.createFolder(ctx, curID, seg)
		if err != nil {
			return pathid.Node{}, err
		}
		d.resolver.Put(curPath, created)
		curID = created.ID
	}
	return pathid.Node{ID: curID, Name: segs[len(segs)-1], IsDir: true}, nil
}

func (d *Driver) createFolder(ctx context.Context, parentID, name string) (pathid.Node, error) {
	meta, err := json.Marshal(map[string]any{
		"name":     name,
		"mimeType": folderMimeType,
		"parents":  []string{parentID},
	})
	if err != nil {
		return pathid.Node{}, err
	}
	u := d.apiBase + "/drive/v3/files?supportsAllDrives=true&fields=id,name,mimeType,modifiedTime"
	req, err := d.newAuthRequest(ctx, http.MethodPost, u, bytes.NewReader(meta))
	if err != nil {
		return pathid.Node{}, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := d.client.Do(req)
	if err != nil {
		return pathid.Node{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pathid.Node{}, fmt.Errorf("gdrive: 创建目录 %q 失败 (HTTP %d): %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var f gFile
	if err := json.Unmarshal(body, &f); err != nil {
		return pathid.Node{}, err
	}
	return f.toNode(), nil
}

// Mkdir 递归创建目录。
func (d *Driver) Mkdir(ctx context.Context, p string) error {
	_, err := d.ensureDir(ctx, p)
	return err
}

// Upload 使用 Google Drive Resumable Upload 协议流式上传文件（若同名文件已存在则原地覆盖内容）。
func (d *Driver) Upload(ctx context.Context, p string, r io.Reader, size int64) error {
	parentClean, baseName, err := pathid.SplitParent(p)
	if err != nil {
		return err
	}
	parentNode, err := d.ensureDir(ctx, parentClean)
	if err != nil {
		return err
	}
	cleanPath := baseName
	if parentClean != "" {
		cleanPath = parentClean + "/" + baseName
	}

	// 检查是否已有同名文件（避免产生重复同名文件）
	var existingID string
	if ex, err := d.lookupChild(ctx, parentNode.ID, baseName); err == nil && !ex.IsDir {
		existingID = ex.ID
	}

	var initMethod, initURL string
	var metaBytes []byte
	if existingID != "" {
		initMethod = http.MethodPatch
		initURL = fmt.Sprintf("%s/upload/drive/v3/files/%s?uploadType=resumable&supportsAllDrives=true&fields=id,name,mimeType,size,modifiedTime,md5Checksum",
			d.apiBase, url.PathEscape(existingID))
		metaBytes, _ = json.Marshal(map[string]any{"name": baseName})
	} else {
		initMethod = http.MethodPost
		initURL = d.apiBase + "/upload/drive/v3/files?uploadType=resumable&supportsAllDrives=true&fields=id,name,mimeType,size,modifiedTime,md5Checksum"
		metaBytes, _ = json.Marshal(map[string]any{
			"name":    baseName,
			"parents": []string{parentNode.ID},
		})
	}

	initReq, err := d.newAuthRequest(ctx, initMethod, initURL, bytes.NewReader(metaBytes))
	if err != nil {
		return err
	}
	initReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	if size >= 0 {
		initReq.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
	}

	initResp, err := d.client.Do(initReq)
	if err != nil {
		return fmt.Errorf("gdrive: 初始化上传失败: %w", err)
	}
	initBody, _ := io.ReadAll(io.LimitReader(initResp.Body, 16<<10))
	initResp.Body.Close()
	if initResp.StatusCode < 200 || initResp.StatusCode >= 300 {
		return fmt.Errorf("gdrive: 初始化上传 HTTP %d: %s", initResp.StatusCode, strings.TrimSpace(string(initBody)))
	}
	sessionURL := initResp.Header.Get("Location")
	if sessionURL == "" {
		return errors.New("gdrive: 未返回 Resumable Location URL")
	}

	if r == nil {
		r = bytes.NewReader(nil)
	}
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURL, r)
	if err != nil {
		return err
	}
	if size >= 0 {
		putReq.ContentLength = size
	}
	putResp, err := d.client.Do(putReq)
	if err != nil {
		return fmt.Errorf("gdrive: 上传数据流失败: %w", err)
	}
	defer putResp.Body.Close()
	putBody, _ := io.ReadAll(io.LimitReader(putResp.Body, 64<<10))
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		return fmt.Errorf("gdrive: 上传失败 HTTP %d: %s", putResp.StatusCode, strings.TrimSpace(string(putBody)))
	}

	var uploaded gFile
	if err := json.Unmarshal(putBody, &uploaded); err == nil && uploaded.ID != "" {
		d.resolver.Put(cleanPath, uploaded.toNode())
	} else {
		d.resolver.Invalidate(cleanPath)
	}
	return nil
}

// Delete 将文件或目录移入 Google Drive 回收站（trashed = true）。
func (d *Driver) Delete(ctx context.Context, p string) error {
	clean, node, err := d.resolver.Resolve(ctx, p)
	if err != nil {
		return err
	}
	if clean == "" {
		return driver.ErrInvalidPath
	}

	u := fmt.Sprintf("%s/drive/v3/files/%s?supportsAllDrives=true", d.apiBase, url.PathEscape(node.ID))
	req, err := d.newAuthRequest(ctx, http.MethodPatch, u, strings.NewReader(`{"trashed":true}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("gdrive: Delete %s: %w", clean, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode == http.StatusNotFound {
		d.resolver.Invalidate(clean)
		return driver.ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gdrive: Delete HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	d.resolver.Invalidate(clean)
	return nil
}

// Rename 重命名或移动文件/目录。
func (d *Driver) Rename(ctx context.Context, from, to string) error {
	cleanFrom, srcNode, err := d.resolver.Resolve(ctx, from)
	if err != nil {
		return err
	}
	if cleanFrom == "" {
		return driver.ErrInvalidPath
	}
	oldParentClean, _, err := pathid.SplitParent(cleanFrom)
	if err != nil {
		return err
	}
	newParentClean, newName, err := pathid.SplitParent(to)
	if err != nil {
		return err
	}
	cleanTo := newName
	if newParentClean != "" {
		cleanTo = newParentClean + "/" + newName
	}

	q := url.Values{}
	q.Set("supportsAllDrives", "true")
	q.Set("fields", "id,name,mimeType,size,modifiedTime,md5Checksum")

	targetParentNode, err := d.ensureDir(ctx, newParentClean)
	if err != nil {
		return err
	}
	if oldParentClean != newParentClean {
		_, oldParentNode, err := d.resolver.Resolve(ctx, oldParentClean)
		if err != nil {
			return err
		}
		q.Set("addParents", targetParentNode.ID)
		q.Set("removeParents", oldParentNode.ID)
	}

	// 若目标目录下已存在同名文件（且不是当前文件自身），先移入回收站以保证覆盖语义、避免产生同名文件歧义
	if ex, err := d.lookupChild(ctx, targetParentNode.ID, newName); err == nil && ex.ID != srcNode.ID {
		if ex.IsDir {
			return fmt.Errorf("gdrive: 目标路径 %q 已存在同名文件夹", cleanTo)
		}
		trashURL := fmt.Sprintf("%s/drive/v3/files/%s?supportsAllDrives=true", d.apiBase, url.PathEscape(ex.ID))
		if treq, err := d.newAuthRequest(ctx, http.MethodPatch, trashURL, strings.NewReader(`{"trashed":true}`)); err == nil {
			treq.Header.Set("Content-Type", "application/json; charset=utf-8")
			if tresp, err := d.client.Do(treq); err == nil {
				tresp.Body.Close()
			}
		}
	}

	payload, _ := json.Marshal(map[string]any{"name": newName})
	u := fmt.Sprintf("%s/drive/v3/files/%s?%s", d.apiBase, url.PathEscape(srcNode.ID), q.Encode())
	req, err := d.newAuthRequest(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("gdrive: Rename %s -> %s: %w", cleanFrom, cleanTo, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gdrive: Rename HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	d.resolver.Invalidate(cleanFrom)
	d.resolver.Invalidate(cleanTo)
	return nil
}
