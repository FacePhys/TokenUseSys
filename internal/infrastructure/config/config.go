// Package config 从 YAML 文件加载配置（开发文档 §9），未知字段和不合法的值在启动时拒绝。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	// 运行镜像（scratch）里没有系统时区数据库，把时区数据编进二进制，server.timezone 才能解析。
	_ "time/tzdata"

	"gopkg.in/yaml.v3"
)

// minSessionSecretBytes 是会话签名密钥的最小长度（开发文档 §9）。
const minSessionSecretBytes = 32

type Config struct {
	Upstream UpstreamConfig `yaml:"upstream"`
	Sync     SyncConfig     `yaml:"sync"`
	Storage  StorageConfig  `yaml:"storage"`
	Server   ServerConfig   `yaml:"server"`
	Auth     AuthConfig     `yaml:"auth"`
	Alert    AlertConfig    `yaml:"alert"`
	Log      LogConfig      `yaml:"log"`

	// loc 是 Server.Timezone 解析后的时区，Validate 时设置。
	loc *time.Location
}

// UpstreamConfig 是 4router（New API）的接入信息。access token 拥有账号全部权限，只能写在这里，不能写日志。
type UpstreamConfig struct {
	BaseURL     string        `yaml:"base_url"`
	AccessToken string        `yaml:"access_token"`
	UserID      int           `yaml:"user_id"`
	Timeout     time.Duration `yaml:"timeout"`
	// MinRequestInterval 是两次上游请求的最小间隔，所有请求全局串行（开发文档 §3）。
	MinRequestInterval time.Duration `yaml:"min_request_interval"`
}

type SyncConfig struct {
	LogInterval      time.Duration `yaml:"log_interval"`
	SnapshotInterval time.Duration `yaml:"snapshot_interval"`
	// Lag 是日志同步只取多久以前的数据，保证窗口已封闭（开发文档 §4.3）。
	Lag time.Duration `yaml:"lag"`
	// BackfillDays 是首次启动回填的天数，决定页面最早能看到哪天。
	BackfillDays int    `yaml:"backfill_days"`
	ReconcileAt  string `yaml:"reconcile_at"`
}

type StorageConfig struct {
	SQLitePath            string `yaml:"sqlite_path"`
	LogRetentionDays      int    `yaml:"log_retention_days"`
	SnapshotRetentionDays int    `yaml:"snapshot_retention_days"`
	BackupDir             string `yaml:"backup_dir"`
	BackupKeepDays        int    `yaml:"backup_keep_days"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
	// Timezone 决定按天统计的分界，只支持没有夏令时的时区（开发文档 §3）。
	Timezone string `yaml:"timezone"`
}

type AuthConfig struct {
	AdminUser string `yaml:"admin_user"`
	// AdminPasswordHash 是 bcrypt 哈希，用 relayctl hash-password 生成。
	AdminPasswordHash string        `yaml:"admin_password_hash"`
	SessionSecret     string        `yaml:"session_secret"`
	SessionTTL        time.Duration `yaml:"session_ttl"`
}

// AlertConfig 是同步失败时的通知（开发文档 §4.7），webhook 为空时不发。
type AlertConfig struct {
	Webhook string `yaml:"webhook"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// Dir 非空时按天写入这个目录，不再写到标准输出。
	Dir string `yaml:"dir"`
	// RetentionDays 是按天写入的日志保留的天数（含当天）。
	RetentionDays int `yaml:"retention_days"`
}

// Load 读取并解析配置文件。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件: %w", err)
	}
	return Parse(data)
}

// Parse 解析 YAML 内容，补齐默认值后校验。
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Default 返回默认配置，数值与 config.example.yaml 一致。
func Default() *Config {
	return &Config{
		Upstream: UpstreamConfig{
			BaseURL:            "https://4router.net",
			Timeout:            15 * time.Second,
			MinRequestInterval: 2 * time.Second,
		},
		Sync: SyncConfig{
			LogInterval:      time.Minute,
			SnapshotInterval: 5 * time.Minute,
			Lag:              2 * time.Minute,
			BackfillDays:     7,
			ReconcileAt:      "03:30",
		},
		Storage: StorageConfig{
			SQLitePath:            "/var/lib/relayusage/usage.db",
			LogRetentionDays:      90,
			SnapshotRetentionDays: 90,
			BackupDir:             "/var/lib/relayusage/backups",
			BackupKeepDays:        14,
		},
		Server: ServerConfig{
			Listen:   "127.0.0.1:8200",
			Timezone: "Asia/Shanghai",
		},
		Auth: AuthConfig{
			AdminUser:  "admin",
			SessionTTL: 12 * time.Hour,
		},
		Log: LogConfig{Level: "info", Format: "json", RetentionDays: 7},
	}
}

