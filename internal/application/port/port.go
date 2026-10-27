// Package port 定义同步和看板用到的端口：上游（4router）和本地存储。
package port

import (
	"context"
	"errors"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// kv 表的键（§5），同步器写，看板和 /healthz 读。
const (
	// KeyLogsCursor 之前的日志都已同步，和窗口替换在同一个事务里更新。
	KeyLogsCursor = "logs_cursor"
	// KeyDataSince 是首次回填的起始日期（2026-10-20），日期选择器的下限；resync 更早的日期时提前。
	KeyDataSince = "data_since"
	// KeyBackfillUntil 是首次回填要追到的位置（Unix 秒），游标到这里之前算「回填中」。
	KeyBackfillUntil = "backfill_until"
	// KeyAccount 是最新的账户余额（JSON），不存历史。
	KeyAccount = "account"
	// KeyCurrency 是 /api/status 的换算参数（JSON）。
	KeyCurrency = "status"
	// KeyAuthPausedUntil 是鉴权失败后暂停到的时刻（Unix 秒）。
	KeyAuthPausedUntil = "auth_paused_until"
	// KeyTaskPrefix 加任务名是各任务的执行状态（JSON：最后成功时间、最后错误）。
	KeyTaskPrefix = "task:"
	// 每日任务最后完成的日期。
	KeyReconcileDay = "reconcile_last_day"
	KeyPurgeDay     = "purge_last_day"
	KeyBackupDay    = "backup_last_day"
)

// 同步任务名，也是 kv 里执行状态的键名后缀（KeyTaskPrefix + 名字）。
const (
	TaskStatus    = "status"
	TaskSnapshot  = "snapshot"
	TaskLogs      = "logs"
	TaskPurge     = "purge"
	TaskBackup    = "backup"
	TaskReconcile = "reconcile"
)

// TaskState 是一个任务的执行状态（JSON 存在 kv 里），页面状态条和 /healthz 读它。
// LastErrorAt 晚于 LastOKAt 表示错误还没恢复。
type TaskState struct {
	LastOKAt    int64  `json:"last_ok_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	LastErrorAt int64  `json:"last_error_at,omitempty"`
}

// Alert 是一条告警（§4.7）。Key 相同的告警 6 小时内只发一次；Resolved 为 true 是恢复通知，
// 只在这个 Key 告过警、还没恢复时才发。
type Alert struct {
	Key      string
	Text     string
	Resolved bool
}

// Alerter 发送告警。失败只记日志，不影响同步。
type Alerter interface {
	Notify(ctx context.Context, a Alert)
}

// ErrUpstreamAuth 表示上游鉴权失败（access token 失效或用户 ID 不对）。同步器收到后暂停 30 分钟（§4.7）。
var ErrUpstreamAuth = errors.New("上游鉴权失败")

// Upstream 是 4router 的只读接口，所有调用都经过全局限速。
type Upstream interface {
	// Currency 返回 /api/status 的换算参数。
	Currency(ctx context.Context) (usage.Currency, error)
	// Account 返回账户余额。
	Account(ctx context.Context) (usage.Account, error)
	// Tokens 拉全部令牌。中途某页失败时返回已拉到的令牌和错误，调用方据此决定是否标删除（§4.4）。
	Tokens(ctx context.Context) ([]usage.Token, error)
	// LogsPage 查一页消费日志，区间 [start, end] 两端都含（§10 第 6 步），page 从 1 开始，每页 usage.PageSize 条。
	// total 是上游给的总数，只做参考。
	LogsPage(ctx context.Context, start, end int64, page int) (entries []usage.LogEntry, total int64, err error)
	// LogStat 返回 [start, end] 内消费日志的 quota 合计。
	LogStat(ctx context.Context, start, end int64) (int64, error)
}

// Store 是本地 SQLite 的读写（§5）。
type Store interface {
	GetKV(ctx context.Context, key string) (value string, ok bool, err error)
	SetKV(ctx context.Context, key, value string) error

	// SyncTokens 写入一轮令牌列表：upsert 令牌，used_quota 或 status 有变化的写快照（时间记 slot）。
	// complete 为 true（所有页都拉成功）时，把列表里没有的令牌标为已删除。
	SyncTokens(ctx context.Context, tokens []usage.Token, complete bool, slot, now time.Time) error
	// EnsureTokens 为只在日志里见过的令牌建占位行（状态未知、已删除），已有的不动。
	EnsureTokens(ctx context.Context, entries []usage.LogEntry, now time.Time) error
	// TokenIDsByName 返回令牌名到 id 的映射，供日志归属用（含已删除的令牌）。
	TokenIDsByName(ctx context.Context) (map[string][]int64, error)

	// HasLocalData 返回 [start, end) 内本地是否有明细，或者有完全落在窗口里的小时汇总（§4.3 防误删）。
	HasLocalData(ctx context.Context, start, end int64) (bool, error)
	// ReplaceWindow 在一个事务内：删掉 [start, end) 的明细、写入 entries、重算涉及的小时汇总；
	// cursor 非空时同时把 logs_cursor 改成它。
	ReplaceWindow(ctx context.Context, start, end int64, entries []usage.LogEntry, cursor *int64) error

	// LocalQuota 返回 [start, end) 内明细的 quota 合计。
	LocalQuota(ctx context.Context, start, end int64) (int64, error)
	// SnapshotQuotaDelta 返回各令牌 used_quota 在 start 和 end 两个时刻的差值之和；
	// start 时刻之前还没有任何快照时 ok 为 false。
	SnapshotQuotaDelta(ctx context.Context, start, end int64) (delta int64, ok bool, err error)
	SaveReconcile(ctx context.Context, r usage.ReconcileReport) error

	// Purge 删除 logsBefore 以前的明细，以及 snapshotsBefore 以前的快照（每个令牌至少保留最新一行）。
	Purge(ctx context.Context, logsBefore, snapshotsBefore int64) (logs, snapshots int64, err error)
	// Backup 把数据库备份到 dir，删除超过 keepDays 天的备份，返回备份文件路径（§12.6）。
	Backup(ctx context.Context, dir string, now time.Time, keepDays int) (string, error)
}

// Bucket 是用量查询的时间粒度。
type Bucket int

const (
	BucketNone Bucket = iota // 不按时间分组
	BucketHour
	BucketDay // 按 server.timezone 的自然日
)

// UsageQuery 是对 usage_hourly 的汇总查询：时间范围 [Start, End) 对齐整点，按需要的维度分组。
type UsageQuery struct {
	Start, End int64
	Bucket     Bucket
	// TZOffset 是按天分组时当地时区相对 UTC 的秒数（§3）。
	TZOffset int64
	ByToken  bool
	ByModel  bool
	// TokenID 非空时只看这个令牌（0 是未归属）。
	TokenID *int64
}

// UsageGroup 是一组汇总结果。不按某个维度分组时，对应字段为零值。
type UsageGroup struct {
	// Bucket 是时间段的起点（Unix 秒）：按小时是整点，按天是当地 0 点。
	Bucket  int64
	TokenID int64
	Model   string
	usage.Totals
}

// LogQuery 查一个令牌的最近请求，按时间倒序。Start、End 为 0 时不限。
type LogQuery struct {
	TokenID    int64
	Model      string
	Start, End int64
	Limit      int
	Offset     int
}

// DashboardStore 是看板的只读查询，走读库，不和同步器争写库的连接（§5）。
type DashboardStore interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
	Tokens(ctx context.Context) ([]usage.TokenRecord, error)
	Token(ctx context.Context, id int64) (usage.TokenRecord, bool, error)
	Usage(ctx context.Context, q UsageQuery) ([]UsageGroup, error)
	Logs(ctx context.Context, q LogQuery) (entries []usage.LogEntry, total int64, err error)
	// LatestReconcile 返回最近一天的对账结果，没有时为 nil。
	LatestReconcile(ctx context.Context) (*usage.ReconcileReport, error)
}
