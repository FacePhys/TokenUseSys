// server 是看板服务进程：后台同步 4router 的令牌和日志到本地 SQLite（开发文档 §4），并提供看板接口（§6）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/FacePhys/TokenUseSys/internal/application/dashboard"
	"github.com/FacePhys/TokenUseSys/internal/application/syncer"
	"github.com/FacePhys/TokenUseSys/internal/bootstrap"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/config"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/logging"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/sqlite"
	httpapi "github.com/FacePhys/TokenUseSys/internal/interfaces/http"
	"github.com/FacePhys/TokenUseSys/web"
)

// shutdownTimeout 是停机时等待进行中的请求的时间。
const shutdownTimeout = 10 * time.Second

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()
	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	// 启动检查（§9）：上游凭据、管理员密码哈希、会话密钥缺一不可。
	if err := cfg.ValidateServer(); err != nil {
		return err
	}
	log, closeLog, err := logging.Setup(cfg.Log, "app", os.Stdout)
	if err != nil {
		return err
	}
	defer closeLog()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 上次中断留下的半截备份（§12.6）。
	if err := sqlite.CleanupTempBackups(cfg.Storage.BackupDir); err != nil {
		log.Warn("清理残留备份失败", "error", err)
	}
	store, err := sqlite.Open(ctx, cfg.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer store.Close()

	gin.SetMode(gin.ReleaseMode)
	router, err := httpapi.NewRouter(httpapi.Options{
		Log:       log,
		Dashboard: dashboard.New(store.Dashboard(), cfg.Location(), cfg.TZOffset(), nil),
		Auth: httpapi.AuthOptions{User: cfg.Auth.AdminUser, PasswordHash: cfg.Auth.AdminPasswordHash,
			Secret: cfg.Auth.SessionSecret, TTL: cfg.Auth.SessionTTL},
		Web: web.FS,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Server.Listen, Handler: router, ReadHeaderTimeout: 10 * time.Second}

	alerter, err := bootstrap.NewAlerter(cfg, log)
	if err != nil {
		return err
	}
	syncDone := make(chan error, 1)
	go func() {
		s := bootstrap.NewSyncer(cfg, bootstrap.NewUpstream(cfg, log), store, log, syncer.WithAlerter(alerter))
		syncDone <- s.Run(ctx)
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Info("服务已启动", "listen", cfg.Server.Listen, "upstream", cfg.Upstream.BaseURL, "db", cfg.Storage.SQLitePath)

	// 任何一边先退出（端口被占用、同步器初始化失败等），另一边也一起停。
	var listenErr, syncErr error
	syncFinished := false
	select {
	case listenErr = <-serveErr:
	case syncErr = <-syncDone:
		syncFinished = true
	case <-ctx.Done():
	}
	stop()
	log.Info("正在停止服务")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if e := srv.Shutdown(shutdownCtx); e != nil {
		log.Warn("停止 HTTP 服务失败", "error", e)
	}
	// 等同步器退出：正在写的窗口事务回滚，游标不动（§4.6）。
	if !syncFinished {
		select {
		case syncErr = <-syncDone:
		case <-shutdownCtx.Done():
		}
	}
	if errors.Is(listenErr, http.ErrServerClosed) {
		listenErr = nil
	}
	return errors.Join(listenErr, syncErr)
}