// Validate 检查所有命令都需要的配置；密钥是否填写由 ValidateUpstream 和 ValidateServer 检查。
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	u := &c.Upstream
	u.BaseURL = strings.TrimRight(u.BaseURL, "/")
	if parsed, err := url.Parse(u.BaseURL); err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.Path != "" {
		add("upstream.base_url 必须是 http(s)://域名，不带路径")
	}
	if u.UserID < 0 {
		add("upstream.user_id 不能为负数")
	}
	if u.Timeout <= 0 {
		add("upstream.timeout 必须大于 0")
	}
	// 上游按 IP 限流（New API 默认约 180 次 / 3 分钟），间隔太小会触发限流甚至风控。
	if u.MinRequestInterval < time.Second {
		add("upstream.min_request_interval 不能小于 1s")
	}

	s := c.Sync
	if s.LogInterval <= 0 || s.SnapshotInterval <= 0 {
		add("sync.log_interval 和 sync.snapshot_interval 必须大于 0")
	}
	if s.Lag < 0 {
		add("sync.lag 不能为负数")
	}
	if s.BackfillDays < 1 || s.BackfillDays > 3650 {
		add("sync.backfill_days 必须在 1～3650 之间")
	}
	if _, err := time.Parse("15:04", s.ReconcileAt); err != nil {
		add("sync.reconcile_at 必须是 HH:MM 格式")
	}

	st := c.Storage
	if st.SQLitePath == "" || st.BackupDir == "" {
		add("storage.sqlite_path 和 storage.backup_dir 不能为空")
	}
	if st.LogRetentionDays < 1 || st.SnapshotRetentionDays < 1 || st.BackupKeepDays < 1 {
		add("storage 的保留天数都必须大于 0")
	}

	if c.Server.Listen == "" {
		add("server.listen 不能为空")
	}
	loc, err := loadFixedOffsetLocation(c.Server.Timezone)
	if err != nil {
		add("server.timezone: %w", err)
	}
	c.loc = loc

	if c.Auth.SessionTTL <= 0 {
		add("auth.session_ttl 必须大于 0")
	}
	if w := c.Alert.Webhook; w != "" {
		if parsed, err := url.Parse(w); err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			add("alert.webhook 必须是 http(s) 地址")
		}
	}

	l := c.Log
	if !oneOf(l.Level, "debug", "info", "warn", "error") {
		add("log.level 只能是 debug、info、warn、error")
	}
	if !oneOf(l.Format, "json", "text") {
		add("log.format 只能是 json 或 text")
	}
	if l.RetentionDays < 1 {
		add("log.retention_days 必须大于 0")
	}
	return errors.Join(errs...)
}

// ValidateUpstream 检查访问上游需要的凭据，selfcheck、resync 和服务启动前调用。
func (c *Config) ValidateUpstream() error {
	var errs []error
	if c.Upstream.AccessToken == "" {
		errs = append(errs, errors.New("upstream.access_token 不能为空（4router 个人设置 → 安全设置 → 系统访问令牌）"))
	}
	if c.Upstream.UserID <= 0 {
		errs = append(errs, errors.New("upstream.user_id 必须填写 4router 用户 ID"))
	}
	return errors.Join(errs...)
}

// ValidateServer 是服务启动检查（开发文档 §9）：上游凭据、管理员密码哈希和会话密钥缺一不可。
func (c *Config) ValidateServer() error {
	errs := []error{c.ValidateUpstream()}
	if h := c.Auth.AdminPasswordHash; !strings.HasPrefix(h, "$2") {
		errs = append(errs, errors.New("auth.admin_password_hash 必须是 bcrypt 哈希，用 relayctl hash-password 生成"))
	}
	if c.Auth.AdminUser == "" {
		errs = append(errs, errors.New("auth.admin_user 不能为空"))
	}
	if len(c.Auth.SessionSecret) < minSessionSecretBytes {
		errs = append(errs, fmt.Errorf("auth.session_secret 至少 %d 字节，用 openssl rand -hex 32 生成", minSessionSecretBytes))
	}
	return errors.Join(errs...)
}

// Location 返回 server.timezone 对应的时区。
func (c *Config) Location() *time.Location { return c.loc }

// TZOffset 返回 server.timezone 相对 UTC 的秒数（Asia/Shanghai 是 28800），按天统计用整数运算
// (hour + offset) / 86400（开发文档 §3）。
func (c *Config) TZOffset() int64 {
	_, offset := time.Now().In(c.loc).Zone()
	return int64(offset)
}

// loadFixedOffsetLocation 加载时区，并拒绝有夏令时或偏移不是整小时的时区：
// 按天统计假设每天都是 24 小时、偏移固定，且本地整点就是 UTC 整点。
func loadFixedOffsetLocation(name string) (*time.Location, error) {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	year := time.Now().Year()
	_, jan := time.Date(year, time.January, 1, 0, 0, 0, 0, loc).Zone()
	_, jul := time.Date(year, time.July, 1, 0, 0, 0, 0, loc).Zone()
	if jan != jul {
		return nil, fmt.Errorf("%s 有夏令时，不支持", name)
	}
	// 小时汇总按 UTC 整点切分，本地整点必须和它对齐。
	if jan%3600 != 0 {
		return nil, fmt.Errorf("%s 的偏移不是整小时，不支持", name)
	}
	return loc, nil
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}
