package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// hour0 是一个整点（2026-10-27 00:00 +08:00）。
const hour0 = int64(1792512000 + 6*86400)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "data", "usage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func entry(at int64, tokenID int64, model string, quota int64) usage.LogEntry {
	return usage.LogEntry{CreatedAt: at, TokenID: tokenID, TokenName: "t", ModelName: model, Quota: quota,
		PromptTokens: 10, CompletionTokens: 2, CacheReadTokens: 3, IsStream: true}
}

type hourly struct {
	Hour, TokenID      int64
	Model              string
	Requests, Quota    int64
	Prompt, Completion int64
	CacheRead          int64
}

func hourlyRows(t *testing.T, db *DB) []hourly {
	t.Helper()
	rows, err := db.w.Query(`SELECT hour, token_id, model_name, requests, quota, prompt_tokens, completion_tokens,
		cache_read_tokens FROM usage_hourly ORDER BY hour, token_id, model_name`)
	require.NoError(t, err)
	defer rows.Close()
	var out []hourly
	for rows.Next() {
		var h hourly
		require.NoError(t, rows.Scan(&h.Hour, &h.TokenID, &h.Model, &h.Requests, &h.Quota, &h.Prompt, &h.Completion, &h.CacheRead))
		out = append(out, h)
	}
	return out
}

func count(t *testing.T, db *DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.w.QueryRow(query, args...).Scan(&n))
	return n
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, db.SetKV(context.Background(), "k", "v"))
	require.NoError(t, db.Close())

	db, err = Open(context.Background(), path)
	require.NoError(t, err, "已执行过的迁移不再执行")
	defer db.Close()
	v, ok, err := db.GetKV(context.Background(), "k")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "v", v)
	_, ok, err = db.GetKV(context.Background(), "missing")
	require.NoError(t, err)
	require.False(t, ok)

	var mode string
	require.NoError(t, db.r.QueryRow(`PRAGMA journal_mode`).Scan(&mode))
	require.Equal(t, "wal", mode)
}

func TestReplaceWindowRecomputesWholeHours(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	// 第一个窗口 [00:00, 00:30)，第二个窗口 [00:30, 01:10) 跨到下一个小时。
	require.NoError(t, db.ReplaceWindow(ctx, hour0, hour0+1800, []usage.LogEntry{
		entry(hour0+10, 1, "m1", 100), entry(hour0+20, 1, "m1", 50), entry(hour0+30, 2, "m2", 7),
	}, nil))
	cursor := hour0 + 4200
	require.NoError(t, db.ReplaceWindow(ctx, hour0+1800, cursor, []usage.LogEntry{
		entry(hour0+1900, 1, "m1", 1), entry(hour0+3700, 1, "m1", 5),
	}, &cursor))

	require.Equal(t, []hourly{
		{hour0, 1, "m1", 3, 151, 30, 6, 9},
		{hour0, 2, "m2", 1, 7, 10, 2, 3},
		{hour0 + 3600, 1, "m1", 1, 5, 10, 2, 3},
	}, hourlyRows(t, db), "同一小时里前一个窗口的明细一起汇总")
	v, _, err := db.GetKV(ctx, port.KeyLogsCursor)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprint(cursor), v)

	// 同一窗口重复同步，结果不变；条数变少时汇总跟着变。
	require.NoError(t, db.ReplaceWindow(ctx, hour0+1800, cursor, []usage.LogEntry{
		entry(hour0+1900, 1, "m1", 1), entry(hour0+3700, 1, "m1", 5),
	}, &cursor))
	require.Equal(t, int64(5), count(t, db, `SELECT COUNT(*) FROM usage_logs`))
	require.NoError(t, db.ReplaceWindow(ctx, hour0+1800, cursor, nil, &cursor))
	require.Equal(t, int64(3), count(t, db, `SELECT COUNT(*) FROM usage_logs`))
	require.Equal(t, []hourly{
		{hour0, 1, "m1", 2, 150, 20, 4, 6},
		{hour0, 2, "m2", 1, 7, 10, 2, 3},
	}, hourlyRows(t, db))
}

func TestReplaceWindowRejectsEntriesOutsideWindow(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	cursor := hour0 + 60
	err := db.ReplaceWindow(ctx, hour0, hour0+60, []usage.LogEntry{entry(hour0+60, 1, "m", 1)}, &cursor)
	require.ErrorContains(t, err, "不在窗口")
	_, ok, _ := db.GetKV(ctx, port.KeyLogsCursor)
	require.False(t, ok, "失败时游标不动")
}

func TestReplaceWindowRollsBackOnCancel(t *testing.T) {
	db := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cursor := hour0 + 60
	require.Error(t, db.ReplaceWindow(ctx, hour0, hour0+60, []usage.LogEntry{entry(hour0, 1, "m", 1)}, &cursor))
	_, ok, err := db.GetKV(context.Background(), port.KeyLogsCursor)
	require.NoError(t, err)
	require.False(t, ok)
	require.Zero(t, count(t, db, `SELECT COUNT(*) FROM usage_logs`))
}

