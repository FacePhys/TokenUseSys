package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/sqlite"
)

var cst = time.FixedZone("CST", 8*3600)

// 当前时间 2026-10-27 12:00；数据在 10-25～10-27 三天里。
var (
	now   = time.Date(2026, 10, 27, 12, 0, 0, 0, cst)
	day25 = time.Date(2026, 10, 25, 0, 0, 0, 0, cst)
)

type fixture struct {
	t   *testing.T
	db  *sqlite.DB
	svc *Service
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "usage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{t: t, db: db, now: now}
	f.svc = New(db.Dashboard(), cst, 8*3600, func() time.Time { return f.now })
	return f
}

func (f *fixture) kv(key string, v any) {
	f.t.Helper()
	s, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		require.NoError(f.t, err)
		s = string(b)
	}
	require.NoError(f.t, f.db.SetKV(context.Background(), key, s))
}

func at(day int, hour int) int64 {
	return day25.AddDate(0, 0, day).Add(time.Duration(hour) * time.Hour).Unix()
}

func e(ts int64, token int64, model string, quota int64) usage.LogEntry {
	return usage.LogEntry{CreatedAt: ts, TokenID: token, TokenName: "x", ModelName: model, Quota: quota,
		PromptTokens: 10, CompletionTokens: 2, CacheReadTokens: 1, IsStream: true}
}

// seed 写入：令牌 1 启用、2 禁用且无限额度、3 已删除但区间内有用量、4 已删除且没有用量；外加未归属用量。
// 金额按 500000 quota = ¥1 换算。
func (f *fixture) seed() {
	f.t.Helper()
	ctx := context.Background()
	slot := now.Add(-time.Hour)
	require.NoError(f.t, f.db.SyncTokens(ctx, []usage.Token{
		{ID: 1, Name: "prod", KeyHint: "sk-AbCd…9xYz", Status: 1, RemainQuota: 1_000_000, UsedQuota: 2_500_000, ExpiredTime: -1, AccessedTime: 1793080000},
		{ID: 2, Name: "dev", Status: 2, Unlimited: true, UsedQuota: 500_000, ExpiredTime: 1800000000, ModelLimits: "gpt-4o, claude"},
		{ID: 3, Name: "old", Status: 1},
		{ID: 4, Name: "gone", Status: 1},
	}, true, slot, slot))
	require.NoError(f.t, f.db.SyncTokens(ctx, []usage.Token{
		{ID: 1, Name: "prod", Status: 1, RemainQuota: 1_000_000, UsedQuota: 2_500_000, ExpiredTime: -1, AccessedTime: 1793080000},
		{ID: 2, Name: "dev", Status: 2, Unlimited: true, UsedQuota: 500_000, ExpiredTime: 1800000000, ModelLimits: "gpt-4o, claude"},
	}, true, slot, slot)) // 3、4 被标为已删除
	entries := []usage.LogEntry{
		e(at(0, 1), 1, "m1", 1_000_000), // 10-25：¥2
		e(at(0, 2), 2, "m2", 250_000),   // 10-25：¥0.5
		e(at(1, 3), 1, "m1", 500_000),   // 10-26：¥1
		e(at(1, 4), 3, "m1", 100_000),   // 10-26：¥0.2，已删除的令牌
		e(at(1, 5), 0, "m3", 50_000),    // 10-26：¥0.1，未归属
		e(at(2, 9), 1, "m2", 123_457),   // 10-27：¥0.246914
	}
	require.NoError(f.t, f.db.ReplaceWindow(ctx, day25.Unix(), now.Unix(), entries, nil))
	f.kv(port.KeyCurrency, usage.Currency{QuotaPerUnit: 500000, DisplayType: "CNY", ExchangeRate: 1})
	f.kv(port.KeyLogsCursor, fmt.Sprint(now.Add(-2*time.Minute).Unix()))
	f.kv(port.KeyBackfillUntil, fmt.Sprint(day25.Unix()+3600))
	f.kv(port.KeyDataSince, "2026-10-25")
}

func (f *fixture) rng(start, end string) Range {
	f.t.Helper()
	r, err := f.svc.ParseRange(start, end)
	require.NoError(f.t, err)
	return r
}

