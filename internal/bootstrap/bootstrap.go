// Package bootstrap 按配置组装上游客户端、本地存储和同步器，供 server 和 relayctl 共用。
package bootstrap

import (
	"log/slog"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/application/syncer"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/alert"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/config"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/newapi"
)

// NewUpstream 创建 4router 只读客户端。
func NewUpstream(cfg *config.Config, log *slog.Logger) *newapi.Client {
	up := cfg.Upstream
	return newapi.New(newapi.Options{
		BaseURL: up.BaseURL, AccessToken: up.AccessToken, UserID: up.UserID,
		Timeout: up.Timeout, MinInterval: up.MinRequestInterval, Log: log,
	})
}

// NewSyncer 创建同步器。
func NewSyncer(cfg *config.Config, up port.Upstream, store port.Store, log *slog.Logger, opts ...syncer.Option) *syncer.Syncer {
	return syncer.New(up, store, SyncerConfig(cfg), log, opts...)
}

// NewAlerter 按配置创建告警（§4.7）：没配 alert.webhook 时不发。
func NewAlerter(cfg *config.Config, log *slog.Logger) (port.Alerter, error) {
	if cfg.Alert.Webhook == "" {
		return alert.Noop{}, nil
	}
	return alert.NewWebhook(cfg.Alert.Webhook, log)
}

// SyncerConfig 把配置转换成同步器参数。
func SyncerConfig(cfg *config.Config) syncer.Config {
	// reconcile_at 已在加载配置时校验过格式。
	at, _ := time.Parse("15:04", cfg.Sync.ReconcileAt)
	return syncer.Config{
		LogInterval:           cfg.Sync.LogInterval,
		SnapshotInterval:      cfg.Sync.SnapshotInterval,
		Lag:                   cfg.Sync.Lag,
		BackfillDays:          cfg.Sync.BackfillDays,
		ReconcileAt:           time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute,
		LogRetentionDays:      cfg.Storage.LogRetentionDays,
		SnapshotRetentionDays: cfg.Storage.SnapshotRetentionDays,
		BackupDir:             cfg.Storage.BackupDir,
		BackupKeepDays:        cfg.Storage.BackupKeepDays,
		Location:              cfg.Location(),
	}
}
