package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ufile/internal/api"
	"ufile/internal/audit"
	"ufile/internal/auth"
	"ufile/internal/config"
	"ufile/internal/crypt"
	"ufile/internal/driver"
	"ufile/internal/index"
	"ufile/internal/share"
	"ufile/internal/source"
	"ufile/web"

	// 统一注册所有内建驱动（init 时写入 driver 注册表）
	_ "ufile/internal/driver/all"
)

// loadOrCreateJWTKey 读取（或生成）data/jwt.key：JWT 签名密钥，
// 独立于凭据主密钥（二者泄露影响面不同，分开轮换）。
func loadOrCreateJWTKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, "jwt.key")
	if raw, err := os.ReadFile(path); err == nil {
		key, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(raw)))
		if err != nil || len(key) < 32 {
			return nil, fmt.Errorf("jwt.key 内容非法")
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	addr := flag.String("addr", "", "监听地址（覆盖配置文件 server.port），如 :8080")
	dataDir := flag.String("data", "./data", "数据目录（源定义/主密钥/用户/索引/审计）")
	webDir := flag.String("web", "", "Web 静态资源目录（默认用内嵌的 web/dist；前端开发时指向 Vite 产物目录）")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 源管理器：data/sources.json 为准；config.yaml 仅首次播种。
	// 后续源的新增/删除走 Web 入口与 API，不再需要改配置重启。
	// 主密钥用于凭据加密落库（data/master.key，首次自动生成）。
	key, created, err := crypt.LoadOrCreateKey(*dataDir)
	if err != nil {
		log.Fatalf("初始化主密钥失败: %v", err)
	}
	cipher, err := crypt.New(key)
	if err != nil {
		log.Fatalf("初始化加密失败: %v", err)
	}
	if created {
		log.Printf("已生成主密钥 data/master.key —— 请务必备份该文件，丢失后已保存的凭据将无法解密")
	}
	mgr, err := source.NewManager((*dataDir), cfg.Seeds(), cipher)
	if err != nil {
		log.Fatalf("初始化文件源失败: %v", err)
	}

	// 鉴权（M1 安全基线）：JWT 签名密钥独立于凭据主密钥，同样落 data/。
	jwtSecret, err := loadOrCreateJWTKey((*dataDir))
	if err != nil {
		log.Fatalf("初始化 JWT 密钥失败: %v", err)
	}
	authStore, err := auth.NewStore((*dataDir), jwtSecret)
	if err != nil {
		log.Fatalf("初始化用户存储失败: %v", err)
	}
	auditLog, err := audit.Open((*dataDir), func(err error) {
		log.Printf("[warn] 审计日志写入失败: %v", err)
	})
	if err != nil {
		log.Fatalf("初始化审计日志失败: %v", err)
	}

	// 索引缓存（M1）：目录 TTL 缓存 + 后台定时全量同步 + 快照持久化。
	idxCache := index.New((*dataDir), index.Config{
		TTL:        10 * time.Minute,
		SyncEvery:  time.Hour,
		Snapshot:   true,
		MaxEntries: 10000,
	}, mgr.IDs, func(id string) (driver.FSDriver, bool) {
		src, err := mgr.Get(id)
		if err != nil {
			return nil, false
		}
		return src.Driver(), true
	})
	// 源删除时联动清理索引（复用 Manager 的删除钩子）
	mgr.OnRemove = idxCache.Forget

	// 分享外链（M3）：data/shares.json；grant cookie 签名复用 jwt.key（独立主密钥会多一个要备份的文件）
	shareMgr, err := share.NewManager((*dataDir), jwtSecret)
	if err != nil {
		log.Fatalf("初始化分享存储失败: %v", err)
	}

	// 前端静态资源：默认内嵌 web/dist；-web 参数可指向磁盘目录（前端开发调试）
	var webFS fs.FS
	if *webDir != "" {
		webFS = os.DirFS(*webDir)
		log.Printf("前端资源使用磁盘目录: %s", *webDir)
	} else {
		webFS, err = web.FS()
		if err != nil {
			log.Fatalf("加载内嵌前端资源失败: %v", err)
		}
	}

	srv, err := api.New(mgr, authStore, auditLog, idxCache, shareMgr, webFS)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	address := *addr
	if address == "" {
		address = fmt.Sprintf(":%d", cfg.Server.Port)
	}
	httpSrv := &http.Server{Addr: address, Handler: srv.Handler()}

	// 后台索引同步随服务启停
	ctxSync, stopSync := context.WithCancel(context.Background())
	defer stopSync()
	idxCache.Start(ctxSync)

	go func() {
		log.Printf("u-file M1 已启动: http://localhost%s", address)
		log.Printf("已接入 %d 个文件源；鉴权已启用（首次访问 Web 会引导创建管理员）", len(cfg.Sources))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("正在关闭……")
	stopSync()
	idxCache.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
