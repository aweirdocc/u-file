// Package s3 用 aws-sdk-go-v2 统一接入 S3 协议族：
// AWS S3、MinIO、Cloudflare R2、阿里云 OSS、腾讯云 COS、华为云 OBS、七牛 Kodo、百度 BOS。
// 各厂商差异通过每源配置收敛：endpoint / region / use_path_style / prefix。
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"ufile/internal/driver"
)

func init() { driver.Register("s3", New) }

// maxPresignTTL 对齐最严格的 S3 实现（Cloudflare R2 官方：预签名最长 7 天）。
const maxPresignTTL = 7 * 24 * time.Hour

type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
	prefix  string // 源根前缀，非空时以 / 结尾
	credent bool   // 是否配置了静态凭据（决定能否生成预签名直链）
}

func New(cfg map[string]any) (driver.FSDriver, error) {
	bucket, err := driver.RequiredStr(cfg, "bucket")
	if err != nil {
		return nil, err
	}
	endpoint := driver.Str(cfg, "endpoint", "")
	region := driver.Str(cfg, "region", "us-east-1")
	ak := driver.Str(cfg, "access_key", "")
	sk := driver.Str(cfg, "secret_key", "")
	prefix := strings.Trim(driver.Str(cfg, "prefix", ""), "/")
	if prefix != "" {
		prefix += "/"
	}

	awsCfg := aws.Config{Region: region}
	credent := false
	if ak != "" || sk != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(ak, sk, "")
		credent = true
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = driver.Bool(cfg, "use_path_style", false)
	})
	return &S3{
		client:  client,
		presign: s3.NewPresignClient(client),
		bucket:  bucket,
		prefix:  prefix,
		credent: credent,
	}, nil
}

func (d *S3) Scheme() string { return "s3" }

func (d *S3) Capabilities() driver.Capabilities {
	// 预签名直链依赖有效凭据：无凭据（匿名只读场景）时不声明该能力，
	// 否则 PresignGetObject 会因 noAuth signer 直接报错。
	// 写操作同样需要凭据。
	write := d.credent
	return driver.Capabilities{List: true, Stat: true, Download: true, PresignURL: d.credent, ListCursor: true, Write: write}
}

func (d *S3) List(ctx context.Context, dir string, opt driver.ListOptions) (*driver.ListResult, error) {
	rel, err := driver.CleanPath(dir)
	if err != nil {
		return nil, err
	}
	prefix := d.prefix + rel
	if prefix != "" {
		prefix += "/"
	}

	limit := opt.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	in := &s3.ListObjectsV2Input{
		Bucket:    aws.String(d.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int32(int32(limit)),
	}
	if opt.Cursor != "" {
		in.ContinuationToken = aws.String(opt.Cursor)
	}
	out, err := d.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("s3: ListObjectsV2: %w", err)
	}

	res := &driver.ListResult{Entries: []driver.Entry{}}
	for _, cp := range out.CommonPrefixes {
		name := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(cp.Prefix), prefix), "/")
		if name == "" {
			continue
		}
		res.Entries = append(res.Entries, driver.Entry{
			Path:  joinRel(rel, name),
			Name:  name,
			IsDir: true,
		})
	}
	for _, o := range out.Contents {
		name := strings.TrimPrefix(aws.ToString(o.Key), prefix)
		if name == "" {
			continue
		}
		e := driver.Entry{
			Path:  joinRel(rel, name),
			Name:  name,
			Size:  derefI64(o.Size),
			ETag:  strings.Trim(aws.ToString(o.ETag), `"`),
			IsDir: strings.HasSuffix(aws.ToString(o.Key), "/"), // 兼容“目录占位对象”
		}
		if o.LastModified != nil {
			e.ModTime = *o.LastModified
		}
		res.Entries = append(res.Entries, e)
	}
	if aws.ToBool(out.IsTruncated) {
		res.NextCursor = aws.ToString(out.NextContinuationToken)
	}
	return res, nil
}

func (d *S3) Stat(ctx context.Context, path string) (*driver.Entry, error) {
	rel, err := driver.CleanPath(path)
	if err != nil {
		return nil, err
	}
	if rel == "" { // 源根目录
		return &driver.Entry{IsDir: true}, nil
	}
	key := d.prefix + rel

	out, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		// 注意：HeadObject 的 Key 是规范化的（CleanPath 已去除尾部 /），
		// 能命中说明是普通对象；“目录占位对象”由 List 的 CommonPrefixes 分组处理。
		e := &driver.Entry{
			Path: rel,
			Name: baseName(rel),
			Size: derefI64(out.ContentLength),
			ETag: strings.Trim(aws.ToString(out.ETag), `"`),
		}
		if out.LastModified != nil {
			e.ModTime = *out.LastModified
		}
		return e, nil
	}
	if !isNotFound(err) {
		return nil, fmt.Errorf("s3: HeadObject: %w", err)
	}

	// 不是对象 → 按目录前缀探测（该前缀下存在任意对象/子前缀即视为目录）
	lo, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(d.bucket),
		Prefix:  aws.String(key + "/"),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("s3: ListObjectsV2: %w", err)
	}
	if len(lo.Contents) > 0 || len(lo.CommonPrefixes) > 0 {
		return &driver.Entry{Path: rel, Name: baseName(rel), IsDir: true}, nil
	}
	return nil, driver.ErrNotFound
}

