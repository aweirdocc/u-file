package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ufile/internal/api"
	"ufile/internal/config"
	"ufile/internal/source"

	// 统一注册所有内建驱动（init 时写入 driver 注册表）
	_ "ufile/internal/driver/all"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	addr := flag.String("addr", "", "监听地址（覆盖配置文件 server.port），如 :8080")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 源管理器：data/sources.json 为准；config.yaml 仅首次播种。
	// 后续源的新增/删除走 Web 入口与 API，不再需要改配置重启。
	mgr, err := source.NewManager("./data", cfg.Seeds())
	if err != nil {
		log.Fatalf("初始化文件源失败: %v", err)
	}
	srv, err := api.New(mgr)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	address := *addr
	if address == "" {
		address = fmt.Sprintf(":%d", cfg.Server.Port)
	}
	httpSrv := &http.Server{Addr: address, Handler: srv.Handler()}

	go func() {
		log.Printf("u-file M0 PoC 已启动: http://localhost%s", address)
		log.Printf("已接入 %d 个文件源", len(cfg.Sources))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("正在关闭……")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
