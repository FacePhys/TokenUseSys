package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

const (
	// healthLagLimit：回填完成后，同步落后超过它 /healthz 返回 503（§4.7）。
	healthLagLimit = 10 * time.Minute
	// snapshotTolerance：令牌快照差值与上游合计相差超过 1% 时页面标黄（§4.3）。
	snapshotTolerance = 0.01
)

// taskLabels 是任务在状态条里的名字，顺序即检查顺序。
var taskLabels = []struct{ name, label string }{
	{port.TaskLogs, "日志同步"},
	{port.TaskSnapshot, "令牌快照"},
	{port.TaskStatus, "换算参数"},
	{port.TaskReconcile, "对账"},
	{port.TaskPurge, "清理"},
	{port.TaskBackup, "备份"},
}

// SyncStatus 是同步状态，总览的状态条和 /healthz 共用。
type SyncStatus struct {
	Backfilling      bool    `json:"backfilling"`
	BackfillProgress float64 `json:"backfill_progress"` // 0～1
	// SyncedAt 是数据截止的时刻（日志同步的游标），LastOKAt 是日志同步最后一次成功的时刻。
	SyncedAt   int64 `json:"synced_at"`
	LastOKAt   int64 `json:"last_ok_at"`
	LagSeconds int64 `json:"lag_seconds"`
	Healthy    bool  `json:"healthy"`
	// Error 是还没恢复的错误中最近的一个，没有时为空。
	Error           string `json:"error"`
	AuthPausedUntil int64  `json:"auth_paused_until,omitempty"`
}

// syncStatus 从 kv 读出同步状态：回填中算正常；回填完成后落后不超过 10 分钟算正常（§4.7）。
func (s *Service) syncStatus(ctx context.Context) (SyncStatus, error) {
	now := s.now()
	var st SyncStatus
	cursor, err := s.cursor(ctx)
	if err != nil {
		return st, err
	}
	until, err := s.kvInt(ctx, port.KeyBackfillUntil)
	if err != nil {
		return st, err
	}
	st.SyncedAt = cursor
	st.Backfilling = cursor > 0 && cursor < until
	st.BackfillProgress = 1
	if st.Backfilling {
		if since, ok := s.dataSinceStart(ctx); ok && until > since {
			st.BackfillProgress = math.Round(math.Max(0, float64(cursor-since))/float64(until-since)*1000) / 1000
		} else {
			st.BackfillProgress = 0
		}
	}
	if cursor > 0 {
		st.LagSeconds = max(now.Unix()-cursor, 0)
	}
	st.Healthy = cursor > 0 && (st.Backfilling || time.Duration(st.LagSeconds)*time.Second <= healthLagLimit)

	var latest int64
	for _, t := range taskLabels {
		v, ok, err := s.store.GetKV(ctx, port.KeyTaskPrefix+t.name)
		if err != nil {
			return st, err
		}
		var ts port.TaskState
		if !ok || json.Unmarshal([]byte(v), &ts) != nil {
			continue
		}
		if t.name == port.TaskLogs {
			st.LastOKAt = ts.LastOKAt
		}
		if ts.LastError != "" && ts.LastErrorAt > ts.LastOKAt && ts.LastErrorAt > latest {
			latest = ts.LastErrorAt
			st.Error = t.label + "失败：" + ts.LastError
		}
	}
	paused, err := s.kvInt(ctx, port.KeyAuthPausedUntil)
	if err != nil {
		return st, err
	}
	if paused > now.Unix() {
		st.AuthPausedUntil = paused
		st.Error = fmt.Sprintf("上游鉴权失败，同步暂停到 %s；检查 upstream.access_token 和 user_id",
			time.Unix(paused, 0).In(s.loc).Format("15:04"))
	}
	if cursor == 0 && st.Error == "" {
		st.Error = "还没有开始同步"
	}
	return st, nil
}

// Health 是 /healthz 的内容（§6）。
type Health struct {
	OK           bool   `json:"ok"`
	Backfilling  bool   `json:"backfilling"`
	LastSyncOKAt int64  `json:"last_sync_ok_at"`
	LagSeconds   int64  `json:"lag_seconds"`
	Error        string `json:"error"`
}

func (s *Service) Health(ctx context.Context) (Health, error) {
	st, err := s.syncStatus(ctx)
	if err != nil {
		return Health{}, err
	}
	return Health{OK: st.Healthy, Backfilling: st.Backfilling, LastSyncOKAt: st.LastOKAt, LagSeconds: st.LagSeconds,
		Error: st.Error}, nil
}

