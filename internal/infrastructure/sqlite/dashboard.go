package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// Dashboard 是看板的只读查询，只用读库：同步器的写事务不会卡住页面。
type Dashboard struct{ r *sql.DB }

var _ port.DashboardStore = (*Dashboard)(nil)

// Dashboard 返回看板查询。
func (d *DB) Dashboard() *Dashboard { return &Dashboard{r: d.r} }

func (q *Dashboard) GetKV(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := q.r.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取 kv %s: %w", key, err)
	}
	return v, true, nil
}

const tokenColumns = `id, name, COALESCE(key_hint, ''), status, COALESCE(group_name, ''), unlimited, remain_quota,
	used_quota, COALESCE(expired_time, 0), COALESCE(created_time, 0), COALESCE(accessed_time, 0),
	COALESCE(model_limits, ''), first_seen_at, updated_at, COALESCE(deleted_at, 0)`

type scanner interface{ Scan(dest ...any) error }

func scanToken(s scanner) (usage.TokenRecord, error) {
	var t usage.TokenRecord
	err := s.Scan(&t.ID, &t.Name, &t.KeyHint, &t.Status, &t.Group, &t.Unlimited, &t.RemainQuota, &t.UsedQuota,
		&t.ExpiredTime, &t.CreatedTime, &t.AccessedTime, &t.ModelLimits, &t.FirstSeenAt, &t.UpdatedAt, &t.DeletedAt)
	return t, err
}

func (q *Dashboard) Tokens(ctx context.Context) ([]usage.TokenRecord, error) {
	rows, err := q.r.QueryContext(ctx, `SELECT `+tokenColumns+` FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usage.TokenRecord
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (q *Dashboard) Token(ctx context.Context, id int64) (usage.TokenRecord, bool, error) {
	t, err := scanToken(q.r.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

// Usage 汇总 usage_hourly。按天分组时用整数运算算出当地 0 点：((hour + offset) / 86400) * 86400 - offset，
// 不用 SQL 日期函数（§3）。
func (q *Dashboard) Usage(ctx context.Context, uq port.UsageQuery) ([]port.UsageGroup, error) {
	var args []any
	bucket := "0"
	switch uq.Bucket {
	case port.BucketHour:
		bucket = "hour"
	case port.BucketDay:
		bucket = "((hour + ?) / 86400) * 86400 - ?"
		args = append(args, uq.TZOffset, uq.TZOffset)
	}
	token, model := "0", "''"
	if uq.ByToken {
		token = "token_id"
	}
	if uq.ByModel {
		model = "model_name"
	}
	where := "hour >= ? AND hour < ?"
	args = append(args, uq.Start, uq.End)
	if uq.TokenID != nil {
		where += " AND token_id = ?"
		args = append(args, *uq.TokenID)
	}
	rows, err := q.r.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s AS b, %s AS t, %s AS m, SUM(requests), SUM(quota), SUM(prompt_tokens), SUM(completion_tokens),
			SUM(cache_read_tokens), SUM(cache_write_tokens)
		FROM usage_hourly WHERE %s
		GROUP BY b, t, m ORDER BY b, t, m`, bucket, token, model, where), args...)
	if err != nil {
		return nil, fmt.Errorf("汇总用量: %w", err)
	}
	defer rows.Close()
	var out []port.UsageGroup
	for rows.Next() {
		var g port.UsageGroup
		if err := rows.Scan(&g.Bucket, &g.TokenID, &g.Model, &g.Requests, &g.Quota, &g.PromptTokens,
			&g.CompletionTokens, &g.CacheReadTokens, &g.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (q *Dashboard) Logs(ctx context.Context, lq port.LogQuery) ([]usage.LogEntry, int64, error) {
	where := "token_id = ?"
	args := []any{lq.TokenID}
	if lq.Model != "" {
		where += " AND model_name = ?"
		args = append(args, lq.Model)
	}
	if lq.Start > 0 {
		where += " AND created_at >= ?"
		args = append(args, lq.Start)
	}
	if lq.End > 0 {
		where += " AND created_at < ?"
		args = append(args, lq.End)
	}
	var total int64
	if err := q.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计请求数: %w", err)
	}
	rows, err := q.r.QueryContext(ctx, `
		SELECT created_at, token_id, COALESCE(token_name, ''), model_name, quota, COALESCE(prompt_tokens, 0),
			COALESCE(completion_tokens, 0), COALESCE(cache_read_tokens, 0), COALESCE(cache_write_tokens, 0),
			COALESCE(use_time, 0), COALESCE(frt_ms, 0), COALESCE(is_stream, 0), COALESCE(group_name, ''),
			COALESCE(request_id, '')
		FROM usage_logs WHERE `+where+` ORDER BY created_at DESC, pk DESC LIMIT ? OFFSET ?`,
		append(args, lq.Limit, lq.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询请求: %w", err)
	}
	defer rows.Close()
	var out []usage.LogEntry
	for rows.Next() {
		var e usage.LogEntry
		if err := rows.Scan(&e.CreatedAt, &e.TokenID, &e.TokenName, &e.ModelName, &e.Quota, &e.PromptTokens,
			&e.CompletionTokens, &e.CacheReadTokens, &e.CacheWriteTokens, &e.UseTime, &e.FRTMillis, &e.IsStream,
			&e.Group, &e.RequestID); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func (q *Dashboard) LatestReconcile(ctx context.Context) (*usage.ReconcileReport, error) {
	var r usage.ReconcileReport
	var upstream, local, snapshot, checked sql.NullInt64
	err := q.r.QueryRowContext(ctx, `
		SELECT day, upstream_quota, local_quota, snapshot_quota, checked_at FROM reconcile_reports ORDER BY day DESC LIMIT 1`).
		Scan(&r.Day, &upstream, &local, &snapshot, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.UpstreamQuota, r.LocalQuota, r.CheckedAt = upstream.Int64, local.Int64, checked.Int64
	if snapshot.Valid {
		r.SnapshotQuota = &snapshot.Int64
	}
	return &r, nil
}
