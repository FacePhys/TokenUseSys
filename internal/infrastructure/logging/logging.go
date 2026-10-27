// Package logging 按配置创建结构化日志（复制自 rag）。日志里不记录 access token、请求头和完整 sk- key。
package logging

import (
	"context"
	"io"
	"log/slog"

	"github.com/FacePhys/TokenUseSys/internal/infrastructure/config"
)

// Setup 按配置创建进程的日志。配置了 log.dir 时按天写入 dir/<name>-<日期>.log 并只保留最近 retention_days 天，
// 不再写到 fallback（容器日志只按大小轮转，做不到按天删除）；没有配置时写到 fallback，用于本地开发。
// 返回的函数在进程退出前关闭日志文件。
func Setup(cfg config.LogConfig, name string, fallback io.Writer) (*slog.Logger, func(), error) {
	if cfg.Dir == "" {
		return New(cfg, fallback), func() {}, nil
	}
	f, err := OpenDaily(cfg.Dir, name, cfg.RetentionDays)
	if err != nil {
		return nil, nil, err
	}
	return New(cfg, f), func() { _ = f.Close() }, nil
}

// New 创建写到 w 的日志；配置已经在加载时校验过。
func New(cfg config.LogConfig, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// AtLeast 返回只输出 level 及以上级别的日志，用于屏蔽第三方库的例行日志。
func AtLeast(log *slog.Logger, level slog.Level) *slog.Logger {
	return slog.New(minLevelHandler{Handler: log.Handler(), level: level})
}

type minLevelHandler struct {
	slog.Handler
	level slog.Level
}

func (h minLevelHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.level && h.Handler.Enabled(ctx, l)
}

func (h minLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevelHandler{Handler: h.Handler.WithAttrs(attrs), level: h.level}
}

func (h minLevelHandler) WithGroup(name string) slog.Handler {
	return minLevelHandler{Handler: h.Handler.WithGroup(name), level: h.level}
}
