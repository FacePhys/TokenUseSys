package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// seedUsage 在 hour0（当地 0 点）开始的两天里写入明细：令牌 1、2 和未归属（0），两个模型。
func seedUsage(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	entries := []usage.LogEntry{
		entry(hour0+10, 1, "m1", 100),           // 第 1 天 00 点
		entry(hour0+20, 1, "m2", 50),            // 第 1 天 00 点
		entry(hour0+3*3600, 2, "m1", 7),         // 第 1 天 03 点
		entry(hour0+86400+60, 1, "m1", 1000),    // 第 2 天 00 点
		entry(hour0+86400+5*3600, 0, "m1", 3),   // 第 2 天 05 点，未归属
		entry(hour0+86400+23*3600, 2, "m2", 11), // 第 2 天 23 点
	}
	require.NoError(t, db.ReplaceWindow(ctx, hour0, hour0+2*86400, entries, nil))
}

func TestDashboardUsageGrouping(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedUsage(t, db)
	q := db.Dashboard()
	const offset = 8 * 3600

	byToken, err := q.Usage(ctx, port.UsageQuery{Start: hour0, End: hour0 + 2*86400, ByToken: true})
	require.NoError(t, err)
	require.Len(t, byToken, 3)
	require.Equal(t, int64(0), byToken[0].TokenID)
	require.Equal(t, int64(1150), byToken[1].Quota)
	require.Equal(t, int64(3), byToken[1].Requests)
	require.Equal(t, int64(18), byToken[2].Quota)

	byDay, err := q.Usage(ctx, port.UsageQuery{Start: hour0, End: hour0 + 2*86400, Bucket: port.BucketDay, TZOffset: offset})
	require.NoError(t, err)
	require.Len(t, byDay, 2)
	require.Equal(t, hour0, byDay[0].Bucket, "按当地 0 点分组")
	require.Equal(t, int64(157), byDay[0].Quota)
	require.Equal(t, hour0+86400, byDay[1].Bucket)
	require.Equal(t, int64(1014), byDay[1].Quota, "23 点那条也算第 2 天")

	tok := int64(1)
	models, err := q.Usage(ctx, port.UsageQuery{Start: hour0, End: hour0 + 2*86400, ByModel: true, TokenID: &tok})
	require.NoError(t, err)
	require.Equal(t, []string{"m1", "m2"}, []string{models[0].Model, models[1].Model})
	require.Equal(t, int64(1100), models[0].Quota)

	hourly, err := q.Usage(ctx, port.UsageQuery{Start: hour0, End: hour0 + 86400, Bucket: port.BucketHour, ByToken: true, ByModel: true})
	require.NoError(t, err)
	require.Len(t, hourly, 3)
	require.Equal(t, hour0+3*3600, hourly[2].Bucket)

	none, err := q.Usage(ctx, port.UsageQuery{Start: hour0 + 10*86400, End: hour0 + 11*86400, ByToken: true})
	require.NoError(t, err)
	require.Empty(t, none, "没有数据时不返回空行")
}

func TestDashboardLogsAndTokens(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedUsage(t, db)
	now := time.Unix(hour0, 0)
	require.NoError(t, db.SyncTokens(ctx, []usage.Token{{ID: 1, Name: "a", Status: 1, KeyHint: "sk-AbCd…9xYz", ExpiredTime: -1},
		{ID: 2, Name: "b", Status: 2}}, true, now, now))
	q := db.Dashboard()

	logs, total, err := q.Logs(ctx, port.LogQuery{TokenID: 1, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, logs, 2)
	require.Equal(t, hour0+86400+60, logs[0].CreatedAt, "按时间倒序")
	require.True(t, logs[0].IsStream)
	logs, _, err = q.Logs(ctx, port.LogQuery{TokenID: 1, Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, logs, 1)
	logs, total, err = q.Logs(ctx, port.LogQuery{TokenID: 1, Model: "m2", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	_, total, err = q.Logs(ctx, port.LogQuery{TokenID: 1, Start: hour0 + 86400, End: hour0 + 2*86400, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)

	tokens, err := q.Tokens(ctx)
	require.NoError(t, err)
	require.Len(t, tokens, 2)
	require.Equal(t, "sk-AbCd…9xYz", tokens[0].KeyHint)
	require.Equal(t, int64(-1), tokens[0].ExpiredTime)
	require.Equal(t, "enabled", tokens[0].StatusLabel())
	require.Equal(t, "disabled", tokens[1].StatusLabel())
	_, ok, err := q.Token(ctx, 9)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestLatestReconcile(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	r, err := db.Dashboard().LatestReconcile(ctx)
	require.NoError(t, err)
	require.Nil(t, r)
	snap := int64(9)
	require.NoError(t, db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-25", UpstreamQuota: 1, LocalQuota: 1}))
	require.NoError(t, db.SaveReconcile(ctx, usage.ReconcileReport{Day: "2026-10-26", UpstreamQuota: 5, LocalQuota: 4, SnapshotQuota: &snap}))
	r, err = db.Dashboard().LatestReconcile(ctx)
	require.NoError(t, err)
	require.Equal(t, "2026-10-26", r.Day)
	require.Equal(t, int64(9), *r.SnapshotQuota)
}