func (d *S3) Open(ctx context.Context, path string, opt driver.OpenOptions) (io.ReadCloser, error) {
	rel, err := driver.CleanPath(path)
	if err != nil {
		return nil, err
	}
	if rel == "" {
		return nil, driver.ErrInvalidPath
	}
	in := &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.prefix + rel),
	}
	switch {
	case opt.Offset > 0 && opt.Length > 0:
		in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", opt.Offset, opt.Offset+opt.Length-1))
	case opt.Offset > 0:
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", opt.Offset))
	}
	out, err := d.client.GetObject(ctx, in)
	if err != nil {
		if isNotFound(err) {
			return nil, driver.ErrNotFound
		}
		return nil, fmt.Errorf("s3: GetObject: %w", err)
	}
	return out.Body, nil
}

func (d *S3) PresignGet(ctx context.Context, path string, ttl time.Duration) (string, error) {
	rel, err := driver.CleanPath(path)
	if err != nil {
		return "", err
	}
	if rel == "" {
		return "", driver.ErrInvalidPath
	}
	if !d.credent {
		return "", fmt.Errorf("s3: 未配置 access_key/secret_key，无法生成预签名直链")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if ttl > maxPresignTTL {
		ttl = maxPresignTTL
	}
	req, err := d.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.prefix + rel),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("s3: PresignGetObject: %w", err)
	}
	return req.URL, nil
}

// —— 写能力（Writer；需凭据）——

// Upload 上传对象（PutObject 流式；size 已知时声明 Content-Length）。
func (d *S3) Upload(ctx context.Context, path string, r io.Reader, size int64) error {
	rel, err := driver.CleanPath(path)
	if err != nil {
		return err
	}
	if rel == "" || strings.HasSuffix(path, "/") {
		return driver.ErrInvalidPath
	}
	if !d.credent {
		return errors.New("s3: 未配置凭据，不支持写操作")
	}
	in := &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.prefix + rel),
		Body:   r,
	}
	if size > 0 {
		in.ContentLength = aws.Int64(size)
	}
	if _, err := d.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("s3: PutObject: %w", err)
	}
	return nil
}

// Delete 删除对象；目录语义为删除该前缀下全部对象（分批，每批至多 1000）。
func (d *S3) Delete(ctx context.Context, path string) error {
	rel, err := driver.CleanPath(path)
	if err != nil {
		return err
	}
	if rel == "" {
		return driver.ErrInvalidPath // 源根不可删
	}
	if !d.credent {
		return errors.New("s3: 未配置凭据，不支持写操作")
	}
	key := d.prefix + rel

	// 先按对象删（普通对象 + 目录占位对象）
	if _, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket), Key: aws.String(key),
	}); err != nil {
		return fmt.Errorf("s3: DeleteObject: %w", err)
	}
	// 目录：批量删除前缀下全部对象
	var after string
	for {
		in := &s3.ListObjectsV2Input{
			Bucket: aws.String(d.bucket), Prefix: aws.String(key + "/"), MaxKeys: aws.Int32(1000),
		}
		if after != "" {
			in.ContinuationToken = aws.String(after)
		}
		out, err := d.client.ListObjectsV2(ctx, in)
		if err != nil {
			return fmt.Errorf("s3: ListObjectsV2: %w", err)
		}
		if len(out.Contents) > 0 {
			ids := make([]types.ObjectIdentifier, 0, len(out.Contents))
			for _, o := range out.Contents {
				ids = append(ids, types.ObjectIdentifier{Key: o.Key})
			}
			if _, err := d.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(d.bucket),
				Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
			}); err != nil {
				return fmt.Errorf("s3: DeleteObjects: %w", err)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		after = aws.ToString(out.NextContinuationToken)
	}
	return nil
}

// Mkdir 创建目录占位对象（零字节 key/），父前缀一并补齐。
func (d *S3) Mkdir(ctx context.Context, path string) error {
	rel, err := driver.CleanPath(path)
	if err != nil {
		return err
	}
	if rel == "" {
		return driver.ErrInvalidPath
	}
	if !d.credent {
		return errors.New("s3: 未配置凭据，不支持写操作")
	}
	if _, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(d.prefix + rel + "/"),
		Body:   bytes.NewReader(nil),
	}); err != nil {
		return fmt.Errorf("s3: PutObject(placeholder): %w", err)
	}
	return nil
}

