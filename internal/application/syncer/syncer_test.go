package syncer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/sqlite"
)

var cst = time.FixedZone("CST", 8*3600)

// fakeUpstream 在内存里模拟 4router：日志按时间倒序分页，区间两端都含。
type fakeUpstream struct {
	mu      sync.Mutex
	logs    []usage.LogEntry
	tokens  []usage.Token
	pageCap int // 上游实际的每页条数上限，0 表示 100
	// onPage 在每次查日志时调用，测试用来中途取消。
	onPage        func(page int)
	authErr       bool
	failTokenPage bool
	failLogs      bool
	calls         int
}

// fakeAlerter 记下所有告警（去重是 alert 包的事，这里原样记录）。
type fakeAlerter struct{ alerts []port.Alert }

func (f *fakeAlerter) Notify(_ context.Context, a port.Alert) { f.alerts = append(f.alerts, a) }

// raised 返回发出的告警（不含恢复通知）的 key。
func (f *fakeAlerter) raised() []string {
	var keys []string
	for _, a := range f.alerts {
		if !a.Resolved {
			keys = append(keys, a.Key)
		}
	}
	return keys
}

func (f *fakeAlerter) last(key string) port.Alert {
	for i := len(f.alerts) - 1; i >= 0; i-- {
		if f.alerts[i].Key == key {
			return f.alerts[i]
		}
	}
	return port.Alert{}
}

func (f *fakeUpstream) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls++
	if f.authErr {
		return fmt.Errorf("%w：测试", port.ErrUpstreamAuth)
	}
	return nil
}

func (f *fakeUpstream) Currency(ctx context.Context) (usage.Currency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return usage.Currency{QuotaPerUnit: 500000, DisplayType: "CNY", ExchangeRate: 1}, f.check(ctx)
}

func (f *fakeUpstream) Account(ctx context.Context) (usage.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return usage.Account{Quota: 1000, UsedQuota: 10}, f.check(ctx)
}

func (f *fakeUpstream) Tokens(ctx context.Context) ([]usage.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	if f.failTokenPage {
		return slices.Clone(f.tokens[:1]), errors.New("第 2 页失败")
	}
	return slices.Clone(f.tokens), nil
}

func (f *fakeUpstream) LogsPage(ctx context.Context, start, end int64, page int) ([]usage.LogEntry, int64, error) {
	if f.onPage != nil {
		f.onPage(page)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(ctx); err != nil {
		return nil, 0, err
	}
	if f.failLogs {
		return nil, 0, errors.New("上游返回 502")
	}
	matched := f.matching(start, end)
	size := usage.PageSize
	if f.pageCap > 0 {
		size = f.pageCap
	}
	from := min((page-1)*size, len(matched))
	to := min(from+size, len(matched))
	return slices.Clone(matched[from:to]), int64(len(matched)), nil
}

func (f *fakeUpstream) matching(start, end int64) []usage.LogEntry {
	var matched []usage.LogEntry
	for _, l := range f.logs {
		if l.CreatedAt >= start && l.CreatedAt <= end {
			matched = append(matched, l)
		}
	}
	slices.SortStableFunc(matched, func(a, b usage.LogEntry) int { return int(b.CreatedAt - a.CreatedAt) })
	return matched
}

func (f *fakeUpstream) LogStat(ctx context.Context, start, end int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum int64
	for _, l := range f.matching(start, end) {
		sum += l.Quota
	}
	return sum, f.check(ctx)
}

func (f *fakeUpstream) add(entries ...usage.LogEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, entries...)
}

// spread 生成 [from, to) 内每隔 step 一条的日志，令牌在 1、2 之间轮换；每第 5 条 token_id 为 0，只带名字。
func spread(from, to time.Time, step time.Duration) []usage.LogEntry {
	var out []usage.LogEntry
	i := 0
	for at := from; at.Before(to); at = at.Add(step) {
		id := int64(1 + i%2)
		e := usage.LogEntry{CreatedAt: at.Unix(), TokenID: id, TokenName: map[int64]string{1: "a", 2: "b"}[id],
			ModelName: []string{"m1", "m2", "m3"}[i%3], Quota: int64(100 + i), PromptTokens: 10, CompletionTokens: 3}
		if i%5 == 4 {
			e.TokenID = 0
		}
		out = append(out, e)
		i++
	}
	return out
}

type harness struct {
	t   *testing.T
	s   *Syncer
	up  *fakeUpstream
	db  *sqlite.DB
	now time.Time
	log *bytes.Buffer
}

