// Package alipan 实现阿里云盘官方开放平台（openapi.alipan.com）文件源驱动。
//
// 支持授权模式：
//  1. 官方自建应用模式：配置 refresh_token + client_id + client_secret；
//  2. 三方 OAuth 令牌刷新代理模式：配置 refresh_token + oauth_token_url（个人用户免企业资质常用）。
//
// 特性：
//   - 支持切换“备份盘”（drive_type="backup"，默认）与“资源库”（drive_type="resource"）；
//   - 刷新出新 refresh_token 时自动通过 driver.NotifyRefreshToken 加密回写 sources.json；
//   - 实现 driver.Presigner（最大 4 小时免鉴权直链，支持 302 跳转直连阿里云 CDN）；
//   - 支持流式 Range 下载、10MB 分片流式上传、建目录、改名/移动、删除（移入云盘回收站）。
package alipan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ufile/internal/driver"
	"ufile/internal/driver/httputil"
	"ufile/internal/driver/pathid"
)

func init() { driver.Register("alipan", New) }

const (
	defaultAPIBase  = "https://openapi.alipan.com"
	defaultPartSize = 10 << 20 // 10 MiB 分片上传块大小
)

// Driver 阿里云盘驱动实例。
type Driver struct {
	apiBase       string
	oauthTokenURL string
	clientID      string
	clientSecret  string
	refreshToken  string
	driveType     string // "backup" | "resource"
	rootFolderID  string

	client   *http.Client
	resolver *pathid.Resolver
	cfg      map[string]any

	tokMu       sync.Mutex
	accessToken string
	tokenExpiry time.Time

	driveMu sync.Mutex
	driveID string
}

// New 构造阿里云盘驱动。
func New(cfg map[string]any) (driver.FSDriver, error) {
	rtok, err := driver.RequiredStr(cfg, "refresh_token")
	if err != nil {
		return nil, fmt.Errorf("alipan: 缺少 refresh_token")
	}
	cid := strings.TrimSpace(driver.Str(cfg, "client_id", ""))
	csec := strings.TrimSpace(driver.Str(cfg, "client_secret", ""))
	tokenURL := strings.TrimSpace(driver.Str(cfg, "oauth_token_url", ""))
	if tokenURL == "" && (cid == "" || csec == "") {
		return nil, errors.New("alipan: 需配置 client_id + client_secret，或配置三方 oauth_token_url")
	}

	driveType := strings.ToLower(strings.TrimSpace(driver.Str(cfg, "drive_type", "backup")))
	if driveType == "" {
		driveType = "backup"
	}
	if driveType != "backup" && driveType != "resource" {
		return nil, fmt.Errorf("alipan: 不支持的 drive_type %q（可选 backup 或 resource）", driveType)
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
		apiBase:       strings.TrimRight(driver.Str(cfg, "api_base", defaultAPIBase), "/"),
		oauthTokenURL: tokenURL,
		clientID:      cid,
		clientSecret:  csec,
		refreshToken:  strings.TrimSpace(rtok),
		driveType:     driveType,
		rootFolderID:  rootID,
		driveID:       strings.TrimSpace(driver.Str(cfg, "drive_id", "")),
		client:        cli,
		cfg:           cfg,
	}
	d.resolver = pathid.NewResolver(d.rootFolderID, pathid.DefaultTTL, d.lookupChild)
	return d, nil
}

func (d *Driver) Scheme() string { return "alipan" }

func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{
		List:       true,
		Stat:       true,
		Download:   true,
		PresignURL: true,
		ListCursor: true,
		Write:      true,
	}
}

// getAccessToken 获取并自动刷新阿里云盘 access_token；若返回新 refresh_token 则通知持久化。
func (d *Driver) getAccessToken(ctx context.Context) (string, error) {
	d.tokMu.Lock()
	defer d.tokMu.Unlock()

	if d.accessToken != "" && time.Now().Add(30*time.Second).Before(d.tokenExpiry) {
		return d.accessToken, nil
	}

	targetURL := d.oauthTokenURL
	if targetURL == "" {
		targetURL = d.apiBase + "/oauth/access_token"
	}
	payload := map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": d.refreshToken,
	}
	if d.clientID != "" {
		payload["client_id"] = d.clientID
	}
	if d.clientSecret != "" {
		payload["client_secret"] = d.clientSecret
	}
	rawReq, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(rawReq))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("alipan: 刷新令牌请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("alipan: 刷新令牌失败 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokResp); err != nil || tokResp.AccessToken == "" {
		return "", fmt.Errorf("alipan: 解析令牌响应失败: %w", err)
	}
	if tokResp.ExpiresIn <= 0 {
		tokResp.ExpiresIn = 7200
	}
	d.accessToken = tokResp.AccessToken
	d.tokenExpiry = time.Now().Add(time.Duration(tokResp.ExpiresIn) * time.Second)

	if tokResp.RefreshToken != "" && tokResp.RefreshToken != d.refreshToken {
		d.refreshToken = tokResp.RefreshToken
		driver.NotifyRefreshToken(d.cfg, tokResp.RefreshToken)
	}
	return d.accessToken, nil
}