// Rename 重命名/移动：先 CopyObject 到新键，再删旧键；目录则逐对象拷贝。
func (d *S3) Rename(ctx context.Context, from, to string) error {
	srcRel, err := driver.CleanPath(from)
	if err != nil {
		return err
	}
	dstRel, err := driver.CleanPath(to)
	if err != nil {
		return err
	}
	if srcRel == "" || dstRel == "" || srcRel == dstRel {
		return driver.ErrInvalidPath
	}
	if !d.credent {
		return errors.New("s3: 未配置凭据，不支持写操作")
	}
	if strings.HasPrefix(dstRel, srcRel+"/") {
		return driver.ErrInvalidPath // 不能移进自己
	}
	srcKey := d.prefix + srcRel
	dstKey := d.prefix + dstRel

	// 尝试按对象重命名
	if _, err := d.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(d.bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(url.PathEscape(d.bucket + "/" + srcKey)),
	}); err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("s3: CopyObject: %w", err)
		}
		// 不是对象 → 按目录：枚举前缀逐个拷贝再删
		return d.renamePrefix(ctx, srcKey+"/", dstKey+"/")
	}
	if _, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket), Key: aws.String(srcKey),
	}); err != nil {
		return fmt.Errorf("s3: DeleteObject: %w", err)
	}
	return nil
}

// Copy 同一 S3 源（同 Bucket）内服务端直接拷贝对象或目录前缀（实现 driver.Copier）。
func (d *S3) Copy(ctx context.Context, from, to string) error {
	srcRel, err := driver.CleanPath(from)
	if err != nil {
		return err
	}
	dstRel, err := driver.CleanPath(to)
	if err != nil {
		return err
	}
	if srcRel == "" || dstRel == "" || srcRel == dstRel {
		return driver.ErrInvalidPath
	}
	if !d.credent {
		return errors.New("s3: 未配置凭据，不支持写操作")
	}
	if strings.HasPrefix(dstRel, srcRel+"/") {
		return driver.ErrInvalidPath
	}
	srcKey := d.prefix + srcRel
	dstKey := d.prefix + dstRel

	if _, err := d.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(d.bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(url.PathEscape(d.bucket + "/" + srcKey)),
	}); err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("s3: CopyObject: %w", err)
		}
		return d.copyPrefix(ctx, srcKey+"/", dstKey+"/")
	}
	return nil
}

func (d *S3) copyPrefix(ctx context.Context, srcPrefix, dstPrefix string) error {
	var after string
	copiedAny := false
	for {
		in := &s3.ListObjectsV2Input{
			Bucket: aws.String(d.bucket), Prefix: aws.String(srcPrefix), MaxKeys: aws.Int32(1000),
		}
		if after != "" {
			in.ContinuationToken = aws.String(after)
		}
		out, err := d.client.ListObjectsV2(ctx, in)
		if err != nil {
			return fmt.Errorf("s3: ListObjectsV2: %w", err)
		}
		for _, o := range out.Contents {
			key := aws.ToString(o.Key)
			newKey := dstPrefix + strings.TrimPrefix(key, srcPrefix)
			if _, err := d.client.CopyObject(ctx, &s3.CopyObjectInput{
				Bucket:     aws.String(d.bucket),
				Key:        aws.String(newKey),
				CopySource: aws.String(url.PathEscape(d.bucket + "/" + key)),
			}); err != nil {
				return fmt.Errorf("s3: CopyObject: %w", err)
			}
			copiedAny = true
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		after = aws.ToString(out.NextContinuationToken)
	}
	if !copiedAny {
		return driver.ErrNotFound
	}
	return nil
}

func (d *S3) renamePrefix(ctx context.Context, srcPrefix, dstPrefix string) error {
	var after string
	for {
		in := &s3.ListObjectsV2Input{
			Bucket: aws.String(d.bucket), Prefix: aws.String(srcPrefix), MaxKeys: aws.Int32(1000),
		}
		if after != "" {
			in.ContinuationToken = aws.String(after)
		}
		out, err := d.client.ListObjectsV2(ctx, in)
		if err != nil {
			return fmt.Errorf("s3: ListObjectsV2: %w", err)
		}
		for _, o := range out.Contents {
			key := aws.ToString(o.Key)
			newKey := dstPrefix + strings.TrimPrefix(key, srcPrefix)
			if _, err := d.client.CopyObject(ctx, &s3.CopyObjectInput{
				Bucket:     aws.String(d.bucket),
				Key:        aws.String(newKey),
				CopySource: aws.String(url.PathEscape(d.bucket + "/" + key)),
			}); err != nil {
				return fmt.Errorf("s3: CopyObject: %w", err)
			}
			if _, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(d.bucket), Key: aws.String(key),
			}); err != nil {
				return fmt.Errorf("s3: DeleteObject: %w", err)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		after = aws.ToString(out.NextContinuationToken)
	}
	return nil
}

// isNotFound 兼容各家 S3 实现对“不存在”的不同表达：
// 标准 APIError 码优先，字符串兜底（部分兼容层只回 404 状态码）。
func isNotFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchBucket":
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "StatusCode: 404") ||
		strings.Contains(msg, "NotFound") ||
		strings.Contains(msg, "NoSuchKey")
}

func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