func TestHasLocalData(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	require.NoError(t, db.ReplaceWindow(ctx, hour0, hour0+3600, []usage.LogEntry{entry(hour0+100, 1, "m", 1)}, nil))
	has, err := db.HasLocalData(ctx, hour0, hour0+3600)
	require.NoError(t, err)
	require.True(t, has)
	has, err = db.HasLocalData(ctx, hour0+3600, hour0+7200)
	require.NoError(t, err)
	require.False(t, has)

	// 明细过了保留期被清理，只剩小时汇总，也算有数据。
	_, _, err = db.Purge(ctx, hour0+3600, 0)
	require.NoError(t, err)
	has, err = db.HasLocalData(ctx, hour0, hour0+3600)
	require.NoError(t, err)
	require.True(t, has)
	has, err = db.HasLocalData(ctx, hour0+60, hour0+120)
	require.NoError(t, err)
	require.False(t, has, "只覆盖小时的一部分时不看小时汇总")
}

func TestSyncTokensSnapshotsOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	slot := time.Unix(hour0, 0)
	tokens := []usage.Token{
		{ID: 1, Name: "a", KeyHint: "sk-AbCd…9xYz", Status: 1, UsedQuota: 10, RemainQuota: 90},
		{ID: 2, Name: "b", Status: 1, UsedQuota: 0, Unlimited: true},
	}
	require.NoError(t, db.SyncTokens(ctx, tokens, true, slot, slot))
	require.Equal(t, int64(2), count(t, db, `SELECT COUNT(*) FROM token_snapshots`))

	// 没有变化：不写快照。
	require.NoError(t, db.SyncTokens(ctx, tokens, true, slot.Add(5*time.Minute), slot.Add(5*time.Minute)))
	require.Equal(t, int64(2), count(t, db, `SELECT COUNT(*) FROM token_snapshots`))

	// used_quota 变化、key 这次没返回：写快照，key_hint 保留原来的。
	tokens[0].UsedQuota, tokens[0].KeyHint = 15, ""
	require.NoError(t, db.SyncTokens(ctx, tokens, true, slot.Add(10*time.Minute), slot.Add(10*time.Minute)))
	require.Equal(t, int64(3), count(t, db, `SELECT COUNT(*) FROM token_snapshots`))
	var hint string
	require.NoError(t, db.w.QueryRow(`SELECT key_hint FROM tokens WHERE id = 1`).Scan(&hint))
	require.Equal(t, "sk-AbCd…9xYz", hint)

	// 某页失败（不完整）时不标删除；完整时标删除；重新出现时清掉。
	require.NoError(t, db.SyncTokens(ctx, tokens[:1], false, slot.Add(15*time.Minute), slot.Add(15*time.Minute)))
	require.Zero(t, count(t, db, `SELECT COUNT(*) FROM tokens WHERE deleted_at IS NOT NULL`))
	require.NoError(t, db.SyncTokens(ctx, tokens[:1], true, slot.Add(20*time.Minute), slot.Add(20*time.Minute)))
	require.Equal(t, int64(2), count(t, db, `SELECT id FROM tokens WHERE deleted_at IS NOT NULL`))
	require.NoError(t, db.SyncTokens(ctx, tokens, true, slot.Add(25*time.Minute), slot.Add(25*time.Minute)))
	require.Zero(t, count(t, db, `SELECT COUNT(*) FROM tokens WHERE deleted_at IS NOT NULL`))

	// 上游一个令牌都没有时全部标删除。
	require.NoError(t, db.SyncTokens(ctx, nil, true, slot.Add(30*time.Minute), slot.Add(30*time.Minute)))
	require.Equal(t, int64(2), count(t, db, `SELECT COUNT(*) FROM tokens WHERE deleted_at IS NOT NULL`))
}

func TestEnsureTokensAndNames(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(hour0, 0)
	require.NoError(t, db.SyncTokens(ctx, []usage.Token{{ID: 1, Name: "a", Status: 1}, {ID: 2, Name: "dup", Status: 1},
		{ID: 3, Name: "dup", Status: 2}}, true, now, now))
	require.NoError(t, db.EnsureTokens(ctx, []usage.LogEntry{
		{TokenID: 1, TokenName: "renamed"}, {TokenID: 9, TokenName: "gone"}, {TokenID: 10}, {TokenID: 0, TokenName: "x"},
	}, now))
	var name string
	var status int
	require.NoError(t, db.w.QueryRow(`SELECT name, status FROM tokens WHERE id = 1`).Scan(&name, &status))
	require.Equal(t, "a", name, "已有的令牌不动")
	require.NoError(t, db.w.QueryRow(`SELECT name, status FROM tokens WHERE id = 9`).Scan(&name, &status))
	require.Equal(t, "gone", name)
	require.Equal(t, usage.StatusUnknown, status)
	require.NoError(t, db.w.QueryRow(`SELECT name FROM tokens WHERE id = 10`).Scan(&name))
	require.Equal(t, "#10", name)
	require.Equal(t, int64(1), count(t, db, `SELECT COUNT(*) FROM tokens WHERE id = 9 AND deleted_at IS NOT NULL`))

	byName, err := db.TokenIDsByName(ctx)
	require.NoError(t, err)
	require.Equal(t, []int64{1}, byName["a"])
	require.Equal(t, []int64{2, 3}, byName["dup"])
}

