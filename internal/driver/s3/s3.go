// Package s3 用 aws-sdk-go-v2 统一接入 S3 协议族：
// AWS S3、MinIO、Cloudflare R2、阿里云 OSS、腾讯云 COS、华为云 OBS、七牛 Kodo、百度 BOS。
// 各厂商差异通过每源配置收敛：endpoint / region / use_path_style / prefix。
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	return driver.Capabilities{List: true, Stat: true, Download: true, PresignURL: d.credent, ListCursor: true}
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
