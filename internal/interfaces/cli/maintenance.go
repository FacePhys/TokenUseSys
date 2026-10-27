package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/bootstrap"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/alert"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/config"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/logging"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/sqlite"
)

// resync 按 1 小时窗口重拉某一天并重算汇总。可以和服务同时运行：拉完一个窗口才开一个短写事务，
// 两个进程的写事务靠 busy_timeout 排队（§4.6）。
func (e *env) resync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resync", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	dayFlag := fs.String("day", "", "要重拉的日期，例如 2026-10-26（只能是昨天及以前）")
	force := fs.Bool("force", false, "上游返回 0 条时也替换（确认上游已清理旧日志、要清空本地这一天时才用）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dayFlag == "" {
		return errors.New("缺少 -day")
	}
	cfg, err := e.loadUpstreamConfig()
	if err != nil {
		return err
	}
	day, err := time.ParseInLocation(usage.DayLayout, *dayFlag, cfg.Location())
	if err != nil {
		return fmt.Errorf("-day 格式应为 2026-10-26：%w", err)
	}
	store, err := sqlite.Open(ctx, cfg.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer store.Close()
	log := logging.New(cfg.Log, e.stderr)
	s := bootstrap.NewSyncer(cfg, bootstrap.NewUpstream(cfg, log), store, log)
	fmt.Fprintf(e.stderr, "重拉 %s：24 个小时窗口，请求间隔 %s\n", *dayFlag, cfg.Upstream.MinRequestInterval)
	res, err := s.Resync(ctx, day, *force)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "已重拉 %s：写入 %d 条\n", *dayFlag, res.Rows)
	if res.Skipped > 0 {
		fmt.Fprintf(e.stdout, "%d 个小时窗口上游返回 0 条、本地有数据，没有替换；确认上游已清理旧日志后，要清空本地才加 -force\n", res.Skipped)
	}
	return nil
}

// backup 立即备份一次（§12.6），规则和服务每天的备份相同。
func (e *env) backup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(e.configPath)
	if err != nil {
		return err
	}
	store, err := sqlite.Open(ctx, cfg.Storage.SQLitePath)
	if err != nil {
		return err
	}
	defer store.Close()
	path, err := store.Backup(ctx, cfg.Storage.BackupDir, time.Now().In(cfg.Location()), cfg.Storage.BackupKeepDays)
	if err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, "已备份到", path)
	return nil
}

func (e *env) loadUpstreamConfig() (*config.Config, error) {
	cfg, err := config.Load(e.configPath)
	if err != nil {
		return nil, err
	}
	return cfg, cfg.ValidateUpstream()
}

// alertTest 往 alert.webhook 发一条测试消息，检查机器人地址和关键词设置（§4.7）。
func (e *env) alertTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("alert-test", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(e.configPath)
	if err != nil {
		return err
	}
	if cfg.Alert.Webhook == "" {
		return errors.New("alert.webhook 没有配置")
	}
	w, err := alert.NewWebhook(cfg.Alert.Webhook, logging.New(cfg.Log, e.stderr))
	if err != nil {
		return err
	}
	if err := w.Send(ctx, "测试消息：告警配置正常，同步出问题时会发到这个群。"); err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, "已发送，到群里看一下有没有收到")
	return nil
}