func newHarness(t *testing.T, now time.Time, up *fakeUpstream, opts ...Option) *harness {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "usage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := &harness{t: t, up: up, db: db, now: now, log: &bytes.Buffer{}}
	cfg := Config{LogInterval: time.Minute, SnapshotInterval: 5 * time.Minute, Lag: 2 * time.Minute, BackfillDays: 2,
		ReconcileAt: 3*time.Hour + 30*time.Minute, LogRetentionDays: 90, SnapshotRetentionDays: 90,
		BackupDir: filepath.Join(t.TempDir(), "backups"), BackupKeepDays: 14, Location: cst}
	opts = append([]Option{WithClock(func() time.Time { return h.now })}, opts...)
	h.s = New(up, db, cfg, slog.New(slog.NewTextHandler(h.log, nil)), opts...)
	require.NoError(t, h.s.Init(context.Background()))
	return h
}

// catchUp 反复执行 Step，直到日志追到 now - lag。
func (h *harness) catchUp() {
	h.t.Helper()
	for i := 0; ; i++ {
		require.Less(h.t, i, 1000, "没有追上")
		h.s.Step(context.Background())
		if h.s.cursor >= h.now.Add(-2*time.Minute).Unix() {
			return
		}
	}
}

func (h *harness) query(q string, args ...any) int64 {
	h.t.Helper()
	var n sql.NullInt64
	require.NoError(h.t, h.db.Read().QueryRow(q, args...).Scan(&n))
	return n.Int64
}

// assertMatchesUpstream 检查 [start, end) 内本地明细和小时汇总都与上游一致。
func (h *harness) assertMatchesUpstream(start, end int64) {
	h.t.Helper()
	want := h.up.matching(start, end-1)
	var sum int64
	for _, l := range want {
		sum += l.Quota
	}
	require.Equal(h.t, int64(len(want)), h.query(`SELECT COUNT(*) FROM usage_logs WHERE created_at >= ? AND created_at < ?`, start, end))
	require.Equal(h.t, sum, h.query(`SELECT SUM(quota) FROM usage_logs WHERE created_at >= ? AND created_at < ?`, start, end))
	require.Equal(h.t, int64(len(want)), h.query(`SELECT SUM(requests) FROM usage_hourly WHERE hour >= ? AND hour < ?`, start, end))
	require.Equal(h.t, sum, h.query(`SELECT SUM(quota) FROM usage_hourly WHERE hour >= ? AND hour < ?`, start, end))
}

func kv(t *testing.T, db *sqlite.DB, key string) string {
	t.Helper()
	v, _, err := db.GetKV(context.Background(), key)
	require.NoError(t, err)
	return v
}

func TestBackfillThenIncremental(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, cst)
	up := &fakeUpstream{tokens: []usage.Token{{ID: 1, Name: "a", Status: 1}, {ID: 2, Name: "b", Status: 1}}}
	up.add(spread(time.Date(2026, 10, 24, 0, 0, 0, 0, cst), now, 7*time.Minute)...)
	h := newHarness(t, now, up)
	require.Equal(t, "2026-10-25", kv(t, h.db, port.KeyDataSince), "今天 0 点往前 2 天")
	require.True(t, h.s.Backfilling())

	h.catchUp()
	require.False(t, h.s.Backfilling())
	backfillStart := time.Date(2026, 10, 25, 0, 0, 0, 0, cst).Unix()
	h.assertMatchesUpstream(backfillStart, now.Add(-2*time.Minute).Unix())
	require.Zero(t, h.query(`SELECT COUNT(*) FROM usage_logs WHERE created_at < ?`, backfillStart), "回填起点之前的不拉")
	require.Zero(t, h.query(`SELECT COUNT(*) FROM usage_logs WHERE token_id = 0`), "token_id 为 0 的按名字归属")
	require.NotEmpty(t, kv(t, h.db, port.KeyCurrency))
	require.Contains(t, kv(t, h.db, port.KeyAccount), `"quota":1000`)
	require.Equal(t, int64(2), h.query(`SELECT COUNT(*) FROM tokens`))

	// 5 分钟后：新日志进来，lag 以内的先不同步。
	h.now = now.Add(5 * time.Minute)
	up.add(usage.LogEntry{CreatedAt: now.Add(time.Minute).Unix(), TokenID: 1, ModelName: "m1", Quota: 7},
		usage.LogEntry{CreatedAt: h.now.Add(-time.Minute).Unix(), TokenID: 1, ModelName: "m1", Quota: 9})
	h.catchUp()
	h.assertMatchesUpstream(backfillStart, h.now.Add(-2*time.Minute).Unix())
	require.Zero(t, h.query(`SELECT COUNT(*) FROM usage_logs WHERE quota = 9`), "lag 以内的窗口还没封闭")
	require.Equal(t, fmt.Sprint(h.now.Add(-2*time.Minute).Unix()), kv(t, h.db, port.KeyLogsCursor))
}