func TestSnapshotQuotaDelta(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	day := hour0
	at := func(offset int64) time.Time { return time.Unix(day+offset, 0) }
	_, ok, err := db.SnapshotQuotaDelta(ctx, day, day+86400)
	require.NoError(t, err)
	require.False(t, ok, "还没有快照")

	require.NoError(t, db.SyncTokens(ctx, []usage.Token{{ID: 1, UsedQuota: 100}}, true, at(-300), at(-300)))
	require.NoError(t, db.SyncTokens(ctx, []usage.Token{{ID: 1, UsedQuota: 150}, {ID: 2, UsedQuota: 40}}, true, at(3600), at(3600)))
	require.NoError(t, db.SyncTokens(ctx, []usage.Token{{ID: 1, UsedQuota: 170}, {ID: 2, UsedQuota: 40}}, true, at(86400), at(86400)))
	require.NoError(t, db.SyncTokens(ctx, []usage.Token{{ID: 1, UsedQuota: 999}, {ID: 2, UsedQuota: 40}}, true, at(86700), at(86700)))

	delta, ok, err := db.SnapshotQuotaDelta(ctx, day, day+86400)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(70+40), delta, "令牌 1 从 100 到 170；令牌 2 当天新建，按 0 起算；0 点之后的快照不算")
}

func TestPurgeKeepsLatestSnapshot(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	require.NoError(t, db.ReplaceWindow(ctx, hour0, hour0+7200, []usage.LogEntry{
		entry(hour0+10, 1, "m", 1), entry(hour0+3700, 1, "m", 2),
	}, nil))
	for i, used := range []int64{1, 2, 3} {
		slot := time.Unix(hour0+int64(i)*3600, 0)
		require.NoError(t, db.SyncTokens(ctx, []usage.Token{{ID: 1, UsedQuota: used}, {ID: 2, UsedQuota: 5}}, true, slot, slot))
	}
	logs, snapshots, err := db.Purge(ctx, hour0+3600, hour0+3*3600)
	require.NoError(t, err)
	require.Equal(t, int64(1), logs)
	require.Equal(t, int64(2), snapshots, "令牌 1 删 2 行，令牌 2 只有一行，是最新的，保留")
	require.Equal(t, int64(2), count(t, db, `SELECT COUNT(*) FROM token_snapshots`))
	require.Equal(t, int64(2), count(t, db, `SELECT COUNT(*) FROM usage_hourly`), "小时汇总永久保留")
}

func TestSaveReconcileUpserts(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	snap := int64(5)
	require.NoError(t, db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-26", UpstreamQuota: 1, LocalQuota: 2, CheckedAt: 3}))
	require.NoError(t, db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-26", UpstreamQuota: 4, LocalQuota: 4, SnapshotQuota: &snap, CheckedAt: 9}))
	var up, local, snapshot int64
	require.NoError(t, db.w.QueryRow(`SELECT upstream_quota, local_quota, snapshot_quota FROM reconcile_reports`).Scan(&up, &local, &snapshot))
	require.Equal(t, [3]int64{4, 4, 5}, [3]int64{up, local, snapshot})
}

func TestBackup(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	require.NoError(t, db.SetKV(ctx, "k", "v"))
	dir := filepath.Join(t.TempDir(), "backups")
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 27, 4, 10, 0, 0, loc)

	require.NoError(t, os.MkdirAll(dir, 0o750))
	old := filepath.Join(dir, "usage-20261012-041000.db")
	kept := filepath.Join(dir, "usage-20261014-041000.db")
	stale := filepath.Join(dir, "usage-20261026-041000.db.tmp")
	other := filepath.Join(dir, "notes.txt")
	for _, f := range []string{old, kept, stale, other} {
		require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
	}
	require.NoError(t, CleanupTempBackups(dir))
	require.NoFileExists(t, stale)

	path, err := db.Backup(ctx, dir, now, 14)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "usage-20261027-041000.db"), path)
	_, err = db.Backup(ctx, dir, now.Add(time.Minute), 14)
	require.NoError(t, err, "同一天再备份一次不冲突")
	require.NoFileExists(t, old, "超过 14 天的删除")
	require.FileExists(t, kept)
	require.FileExists(t, other, "不是本程序生成的文件不动")

	// 备份可以直接打开。
	restored, err := Open(ctx, path)
	require.NoError(t, err)
	defer restored.Close()
	v, ok, err := restored.GetKV(ctx, "k")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "v", v)
}
