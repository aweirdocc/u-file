// fakes3 是 M0 冒烟用的本地 S3 兼容测试服务器（gofakes3 封装）。
// 场景：开发环境拉不到 MinIO 镜像时，用它验证 u-file 的 S3 驱动
// （签名请求 / path-style / ListObjectsV2 / GetObject Range / Presign）。
//
// 用法：go run ./cmd/fakes3 -addr 127.0.0.1:19000 -bucket demo
//
// 说明：gofakes3 不校验签名（社区定位即“fake”），因此签名计算正确性
// 由真实云（OSS/R2 等）联调覆盖；本服务器用于验证请求路由、
// path-style、对象读写与 Range 语义是否被驱动正确使用。
// 禁用 versioning 路由以避免 SDK 探测桶版本状态时拿到 NotImplemented。
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:19000", "监听地址")
	bucket := flag.String("bucket", "demo", "启动时创建的初始桶")
	flag.Parse()

	backend := s3mem.New()
	faker := gofakes3.New(backend,
		gofakes3.WithAutoBucket(true),
		gofakes3.WithoutVersioning(),
		gofakes3.WithLogger(gofakes3.DiscardLog()),
	)
	if *bucket != "" {
		if err := backend.CreateBucket(*bucket); err != nil && !gofakes3.IsAlreadyExists(err) {
			log.Fatalf("创建初始桶失败: %v", err)
		}
		log.Printf("已创建桶: %s", *bucket)
	}
	log.Printf("fakes3 监听 http://%s （无鉴权，仅限本地联调）", *addr)
	log.Fatal(http.ListenAndServe(*addr, faker.Server()))
}