func TestExactlyFullPage(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, cst)
	up := &fakeUpstream{}
	up.add(spread(time.Date(2026, 10, 26, 8, 0, 0, 0, cst), time.Date(2026, 10, 26, 8, 0, 0, 0, cst).Add(100*time.Second), time.Second)...)
	h := newHarness(t, now, up)
	h.catchUp()
	require.Equal(t, int64(100), h.query(`SELECT COUNT(*) FROM usage_logs`))
}

func TestWindowSplitsWhenTooManyRows(t *testing.T) {
	for name, up := range map[string]*fakeUpstream{
		"total 超过上限":           {},
		"上游每页只给 50 条":          {pageCap: 50},
		"上游每页只给 30 条且 total 准": {pageCap: 30},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 27, 10, 0, 0, 0, cst)
			day := time.Date(2026, 10, 26, 0, 0, 0, 0, cst)
			up.add(spread(day.Add(9*time.Hour), day.Add(11*time.Hour), 15*time.Second)...) // 480 条集中在 2 小时
			h := newHarness(t, now, up, WithMaxPages(2))                                   // 一个窗口最多 200 条
			h.catchUp()
			h.assertMatchesUpstream(day.AddDate(0, 0, -1).Unix(), now.Add(-2*time.Minute).Unix())
			require.Contains(t, h.log.String(), "窗口内日志太多")
		})
	}
}

func TestCancelKeepsCursor(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, cst)
	up := &fakeUpstream{}
	up.add(spread(time.Date(2026, 10, 25, 1, 0, 0, 0, cst), time.Date(2026, 10, 25, 2, 0, 0, 0, cst), 10*time.Second)...)
	h := newHarness(t, now, up)
	before := kv(t, h.db, port.KeyLogsCursor)
	ctx, cancel := context.WithCancel(context.Background())
	up.onPage = func(page int) {
		if page == 2 {
			cancel()
		}
	}
	h.s.Step(ctx)
	require.Equal(t, before, kv(t, h.db, port.KeyLogsCursor), "被中断的窗口不推进游标")
	require.Zero(t, h.query(`SELECT COUNT(*) FROM usage_logs`))
	require.NotContains(t, kv(t, h.db, port.KeyTaskPrefix+port.TaskLogs), "context canceled", "停机中断不记成失败")
}

func TestResyncGuardAndForce(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, cst)
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, cst)
	up := &fakeUpstream{}
	up.add(spread(day, day.Add(24*time.Hour), 10*time.Minute)...)
	h := newHarness(t, now, up)
	h.catchUp()
	local := h.query(`SELECT COUNT(*) FROM usage_logs WHERE created_at >= ? AND created_at < ?`, day.Unix(), day.Add(24*time.Hour).Unix())
	require.Equal(t, int64(144), local)

	// 上游清理了这一天：默认不替换，本地数据保留。
	up.logs = nil
	res, err := h.s.Resync(context.Background(), day.Add(12*time.Hour), false)
	require.NoError(t, err)
	require.Equal(t, 24, res.Skipped)
	require.Equal(t, local, h.query(`SELECT COUNT(*) FROM usage_logs`))

	res, err = h.s.Resync(context.Background(), day, true)
	require.NoError(t, err)
	require.Zero(t, res.Skipped)
	require.Zero(t, h.query(`SELECT COUNT(*) FROM usage_logs`), "-force 强制替换")
	require.Zero(t, h.query(`SELECT COUNT(*) FROM usage_hourly`))

	_, err = h.s.Resync(context.Background(), now, false)
	require.ErrorIs(t, err, ErrResyncToday)

	// 比 data_since 早的日期：拉回来并把 data_since 提前。
	early := time.Date(2026, 10, 20, 0, 0, 0, 0, cst)
	up.add(spread(early.Add(time.Hour), early.Add(2*time.Hour), 30*time.Minute)...)
	_, err = h.s.Resync(context.Background(), early, false)
	require.NoError(t, err)
	require.Equal(t, "2026-10-20", kv(t, h.db, port.KeyDataSince))
	require.Equal(t, int64(2), h.query(`SELECT COUNT(*) FROM usage_logs`))
}