func TestParseRange(t *testing.T) {
	f := newFixture(t)
	r := f.rng("", "")
	require.Equal(t, Range{Start: "2026-10-21", End: "2026-10-27"}, Range{Start: r.Start, End: r.End}, "默认近 7 天，含今天")
	require.Equal(t, 7, r.Days())
	require.Equal(t, time.Date(2026, 10, 28, 0, 0, 0, 0, cst).Unix(), r.To(), "结束日期当天也包含")

	r = f.rng("2026-10-27", "2026-10-27")
	require.Equal(t, 1, r.Days())
	r = f.rng("2026-10-01", "")
	require.Equal(t, "2026-10-27", r.End, "只给 start 时到今天")
	r = f.rng("", "2026-10-10")
	require.Equal(t, "2026-10-04", r.Start, "只给 end 时往前 7 天")
	r = f.rng("2025-09-23", "2026-10-27")
	require.Equal(t, MaxRangeDays, r.Days())

	for _, c := range [][2]string{{"2026/10/01", ""}, {"2026-10-28", "2026-10-27"}, {"2025-09-22", "2026-10-27"}, {"", "yesterday"}} {
		_, err := f.svc.ParseRange(c[0], c[1])
		require.ErrorIs(t, err, ErrInvalidArgument, "%v", c)
	}
}

func TestTokensList(t *testing.T) {
	f := newFixture(t)
	f.seed()
	list, err := f.svc.Tokens(context.Background(), f.rng("2026-10-25", "2026-10-27"))
	require.NoError(t, err)
	require.Equal(t, "CNY", list.Currency)
	require.Equal(t, now.Add(-2*time.Minute).Unix(), list.SyncedAt)

	var ids []int64
	for _, it := range list.Items {
		ids = append(ids, it.ID)
	}
	require.Equal(t, []int64{1, 2, 3, 0}, ids, "按金额排；已删除没用量的不列；未归属单独一行")
	prod := list.Items[0]
	require.Equal(t, "enabled", prod.Status)
	require.Equal(t, "sk-AbCd…9xYz", prod.KeyHint)
	require.InDelta(t, 3.2469, prod.Usage.Amount, 1e-9)
	require.Equal(t, int64(3), prod.Usage.Requests)
	require.InDelta(t, 2.0, *prod.Remain, 1e-9)
	require.InDelta(t, 5.0, *prod.UsedTotal, 1e-9)
	require.Nil(t, prod.ExpiresAt, "永不过期为 null")
	require.Equal(t, int64(1793080000), *prod.LastUsedAt)

	dev := list.Items[1]
	require.Equal(t, "disabled", dev.Status)
	require.Nil(t, dev.Remain, "无限额度没有剩余")
	require.Equal(t, int64(1800000000), *dev.ExpiresAt)
	require.Equal(t, "deleted", list.Items[2].Status)
	require.Equal(t, UnattributedName, list.Items[3].Name)
	require.Equal(t, "unattributed", list.Items[3].Status)
	require.Nil(t, list.Items[3].UsedTotal)

	require.InDelta(t, 4.0469, list.Total.Amount, 1e-9)
	require.Equal(t, int64(6), list.Total.Requests)

	// 只看 10-27：已删除的令牌 3 这天没有用量，不列；未删除的照常列出（用量为 0）。
	list, err = f.svc.Tokens(context.Background(), f.rng("2026-10-27", "2026-10-27"))
	require.NoError(t, err)
	ids = ids[:0]
	for _, it := range list.Items {
		ids = append(ids, it.ID)
	}
	require.Equal(t, []int64{1, 2}, ids)
	require.Zero(t, list.Items[1].Usage.Requests)
}