// postJSON 携带 Bearer Token 发送 JSON POST 请求并解码响应。
func (d *Driver) postJSON(ctx context.Context, apiPath string, reqPayload any, respOut any) (int, error) {
	tok, err := d.getAccessToken(ctx)
	if err != nil {
		return 0, err
	}
	var bodyReader io.Reader
	if reqPayload != nil {
		b, err := json.Marshal(reqPayload)
		if err != nil {
			return 0, err
		}
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = strings.NewReader("{}")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiBase+apiPath, bodyReader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("alipan: POST %s: %w", apiPath, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))

	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, driver.ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("alipan: %s HTTP %d: %s", apiPath, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if respOut != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, respOut); err != nil {
			return resp.StatusCode, fmt.Errorf("alipan: 解析 %s 响应失败: %w", apiPath, err)
		}
	}
	return resp.StatusCode, nil
}

// getDriveID 获取并缓存当前选择的 drive_id（backup_drive_id 或 resource_drive_id）。
func (d *Driver) getDriveID(ctx context.Context) (string, error) {
	d.driveMu.Lock()
	defer d.driveMu.Unlock()
	if d.driveID != "" {
		return d.driveID, nil
	}
	var info struct {
		DefaultDriveID  string `json:"default_drive_id"`
		BackupDriveID   string `json:"backup_drive_id"`
		ResourceDriveID string `json:"resource_drive_id"`
	}
	if _, err := d.postJSON(ctx, "/adrive/v1.0/user/getDriveInfo", nil, &info); err != nil {
		return "", err
	}
	switch d.driveType {
	case "resource":
		if info.ResourceDriveID != "" {
			d.driveID = info.ResourceDriveID
		} else {
			d.driveID = info.DefaultDriveID
		}
	default:
		if info.BackupDriveID != "" {
			d.driveID = info.BackupDriveID
		} else {
			d.driveID = info.DefaultDriveID
		}
	}
	if d.driveID == "" {
		return "", errors.New("alipan: 未能从 getDriveInfo 获取有效的 drive_id")
	}
	return d.driveID, nil
}

type aliItem struct {
	FileID      string `json:"file_id"`
	Name        string `json:"name"`
	Type        string `json:"type"` // "folder" | "file"
	Size        int64  `json:"size"`
	UpdatedAt   string `json:"updated_at"`
	ContentHash string `json:"content_hash"`
}

func (it aliItem) toNode() pathid.Node {
	isDir := it.Type == "folder"
	var mod time.Time
	if it.UpdatedAt != "" {
		mod, _ = time.Parse(time.RFC3339, it.UpdatedAt)
	}
	var sz int64
	if !isDir {
		sz = it.Size
	}
	return pathid.Node{
		ID:      it.FileID,
		Name:    it.Name,
		IsDir:   isDir,
		Size:    sz,
		ModTime: mod,
		ETag:    it.ContentHash,
	}
}

// lookupChild 在 parentID 下按名称查找单个直接子节点。
// 优先调 /adrive/v1.0/openFile/get_by_path 或列取匹配。这里通过在 parent_file_id 下分页匹配保证对所有根目录/子目录 ID 一致工作。
func (d *Driver) lookupChild(ctx context.Context, parentID, name string) (pathid.Node, error) {
	driveID, err := d.getDriveID(ctx)
	if err != nil {
		return pathid.Node{}, err
	}
	marker := ""
	for {
		var res struct {
			Items      []aliItem `json:"items"`
			NextMarker string    `json:"next_marker"`
		}
		_, err := d.postJSON(ctx, "/adrive/v1.0/openFile/list", map[string]any{
			"drive_id":       driveID,
			"parent_file_id": parentID,
			"limit":          200,
			"marker":         marker,
		}, &res)
		if err != nil {
			return pathid.Node{}, err
		}
		for _, it := range res.Items {
			if it.Name == name {
				return it.toNode(), nil
			}
		}
		if res.NextMarker == "" {
			break
		}
		marker = res.NextMarker
	}
	return pathid.Node{}, driver.ErrNotFound
}