// Account 是账户余额（不随区间变）。
type Account struct {
	Balance      float64 `json:"balance"`
	Used         float64 `json:"used"`
	RequestCount int64   `json:"request_count"`
	UpdatedAt    int64   `json:"updated_at"`
}

type TokenCounts struct {
	Enabled int `json:"enabled"` // 状态启用且未删除
	Total   int `json:"total"`   // 不含已删除
}

// Reconcile 是最近一天的对账结果（§4.3）。Warn 为 true 时页面标黄：本地合计和上游不一致，
// 或令牌快照差值和上游相差超过 1%。
type Reconcile struct {
	Day       string   `json:"day"`
	Upstream  float64  `json:"upstream"`
	Local     float64  `json:"local"`
	Snapshot  *float64 `json:"snapshot"` // 当天 0 点前没有快照时为 null
	Warn      bool     `json:"warn"`
	CheckedAt int64    `json:"checked_at"`
}

type Summary struct {
	Currency string `json:"currency"`
	// Timezone 是 server.timezone，页面按它显示时间。Today 是这个时区的今天，DataSince 是日期选择器的下限。
	Timezone  string      `json:"timezone"`
	Today     string      `json:"today"`
	DataSince string      `json:"data_since"`
	Account   *Account    `json:"account"` // 还没同步过时为 null
	Tokens    TokenCounts `json:"tokens"`
	Sync      SyncStatus  `json:"sync"`
	Reconcile *Reconcile  `json:"reconcile"`
}

// Summary 返回总览里不随日期区间变的部分（§6）。
func (s *Service) Summary(ctx context.Context) (Summary, error) {
	cur, code, err := s.currency(ctx)
	if err != nil {
		return Summary{}, err
	}
	out := Summary{Currency: code, Timezone: s.loc.String(), Today: usage.DayStart(s.now(), s.loc).Format(usage.DayLayout)}
	if v, ok, err := s.store.GetKV(ctx, port.KeyDataSince); err != nil {
		return Summary{}, err
	} else if ok {
		out.DataSince = v
	}
	if v, ok, err := s.store.GetKV(ctx, port.KeyAccount); err != nil {
		return Summary{}, err
	} else if ok {
		var acc usage.Account
		if json.Unmarshal([]byte(v), &acc) == nil {
			out.Account = &Account{Balance: amount(cur, acc.Quota), Used: amount(cur, acc.UsedQuota),
				RequestCount: acc.RequestCount, UpdatedAt: acc.UpdatedAt}
		}
	}
	records, err := s.store.Tokens(ctx)
	if err != nil {
		return Summary{}, err
	}
	for _, r := range records {
		if r.DeletedAt != 0 {
			continue
		}
		out.Tokens.Total++
		if r.Status == usage.StatusEnabled {
			out.Tokens.Enabled++
		}
	}
	if out.Sync, err = s.syncStatus(ctx); err != nil {
		return Summary{}, err
	}
	rep, err := s.store.LatestReconcile(ctx)
	if err != nil {
		return Summary{}, err
	}
	if rep != nil {
		rc := &Reconcile{Day: rep.Day, Upstream: amount(cur, rep.UpstreamQuota), Local: amount(cur, rep.LocalQuota),
			CheckedAt: rep.CheckedAt, Warn: rep.UpstreamQuota != rep.LocalQuota}
		if rep.SnapshotQuota != nil {
			snap := amount(cur, *rep.SnapshotQuota)
			rc.Snapshot = &snap
			diff := math.Abs(float64(*rep.SnapshotQuota - rep.UpstreamQuota))
			if diff > snapshotTolerance*math.Max(float64(rep.UpstreamQuota), 1) {
				rc.Warn = true
			}
		}
		out.Reconcile = rc
	}
	return out, nil
}

func (s *Service) cursor(ctx context.Context) (int64, error) {
	return s.kvInt(ctx, port.KeyLogsCursor)
}

func (s *Service) kvInt(ctx context.Context, key string) (int64, error) {
	v, ok, err := s.store.GetKV(ctx, key)
	if err != nil || !ok {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("kv %s 不是整数：%q", key, v)
	}
	return n, nil
}

func (s *Service) dataSinceStart(ctx context.Context) (int64, bool) {
	v, ok, err := s.store.GetKV(ctx, port.KeyDataSince)
	if err != nil || !ok {
		return 0, false
	}
	t, err := time.ParseInLocation(usage.DayLayout, v, s.loc)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}