func TestTokenDetail(t *testing.T) {
	f := newFixture(t)
	f.seed()
	d, err := f.svc.Token(context.Background(), 2)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-4o", "claude"}, d.ModelLimits)
	require.Equal(t, "CNY", d.Currency)
	d, err = f.svc.Token(context.Background(), 3)
	require.NoError(t, err)
	require.NotNil(t, d.DeletedAt)
	d, err = f.svc.Token(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, UnattributedName, d.Name)
	_, err = f.svc.Token(context.Background(), 99)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSeriesByDay(t *testing.T) {
	f := newFixture(t)
	f.seed()
	r := f.rng("2026-10-24", "2026-10-27")
	s, err := f.svc.Series(context.Background(), r, port.BucketDay, 2, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"2026-10-24", "2026-10-25", "2026-10-26", "2026-10-27"}, s.Buckets)
	require.Equal(t, day25.AddDate(0, 0, -1).Unix(), s.Starts[0])
	require.Len(t, s.Series, 3, "前 2 名加「其他」")
	require.Equal(t, int64(1), *s.Series[0].TokenID)
	require.Equal(t, "prod", s.Series[0].Name)
	require.Equal(t, []float64{0, 2, 1, 0.2469}, s.Series[0].Amount, "没有数据的日期补 0")
	require.Equal(t, "dev", s.Series[1].Name)
	require.Nil(t, s.Series[2].TokenID)
	require.Equal(t, OthersName, s.Series[2].Name)
	require.Equal(t, []float64{0, 0, 0.3, 0}, s.Series[2].Amount, "已删除的令牌和未归属合并进「其他」")

	// 各序列相加等于区间合计。
	list, err := f.svc.Tokens(context.Background(), r)
	require.NoError(t, err)
	var sum float64
	var reqs int64
	for _, line := range s.Series {
		for i := range line.Amount {
			sum += line.Amount[i]
			reqs += line.Requests[i]
		}
	}
	require.InDelta(t, list.Total.Amount, sum, 1e-9)
	require.Equal(t, list.Total.Requests, reqs)

	// top 足够大时没有「其他」；未归属单独一条。
	s, err = f.svc.Series(context.Background(), r, port.BucketDay, 10, nil)
	require.NoError(t, err)
	require.Len(t, s.Series, 4)
	require.Equal(t, UnattributedName, s.Series[3].Name)

	// top=all：每个令牌各一条（已删除的、未归属的也单独一条），不合并「其他」，按金额从高到低。
	all, err := f.svc.Series(context.Background(), r, port.BucketDay, AllTop, nil)
	require.NoError(t, err)
	var names []string
	for _, line := range all.Series {
		require.NotNil(t, line.TokenID, "不合并「其他」")
		names = append(names, line.Name)
	}
	require.Equal(t, []string{"prod", "dev", "old", UnattributedName}, names)
	_, err = f.svc.Series(context.Background(), r, port.BucketDay, -2, nil)
	require.ErrorIs(t, err, ErrInvalidArgument, "AllTop 以外的负数不合法")

	tok := int64(2)
	s, err = f.svc.Series(context.Background(), r, port.BucketDay, 0, &tok)
	require.NoError(t, err)
	require.Len(t, s.Series, 1)
	require.Equal(t, []float64{0, 0.5, 0, 0}, s.Series[0].Amount)

	tok = 4 // 区间内没有用量：返回一条全 0 的序列
	s, err = f.svc.Series(context.Background(), r, port.BucketDay, 0, &tok)
	require.NoError(t, err)
	require.Equal(t, []float64{0, 0, 0, 0}, s.Series[0].Amount)
	require.Equal(t, "gone", s.Series[0].Name)

	_, err = f.svc.Series(context.Background(), r, port.BucketDay, 21, nil)
	require.ErrorIs(t, err, ErrInvalidArgument)
}

func TestSeriesByHour(t *testing.T) {
	f := newFixture(t)
	f.seed()
	s, err := f.svc.Series(context.Background(), f.rng("2026-10-26", "2026-10-26"), port.BucketHour, 0, nil)
	require.NoError(t, err)
	require.Len(t, s.Buckets, 24)
	require.Equal(t, "2026-10-26 03:00", s.Buckets[3])
	require.InDelta(t, 1.0, s.Series[0].Amount[3], 1e-9)

	_, err = f.svc.Series(context.Background(), f.rng("2026-10-20", "2026-10-27"), port.BucketHour, 0, nil)
	require.ErrorIs(t, err, ErrInvalidArgument, "按小时最长 7 天")
}