// List 列取目录内容，支持游标分页，并将子项预热至路径缓存。
func (d *Driver) List(ctx context.Context, dir string, opt driver.ListOptions) (*driver.ListResult, error) {
	cleanDir, dirNode, err := d.resolver.Resolve(ctx, dir)
	if err != nil {
		return nil, err
	}
	if !dirNode.IsDir {
		return nil, driver.ErrNotFound
	}
	driveID, err := d.getDriveID(ctx)
	if err != nil {
		return nil, err
	}
	limit := opt.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	var res struct {
		Items      []aliItem `json:"items"`
		NextMarker string    `json:"next_marker"`
	}
	_, err = d.postJSON(ctx, "/adrive/v1.0/openFile/list", map[string]any{
		"drive_id":       driveID,
		"parent_file_id": dirNode.ID,
		"limit":          limit,
		"marker":         opt.Cursor,
		"order_by":       "name",
		"order_direction": "ASC",
	}, &res)
	if err != nil {
		return nil, err
	}

	nodes := make([]pathid.Node, 0, len(res.Items))
	entries := make([]driver.Entry, 0, len(res.Items))
	for _, it := range res.Items {
		n := it.toNode()
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
		NextCursor: res.NextMarker,
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

// PresignGet 生成带有效期的阿里云盘免鉴权下载直链（最大 14400 秒 = 4 小时）。
func (d *Driver) PresignGet(ctx context.Context, p string, ttl time.Duration) (string, error) {
	clean, node, err := d.resolver.Resolve(ctx, p)
	if err != nil {
		return "", err
	}
	if clean == "" || node.IsDir {
		return "", driver.ErrInvalidPath
	}
	driveID, err := d.getDriveID(ctx)
	if err != nil {
		return "", err
	}
	sec := int(ttl.Seconds())
	if sec <= 0 {
		sec = 900
	}
	if sec > 14400 {
		sec = 14400
	}

	var res struct {
		URL string `json:"url"`
	}
	_, err = d.postJSON(ctx, "/adrive/v1.0/openFile/getDownloadUrl", map[string]any{
		"drive_id":   driveID,
		"file_id":    node.ID,
		"expire_sec": sec,
	}, &res)
	if err != nil {
		return "", err
	}
	if res.URL == "" {
		return "", errors.New("alipan: getDownloadUrl 返回空 URL")
	}
	return res.URL, nil
}

// Open 打开文件下载流（支持 HTTP Range）。
func (d *Driver) Open(ctx context.Context, p string, opt driver.OpenOptions) (io.ReadCloser, error) {
	dlURL, err := d.PresignGet(ctx, p, 1*time.Hour)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, err
	}
	if rng := httputil.FormatRangeHeader(opt); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alipan: 下载文件流失败: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, driver.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("alipan: 下载返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
	driveID, err := d.getDriveID(ctx)
	if err != nil {
		return pathid.Node{}, err
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
				return pathid.Node{}, fmt.Errorf("alipan: 路径 %q 已存在同名文件", curPath)
			}
			curID = n.ID
			continue
		}
		if n, err := d.lookupChild(ctx, curID, seg); err == nil {
			if !n.IsDir {
				return pathid.Node{}, fmt.Errorf("alipan: 路径 %q 已存在同名文件", curPath)
			}
			d.resolver.Put(curPath, n)
			curID = n.ID
			continue
		}
		// 调用 openFile/create 创建文件夹（check_name_mode=refuse 若已存在会返回同名 folder 的 file_id）
		var created struct {
			FileID string `json:"file_id"`
		}
		_, err := d.postJSON(ctx, "/adrive/v1.0/openFile/create", map[string]any{
			"drive_id":        driveID,
			"parent_file_id":  curID,
			"name":            seg,
			"type":            "folder",
			"check_name_mode": "refuse",
		}, &created)
		if err != nil {
			return pathid.Node{}, err
		}
		n := pathid.Node{ID: created.FileID, Name: seg, IsDir: true, ModTime: time.Now()}
		d.resolver.Put(curPath, n)
		curID = n.ID
	}
	return pathid.Node{ID: curID, Name: segs[len(segs)-1], IsDir: true}, nil
}

// Mkdir 递归创建目录。
func (d *Driver) Mkdir(ctx context.Context, p string) error {
	_, err := d.ensureDir(ctx, p)
	return err
}