func TestReconcilePicksUpLateLogs(t *testing.T) {
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, cst)
	up := &fakeUpstream{tokens: []usage.Token{{ID: 1, Name: "a", Status: 1, UsedQuota: 0}, {ID: 2, Name: "b", Status: 1}}}
	up.add(spread(day, day.Add(10*time.Hour), 10*time.Minute)...)
	h := newHarness(t, day.Add(10*time.Hour), up)
	h.catchUp()

	// 游标已经过去之后，上游才写入一条 08:00 的日志（迟到）。
	late := usage.LogEntry{CreatedAt: day.Add(8 * time.Hour).Unix(), TokenID: 1, ModelName: "m1", Quota: 5000}
	up.add(late)
	up.add(spread(day.Add(10*time.Hour), day.Add(27*time.Hour+40*time.Minute), 10*time.Minute)...)

	// 第二天 03:40：先追上日志，再对账昨天。
	h.now = day.Add(27*time.Hour + 40*time.Minute)
	h.catchUp()
	h.now = h.now.Add(dailyRetry)
	h.s.Step(context.Background())
	require.Equal(t, "2026-10-27", kv(t, h.db, port.KeyReconcileDay))

	var upstream, local int64
	var snapshot sql.NullInt64
	require.NoError(t, h.db.Read().QueryRow(`SELECT upstream_quota, local_quota, snapshot_quota FROM reconcile_reports WHERE day = '2026-10-26'`).
		Scan(&upstream, &local, &snapshot))
	require.Equal(t, upstream, local, "重拉补上了迟到的日志")
	require.Equal(t, int64(1), h.query(`SELECT COUNT(*) FROM usage_logs WHERE quota = 5000`))
	require.False(t, snapshot.Valid, "服务 10:00 才开始，当天 0 点前没有快照，无法比较")

	// 再过一天：10-27 0 点前已有快照，可以比较。
	up.add(spread(day.Add(27*time.Hour+40*time.Minute), day.Add(51*time.Hour+40*time.Minute), 10*time.Minute)...)
	h.now = day.Add(51*time.Hour + 40*time.Minute)
	h.catchUp()
	require.NoError(t, h.db.Read().QueryRow(`SELECT upstream_quota, local_quota, snapshot_quota FROM reconcile_reports WHERE day = '2026-10-27'`).
		Scan(&upstream, &local, &snapshot))
	require.Equal(t, upstream, local)
	require.True(t, snapshot.Valid)
}

func TestAuthFailurePausesUpstreamTasks(t *testing.T) {
	now := time.Date(2026, 10, 27, 4, 20, 0, 0, cst)
	up := &fakeUpstream{authErr: true}
	h := newHarness(t, now, up)
	h.s.Step(context.Background())
	require.Equal(t, 1, up.calls, "第一个任务鉴权失败后，其余访问上游的任务都不再调用")
	require.Equal(t, fmt.Sprint(now.Add(30*time.Minute).Unix()), kv(t, h.db, port.KeyAuthPausedUntil))
	require.Contains(t, kv(t, h.db, port.KeyTaskPrefix+port.TaskStatus), "上游鉴权失败")
	require.Contains(t, h.log.String(), "暂停同步 30 分钟")
	require.Equal(t, "2026-10-27", kv(t, h.db, port.KeyBackupDay), "不访问上游的备份照常执行")

	h.now = now.Add(10 * time.Minute)
	h.s.Step(context.Background())
	require.Equal(t, 1, up.calls, "暂停期间不重试轰炸")

	up.authErr = false
	h.now = now.Add(31 * time.Minute)
	h.catchUp()
	var st port.TaskState
	require.NoError(t, json.Unmarshal([]byte(kv(t, h.db, port.KeyTaskPrefix+port.TaskStatus)), &st))
	require.Empty(t, st.LastError, "恢复后错误清空")
	require.Equal(t, h.now.Unix(), st.LastOKAt)
}

func TestTokenPageFailureDoesNotMarkDeleted(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, cst)
	up := &fakeUpstream{tokens: []usage.Token{{ID: 1, Name: "a", Status: 1}, {ID: 2, Name: "b", Status: 1}}}
	h := newHarness(t, now, up)
	h.catchUp()
	up.failTokenPage = true
	h.now = now.Add(5 * time.Minute)
	h.s.Step(context.Background())
	require.Zero(t, h.query(`SELECT COUNT(*) FROM tokens WHERE deleted_at IS NOT NULL`))
	require.Contains(t, kv(t, h.db, port.KeyTaskPrefix+port.TaskSnapshot), "不标删除")
}