func TestModels(t *testing.T) {
	f := newFixture(t)
	f.seed()
	r := f.rng("2026-10-25", "2026-10-27")
	list, err := f.svc.Models(context.Background(), r, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"m1", "m2", "m3"}, []string{list.Items[0].Model, list.Items[1].Model, list.Items[2].Model})
	require.InDelta(t, 3.2, list.Items[0].Amount, 1e-9)
	require.InDelta(t, 4.0469, list.Total.Amount, 1e-9)

	tok := int64(1)
	list, err = f.svc.Models(context.Background(), r, &tok)
	require.NoError(t, err)
	require.Len(t, list.Items, 2)
}

func TestLogs(t *testing.T) {
	f := newFixture(t)
	f.seed()
	page, err := f.svc.Logs(context.Background(), 1, nil, "", 0, 0)
	require.NoError(t, err)
	require.Equal(t, DefaultLogPageSize, page.PageSize)
	require.Equal(t, int64(3), page.Total)
	require.Equal(t, at(2, 9), page.Items[0].CreatedAt)
	require.InDelta(t, 0.2469, page.Items[0].Amount, 1e-9)

	page, err = f.svc.Logs(context.Background(), 1, nil, "", 2, 2)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)

	r := f.rng("2026-10-26", "2026-10-26")
	page, err = f.svc.Logs(context.Background(), 1, &r, "", 1, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total, "随日期区间筛选")
	page, err = f.svc.Logs(context.Background(), 1, nil, "m2", 1, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)

	_, err = f.svc.Logs(context.Background(), 1, nil, "", 1, 101)
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = f.svc.Logs(context.Background(), 1, nil, "", -1, 10)
	require.ErrorIs(t, err, ErrInvalidArgument)
}

func TestExport(t *testing.T) {
	f := newFixture(t)
	f.seed()
	table, err := f.svc.Export(context.Background(), f.rng("2026-10-25", "2026-10-27"), port.BucketDay, nil)
	require.NoError(t, err)
	require.Len(t, table.Rows, 6)
	first := table.Rows[0]
	require.Equal(t, "2026-10-25", first.Bucket)
	require.Equal(t, "prod", first.TokenName)
	require.InDelta(t, 2.0, first.Amount, 1e-9)
	var names []string
	for _, row := range table.Rows {
		names = append(names, row.TokenName)
	}
	require.Contains(t, names, UnattributedName)
	require.Contains(t, names, "old", "已删除的令牌照常导出")

	table, err = f.svc.Export(context.Background(), f.rng("2026-10-26", "2026-10-26"), port.BucketHour, nil)
	require.NoError(t, err)
	require.Equal(t, "2026-10-26 03:00", table.Rows[0].Bucket)

	_, err = f.svc.Export(context.Background(), f.rng("2026-09-01", "2026-10-27"), port.BucketHour, nil)
	require.ErrorIs(t, err, ErrInvalidArgument, "按小时导出最长 31 天")
}