// Upload 按阿里云盘 OpenAPI 三阶段分片协议流式上传文件（create -> PUT part -> complete）。
func (d *Driver) Upload(ctx context.Context, p string, r io.Reader, size int64) error {
	parentClean, baseName, err := pathid.SplitParent(p)
	if err != nil {
		return err
	}
	parentNode, err := d.ensureDir(ctx, parentClean)
	if err != nil {
		return err
	}
	driveID, err := d.getDriveID(ctx)
	if err != nil {
		return err
	}
	cleanPath := baseName
	if parentClean != "" {
		cleanPath = parentClean + "/" + baseName
	}

	// 计算分片列表（默认每片 10MiB；若 size 未知或 <=0，先预设 100 个分片槽位，按实际读取流式上传）
	partCount := 1
	if size > 0 {
		partCount = int((size + defaultPartSize - 1) / defaultPartSize)
	} else if size < 0 {
		partCount = 100
	}
	type partInfo struct {
		PartNumber int    `json:"part_number"`
		UploadURL  string `json:"upload_url,omitempty"`
	}
	reqParts := make([]partInfo, partCount)
	for i := range reqParts {
		reqParts[i] = partInfo{PartNumber: i + 1}
	}

	createPayload := map[string]any{
		"drive_id":        driveID,
		"parent_file_id":  parentNode.ID,
		"name":            baseName,
		"type":            "file",
		"check_name_mode": "overwrite",
		"part_info_list":  reqParts,
	}
	if size >= 0 {
		createPayload["size"] = size
	}

	var createResp struct {
		FileID       string     `json:"file_id"`
		UploadID     string     `json:"upload_id"`
		PartInfoList []partInfo `json:"part_info_list"`
	}
	if _, err := d.postJSON(ctx, "/adrive/v1.0/openFile/create", createPayload, &createResp); err != nil {
		return err
	}

	if r == nil {
		r = bytes.NewReader(nil)
	}
	buf := make([]byte, defaultPartSize)
	reachedEOF := false
	for idx, part := range createResp.PartInfoList {
		if part.UploadURL == "" {
			continue
		}
		n, readErr := io.ReadFull(r, buf)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return fmt.Errorf("alipan: 读取上传流失败: %w", readErr)
		}
		if n == 0 && idx > 0 {
			reachedEOF = true
			break
		}
		putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, part.UploadURL, bytes.NewReader(buf[:n]))
		if err != nil {
			return err
		}
		putReq.ContentLength = int64(n)
		putResp, err := d.client.Do(putReq)
		if err != nil {
			return fmt.Errorf("alipan: 上传分片 #%d 失败: %w", part.PartNumber, err)
		}
		putBody, _ := io.ReadAll(io.LimitReader(putResp.Body, 4096))
		putResp.Body.Close()
		if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
			return fmt.Errorf("alipan: 上传分片 #%d HTTP %d: %s", part.PartNumber, putResp.StatusCode, strings.TrimSpace(string(putBody)))
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			reachedEOF = true
			break
		}
	}
	if !reachedEOF {
		var extra [1]byte
		if n, _ := r.Read(extra[:]); n > 0 {
			return errors.New("alipan: 流式上传超出预分配分片上限（请在调用 Upload 时传入文件大小 size）")
		}
	}

	var doneItem aliItem
	_, err = d.postJSON(ctx, "/adrive/v1.0/openFile/complete", map[string]any{
		"drive_id":  driveID,
		"file_id":   createResp.FileID,
		"upload_id": createResp.UploadID,
	}, &doneItem)
	if err != nil {
		return err
	}
	if doneItem.FileID != "" {
		d.resolver.Put(cleanPath, doneItem.toNode())
	} else {
		d.resolver.Invalidate(cleanPath)
	}
	return nil
}

// Delete 将文件或目录移入阿里云盘回收站（/adrive/v1.0/openFile/recyclebin/trash）。
func (d *Driver) Delete(ctx context.Context, p string) error {
	clean, node, err := d.resolver.Resolve(ctx, p)
	if err != nil {
		return err
	}
	if clean == "" {
		return driver.ErrInvalidPath
	}
	driveID, err := d.getDriveID(ctx)
	if err != nil {
		return err
	}
	_, err = d.postJSON(ctx, "/adrive/v1.0/openFile/recyclebin/trash", map[string]any{
		"drive_id": driveID,
		"file_id":  node.ID,
	}, nil)
	d.resolver.Invalidate(clean)
	return err
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
	oldParentClean, oldName, err := pathid.SplitParent(cleanFrom)
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
	driveID, err := d.getDriveID(ctx)
	if err != nil {
		return err
	}

	// 1. 跨目录时先执行 move
	if oldParentClean != newParentClean {
		newParentNode, err := d.ensureDir(ctx, newParentClean)
		if err != nil {
			return err
		}
		_, err = d.postJSON(ctx, "/adrive/v1.0/openFile/move", map[string]any{
			"drive_id":          driveID,
			"file_id":           srcNode.ID,
			"to_parent_file_id": newParentNode.ID,
			"check_name_mode":   "overwrite",
			"new_name":          newName,
		}, nil)
		if err != nil {
			return err
		}
	}
	// 2. 若文件名同时发生变更（或同目录改名），调用 update 确保新名称生效（兼容 move 仅在同名冲突时应用 new_name 的行为）
	if oldName != newName {
		_, err = d.postJSON(ctx, "/adrive/v1.0/openFile/update", map[string]any{
			"drive_id":        driveID,
			"file_id":         srcNode.ID,
			"name":            newName,
			"check_name_mode": "refuse",
		}, nil)
		if err != nil {
			return err
		}
	}

	d.resolver.Invalidate(cleanFrom)
	d.resolver.Invalidate(cleanTo)
	return nil
}