func TestSnapshotSlotsAlignToFiveMinutes(t *testing.T) {
	start := time.Date(2026, 10, 27, 9, 58, 30, 0, cst)
	up := &fakeUpstream{tokens: []usage.Token{{ID: 1, Name: "a", Status: 1, UsedQuota: 1}}}
	h := newHarness(t, start, up)
	h.catchUp()
	up.tokens[0].UsedQuota = 2
	h.now = time.Date(2026, 10, 27, 10, 0, 3, 0, cst) // 按时执行，晚了 3 秒
	h.s.Step(context.Background())
	up.tokens[0].UsedQuota = 3
	h.now = time.Date(2026, 10, 27, 10, 7, 0, 0, cst) // 10:05 那次晚了 2 分钟，记实际时间
	h.s.Step(context.Background())

	rows, err := h.db.Read().Query(`SELECT ts FROM token_snapshots ORDER BY ts`)
	require.NoError(t, err)
	defer rows.Close()
	var got []time.Time
	for rows.Next() {
		var ts int64
		require.NoError(t, rows.Scan(&ts))
		got = append(got, time.Unix(ts, 0).In(cst))
	}
	require.Equal(t, []time.Time{start, time.Date(2026, 10, 27, 10, 0, 0, 0, cst), h.now}, got)
}

func TestReconcileWaitsForBackfill(t *testing.T) {
	now := time.Date(2026, 10, 27, 4, 15, 0, 0, cst) // 对账、清理、备份都已到点
	up := &fakeUpstream{}
	up.add(spread(now.Add(-40*time.Hour), now, 10*time.Minute)...)
	h := newHarness(t, now, up)
	h.s.Step(context.Background()) // 只推进了一个窗口（回填中）
	require.True(t, h.s.Backfilling())
	require.Empty(t, kv(t, h.db, port.KeyReconcileDay))
	require.Empty(t, kv(t, h.db, port.KeyTaskPrefix+port.TaskReconcile), "没执行的任务不写状态")
	require.Empty(t, kv(t, h.db, port.KeyTaskPrefix+port.TaskPurge))
	require.Equal(t, "2026-10-27", kv(t, h.db, port.KeyBackupDay), "备份不受回填影响")

	h.catchUp()
	h.now = h.now.Add(dailyRetry)
	h.s.Step(context.Background())
	require.Equal(t, "2026-10-27", kv(t, h.db, port.KeyReconcileDay), "追上之后再对账")
	require.Equal(t, "2026-10-27", kv(t, h.db, port.KeyPurgeDay))
}

func TestPurgeWaitsForBackfill(t *testing.T) {
	now := time.Date(2026, 10, 27, 4, 5, 0, 0, cst)
	up := &fakeUpstream{}
	h := newHarness(t, now, up)
	require.True(t, h.s.Backfilling())
	require.ErrorIs(t, h.s.purge(context.Background(), now), errNotYet)
	h.catchUp()
	require.NoError(t, h.s.purge(context.Background(), now))
}

func TestAuthFailureAlerts(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, cst)
	up := &fakeUpstream{authErr: true}
	alerts := &fakeAlerter{}
	h := newHarness(t, now, up, WithAlerter(alerts))
	h.s.Step(context.Background())
	require.Equal(t, []string{alertAuth}, alerts.raised())
	require.Contains(t, alerts.last(alertAuth).Text, "upstream.access_token")

	up.authErr = false
	h.now = now.Add(31 * time.Minute)
	h.catchUp()
	require.True(t, alerts.last(alertAuth).Resolved, "恢复后发恢复通知")
	require.Equal(t, []string{alertAuth}, alerts.raised(), "回填期间落后不告警")
}

func TestLagAlerts(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, cst)
	up := &fakeUpstream{}
	alerts := &fakeAlerter{}
	h := newHarness(t, now, up, WithAlerter(alerts))
	h.catchUp()
	require.Empty(t, alerts.raised())

	up.failLogs = true
	h.now = now.Add(20 * time.Minute)
	h.s.Step(context.Background())
	require.Empty(t, alerts.raised(), "落后 22 分钟，还没到 30 分钟")
	h.now = now.Add(35 * time.Minute)
	h.s.Step(context.Background())
	require.Equal(t, []string{alertLag}, alerts.raised())
	require.Contains(t, alerts.last(alertLag).Text, "日志同步已落后 37 分钟")
	require.Contains(t, alerts.last(alertLag).Text, "上游返回 502")

	up.failLogs = false
	h.now = now.Add(36 * time.Minute)
	h.catchUp()
	require.True(t, alerts.last(alertLag).Resolved)
}