func TestSummary(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	f.kv(port.KeyAccount, usage.Account{Quota: 5_000_000, UsedQuota: 1_000_000, RequestCount: 42, UpdatedAt: 7})
	sum, err := f.svc.Summary(ctx)
	require.NoError(t, err)
	require.Equal(t, "2026-10-27", sum.Today)
	require.Equal(t, "2026-10-25", sum.DataSince)
	require.InDelta(t, 10.0, sum.Account.Balance, 1e-9)
	require.Equal(t, TokenCounts{Enabled: 1, Total: 2}, sum.Tokens)
	require.True(t, sum.Sync.Healthy)
	require.False(t, sum.Sync.Backfilling)
	require.Nil(t, sum.Reconcile)

	snap := int64(1_010_000)
	require.NoError(t, f.db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-26", UpstreamQuota: 1_000_000, LocalQuota: 1_000_000, SnapshotQuota: &snap}))
	sum, err = f.svc.Summary(ctx)
	require.NoError(t, err)
	require.Equal(t, "2026-10-26", sum.Reconcile.Day)
	require.False(t, sum.Reconcile.Warn, "快照差值相差 1% 以内")
	snap = 1_020_000
	require.NoError(t, f.db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-26", UpstreamQuota: 1_000_000, LocalQuota: 1_000_000, SnapshotQuota: &snap}))
	sum, err = f.svc.Summary(ctx)
	require.NoError(t, err)
	require.True(t, sum.Reconcile.Warn, "快照差值相差超过 1%")
	require.NoError(t, f.db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-26", UpstreamQuota: 1_000_000, LocalQuota: 999_999}))
	sum, err = f.svc.Summary(ctx)
	require.NoError(t, err)
	require.True(t, sum.Reconcile.Warn, "本地合计和上游不一致")
	require.Nil(t, sum.Reconcile.Snapshot)
}

func TestHealthAndSyncErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h, err := f.svc.Health(ctx)
	require.NoError(t, err)
	require.False(t, h.OK)
	require.Equal(t, "还没有开始同步", h.Error)

	f.seed()
	h, err = f.svc.Health(ctx)
	require.NoError(t, err)
	require.True(t, h.OK)
	require.Equal(t, int64(120), h.LagSeconds)

	// 已恢复的错误不显示，没恢复的显示最近一个。
	f.kv(port.KeyTaskPrefix+port.TaskBackup, port.TaskState{LastOKAt: 100, LastError: "旧错误", LastErrorAt: 50})
	f.kv(port.KeyTaskPrefix+port.TaskLogs, port.TaskState{LastOKAt: 100, LastError: "上游 502", LastErrorAt: 200})
	h, err = f.svc.Health(ctx)
	require.NoError(t, err)
	require.Equal(t, "日志同步失败：上游 502", h.Error)
	require.Equal(t, int64(100), h.LastSyncOKAt)

	f.now = now.Add(11 * time.Minute)
	h, err = f.svc.Health(ctx)
	require.NoError(t, err)
	require.False(t, h.OK, "回填完成后落后超过 10 分钟")

	f.kv(port.KeyAuthPausedUntil, fmt.Sprint(f.now.Add(20*time.Minute).Unix()))
	sum, err := f.svc.Summary(ctx)
	require.NoError(t, err)
	require.Contains(t, sum.Sync.Error, "上游鉴权失败，同步暂停到 12:31")

	// 回填中：落后很多也算正常，并给出进度。
	f.kv(port.KeyBackfillUntil, fmt.Sprint(now.Unix()))
	f.kv(port.KeyLogsCursor, fmt.Sprint(day25.AddDate(0, 0, 1).Unix()))
	sum, err = f.svc.Summary(ctx)
	require.NoError(t, err)
	require.True(t, sum.Sync.Backfilling)
	require.True(t, sum.Sync.Healthy)
	require.InDelta(t, 24.0/60.0, sum.Sync.BackfillProgress, 0.001)
}

func TestDailyReport(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	f.kv(port.KeyAccount, usage.Account{Quota: 1_234_567_890})

	text, err := f.svc.DailyReport(ctx, time.Date(2026, 10, 26, 0, 0, 0, 0, cst))
	require.NoError(t, err)
	require.Equal(t, `用量日报（2026-10-26 周一）
总消费 ¥1.30，较前一天 -48.0%
请求 3 次，Token 36（输入 30，输出 6）

按令牌：
prod  ¥1.00（76.9%），1 次
old  ¥0.20（15.4%），1 次
未归属  ¥0.10（7.7%），1 次

按模型：
m1  ¥1.20（92.3%）
m3  ¥0.10（7.7%）

账户余额 ¥2,469.14`, text, "没用量的令牌不列，已删除和未归属的照常列")

	// 数字和看板上选这一天时一致。
	list, err := f.svc.Tokens(ctx, f.rng("2026-10-26", "2026-10-26"))
	require.NoError(t, err)
	require.InDelta(t, 1.30, list.Total.Amount, 1e-9)

	text, err = f.svc.DailyReport(ctx, time.Date(2026, 10, 25, 23, 0, 0, 0, cst))
	require.NoError(t, err)
	require.Contains(t, text, "用量日报（2026-10-25 周日）\n总消费 ¥2.50\n", "前一天没有数据时不比较；当天任意时刻都算这一天")

	text, err = f.svc.DailyReport(ctx, time.Date(2026, 10, 20, 0, 0, 0, 0, cst))
	require.NoError(t, err)
	require.Equal(t, "用量日报（2026-10-20 周二）\n当天没有用量。\n\n账户余额 ¥2,469.14", text)
}

func TestDailyReportReconcileWarning(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, cst)
	snap := int64(1_500_000)
	require.NoError(t, f.db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-26", UpstreamQuota: 700_000, LocalQuota: 650_000, SnapshotQuota: &snap}))
	text, err := f.svc.DailyReport(ctx, day)
	require.NoError(t, err)
	require.Contains(t, text, "注意：当天对账有差异（上游 ¥1.40，本地 ¥1.30，令牌快照 ¥3.00），以上数字可能不准。")

	// 对账一致时不提示；别的日期的对账结果不影响这一天。
	require.NoError(t, f.db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-26", UpstreamQuota: 650_000, LocalQuota: 650_000}))
	text, err = f.svc.DailyReport(ctx, day)
	require.NoError(t, err)
	require.NotContains(t, text, "注意")
	require.NoError(t, f.db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-27", UpstreamQuota: 1, LocalQuota: 2}))
	text, err = f.svc.DailyReport(ctx, day)
	require.NoError(t, err)
	require.NotContains(t, text, "注意")
}

func TestDailyReportMergesTailAndStaysShort(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	slot := now.Add(-time.Hour)
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, cst)
	var tokens []usage.Token
	var entries []usage.LogEntry
	for i := range int64(12) {
		id := i + 1
		tokens = append(tokens, usage.Token{ID: id, Name: fmt.Sprintf("名字特别特别特别特别特别特别特别特别特别特别特别长的令牌-%02d", id), Status: 1})
		// 金额从高到低：令牌 1 最多。模型 7 个。
		entries = append(entries, usage.LogEntry{CreatedAt: day.Unix() + 60*id, TokenID: id, TokenName: "x",
			ModelName: fmt.Sprintf("名字特别特别特别特别特别特别特别特别特别特别特别长的模型-%02d", id%7), Quota: (20 - id) * 50_000_000_000,
			PromptTokens: 123_456_789, CompletionTokens: 12_345_678})
	}
	require.NoError(t, f.db.SyncTokens(ctx, tokens, true, slot, slot))
	require.NoError(t, f.db.ReplaceWindow(ctx, day.Unix(), day.Unix()+86400, entries, nil))
	f.kv(port.KeyCurrency, usage.Currency{QuotaPerUnit: 500000, DisplayType: "CNY", ExchangeRate: 1})

	text, err := f.svc.DailyReport(ctx, day)
	require.NoError(t, err)
	require.Contains(t, text, "其他 4 个  ¥", "前 8 个令牌以外合并")
	require.Contains(t, text, "其他 2 个  ¥", "前 5 个模型以外合并")
	require.Contains(t, text, "…")
	require.Contains(t, text, "Token 16.3亿（输入 14.8亿，输出 1.48亿）")
	require.LessOrEqual(t, len(text)+len("【RelayUsage】"), 2048, "企业微信的文本消息上限是 2048 字节")
	require.True(t, utf8.ValidString(text), "截断时不切断字符")
}

func TestCompactCount(t *testing.T) {
	for want, n := range map[string]int64{"9,999": 9999, "1万": 10000, "1.23万": 12345, "12.3万": 123456, "123万": 1234567, "1亿": 100000000, "1.23亿": 123456789} {
		require.Equal(t, want, compactCount(n))
	}
}

func TestReportFailureShownInStatus(t *testing.T) {
	f := newFixture(t)
	f.seed()
	f.kv(port.KeyTaskPrefix+port.TaskReport, port.TaskState{LastError: "机器人返回 HTTP 502", LastErrorAt: 200})
	h, err := f.svc.Health(context.Background())
	require.NoError(t, err)
	require.Equal(t, "用量日报失败：机器人返回 HTTP 502", h.Error)
	require.True(t, h.OK, "发不出日报不影响同步健康")
}
