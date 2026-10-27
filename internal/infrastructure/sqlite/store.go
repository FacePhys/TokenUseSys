package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

func (d *DB) GetKV(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := d.w.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取 kv %s: %w", key, err)
	}
	return v, true, nil
}

func (d *DB) SetKV(ctx context.Context, key, value string) error {
	return setKV(ctx, d.w, key, value)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func setKV(ctx context.Context, db execer, key, value string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, key, value)
	if err != nil {
		return fmt.Errorf("写入 kv %s: %w", key, err)
	}
	return nil
}

// SyncTokens 写入一轮令牌列表（§4.4）。
func (d *DB) SyncTokens(ctx context.Context, tokens []usage.Token, complete bool, slot, now time.Time) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// key_hint 为空（上游这次没返回 key）时保留原来的；重新出现的令牌清掉 deleted_at。
	upsert, err := tx.PrepareContext(ctx, `
		INSERT INTO tokens (id, name, key_hint, status, group_name, unlimited, remain_quota, used_quota,
			expired_time, created_time, accessed_time, model_limits, first_seen_at, updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			key_hint = COALESCE(NULLIF(excluded.key_hint, ''), tokens.key_hint),
			status = excluded.status,
			group_name = excluded.group_name,
			unlimited = excluded.unlimited,
			remain_quota = excluded.remain_quota,
			used_quota = excluded.used_quota,
			expired_time = excluded.expired_time,
			created_time = excluded.created_time,
			accessed_time = excluded.accessed_time,
			model_limits = excluded.model_limits,
			updated_at = excluded.updated_at,
			deleted_at = NULL`)
	if err != nil {
		return err
	}
	defer upsert.Close()
	lastSnapshot, err := tx.PrepareContext(ctx, `SELECT used_quota, status FROM token_snapshots WHERE token_id = ? ORDER BY ts DESC LIMIT 1`)
	if err != nil {
		return err
	}
	defer lastSnapshot.Close()
	snapshot, err := tx.PrepareContext(ctx, `
		INSERT INTO token_snapshots (token_id, ts, used_quota, remain_quota, status) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (token_id, ts) DO UPDATE SET
			used_quota = excluded.used_quota, remain_quota = excluded.remain_quota, status = excluded.status`)
	if err != nil {
		return err
	}
	defer snapshot.Close()

	ts := now.Unix()
	ids := make([]any, 0, len(tokens))
	for _, t := range tokens {
		ids = append(ids, t.ID)
		if _, err := upsert.ExecContext(ctx, t.ID, t.Name, t.KeyHint, t.Status, t.Group, t.Unlimited, t.RemainQuota,
			t.UsedQuota, t.ExpiredTime, t.CreatedTime, t.AccessedTime, t.ModelLimits, ts, ts); err != nil {
			return fmt.Errorf("写入令牌 %d: %w", t.ID, err)
		}
		// 快照只在 used_quota 或 status 变化时写（§4.4）。
		var used, status int64
		err := lastSnapshot.QueryRowContext(ctx, t.ID).Scan(&used, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) || used != t.UsedQuota || status != int64(t.Status) {
			if _, err := snapshot.ExecContext(ctx, t.ID, slot.Unix(), t.UsedQuota, t.RemainQuota, t.Status); err != nil {
				return fmt.Errorf("写入令牌 %d 的快照: %w", t.ID, err)
			}
		}
	}
	if complete {
		query := `UPDATE tokens SET deleted_at = ? WHERE deleted_at IS NULL`
		if len(ids) > 0 {
			query += ` AND id NOT IN (?` + strings.Repeat(", ?", len(ids)-1) + `)`
		}
		if _, err := tx.ExecContext(ctx, query, append([]any{ts}, ids...)...); err != nil {
			return fmt.Errorf("标记已删除的令牌: %w", err)
		}
	}
	return tx.Commit()
}

// EnsureTokens 为只在日志里见过的令牌建占位行：状态未知（0），视为已删除。已有的不动；
// 之后令牌列表里出现时，SyncTokens 会补全信息并清掉 deleted_at。
func (d *DB) EnsureTokens(ctx context.Context, entries []usage.LogEntry, now time.Time) error {
	seen := map[int64]bool{}
	for _, e := range entries {
		if e.TokenID == 0 || seen[e.TokenID] {
			continue
		}
		seen[e.TokenID] = true
		name := e.TokenName
		if name == "" {
			name = fmt.Sprintf("#%d", e.TokenID)
		}
		ts := now.Unix()
		if _, err := d.w.ExecContext(ctx, `
			INSERT INTO tokens (id, name, status, unlimited, remain_quota, used_quota, first_seen_at, updated_at, deleted_at)
			VALUES (?, ?, ?, 0, 0, 0, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			e.TokenID, name, usage.StatusUnknown, ts, ts, ts); err != nil {
			return fmt.Errorf("写入占位令牌 %d: %w", e.TokenID, err)
		}
	}
	return nil
}

func (d *DB) TokenIDsByName(ctx context.Context) (map[string][]int64, error) {
	rows, err := d.w.QueryContext(ctx, `SELECT id, name FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string][]int64{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		m[name] = append(m[name], id)
	}
	return m, rows.Err()
}

// HasLocalData 看 [start, end) 内有没有明细，或者有没有完全落在窗口里的小时汇总。
// 明细超过保留期被清理后只剩小时汇总，也算有数据，防止用上游的空结果把它清零（§4.3）。
func (d *DB) HasLocalData(ctx context.Context, start, end int64) (bool, error) {
	first, last := usage.WholeHours(start, end)
	var has bool
	err := d.w.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM usage_logs WHERE created_at >= ? AND created_at < ?)
		    OR EXISTS (SELECT 1 FROM usage_hourly WHERE hour >= ? AND hour < ?)`,
		start, end, first, last).Scan(&has)
	return has, err
}

// ReplaceWindow 整段替换 [start, end) 的明细（§4.3），并在同一个事务里重算涉及的小时汇总、推进游标。
// ctx 取消时事务回滚，游标不动。
func (d *DB) ReplaceWindow(ctx context.Context, start, end int64, entries []usage.LogEntry, cursor *int64) error {
	if end <= start {
		return fmt.Errorf("窗口 [%d, %d) 无效", start, end)
	}
	for _, e := range entries {
		if e.CreatedAt < start || e.CreatedAt >= end {
			return fmt.Errorf("日志时间 %d 不在窗口 [%d, %d) 内", e.CreatedAt, start, end)
		}
	}
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_logs WHERE created_at >= ? AND created_at < ?`, start, end); err != nil {
		return fmt.Errorf("删除窗口内的明细: %w", err)
	}
	insert, err := tx.PrepareContext(ctx, `
		INSERT INTO usage_logs (created_at, token_id, token_name, model_name, quota, prompt_tokens, completion_tokens,
			cache_read_tokens, cache_write_tokens, use_time, frt_ms, is_stream, group_name, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, e := range entries {
		if _, err := insert.ExecContext(ctx, e.CreatedAt, e.TokenID, e.TokenName, e.ModelName, e.Quota, e.PromptTokens,
			e.CompletionTokens, e.CacheReadTokens, e.CacheWriteTokens, e.UseTime, e.FRTMillis, e.IsStream, e.Group,
			e.RequestID); err != nil {
			return fmt.Errorf("写入明细: %w", err)
		}
	}
	// 重算窗口涉及的整点：窗口不一定对齐整点，同一小时里之前窗口的明细也在 usage_logs 里，一起汇总。
	first, last := usage.AffectedHours(start, end)
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_hourly WHERE hour >= ? AND hour < ?`, first, last); err != nil {
		return fmt.Errorf("删除小时汇总: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_hourly (hour, token_id, model_name, requests, quota, prompt_tokens, completion_tokens,
			cache_read_tokens, cache_write_tokens)
		SELECT created_at - created_at % 3600, token_id, model_name, COUNT(*), SUM(quota),
			SUM(COALESCE(prompt_tokens, 0)), SUM(COALESCE(completion_tokens, 0)),
			SUM(COALESCE(cache_read_tokens, 0)), SUM(COALESCE(cache_write_tokens, 0))
		FROM usage_logs
		WHERE created_at >= ? AND created_at < ?
		GROUP BY created_at - created_at % 3600, token_id, model_name`, first, last); err != nil {
		return fmt.Errorf("重算小时汇总: %w", err)
	}
	if cursor != nil {
		if err := setKV(ctx, tx, port.KeyLogsCursor, fmt.Sprint(*cursor)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) LocalQuota(ctx context.Context, start, end int64) (int64, error) {
	var sum int64
	err := d.w.QueryRowContext(ctx, `SELECT COALESCE(SUM(quota), 0) FROM usage_logs WHERE created_at >= ? AND created_at < ?`,
		start, end).Scan(&sum)
	return sum, err
}

// SnapshotQuotaDelta 返回各令牌 used_quota 在 end 和 start 两个时刻的差值之和。快照只在变化时写，
// 某时刻的值 = 该时刻之前最近的一行；start 之前没有快照的令牌（当天新建的）按 0 算。
func (d *DB) SnapshotQuotaDelta(ctx context.Context, start, end int64) (int64, bool, error) {
	var ok bool
	if err := d.w.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM token_snapshots WHERE ts <= ?)`, start).Scan(&ok); err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, nil
	}
	var delta int64
	err := d.w.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
			COALESCE((SELECT x.used_quota FROM token_snapshots x WHERE x.token_id = t.token_id AND x.ts <= ? ORDER BY x.ts DESC LIMIT 1), 0) -
			COALESCE((SELECT x.used_quota FROM token_snapshots x WHERE x.token_id = t.token_id AND x.ts <= ? ORDER BY x.ts DESC LIMIT 1), 0)
		), 0)
		FROM (SELECT DISTINCT token_id FROM token_snapshots) t`, end, start).Scan(&delta)
	return delta, true, err
}

func (d *DB) SaveReconcile(ctx context.Context, r usage.ReconcileReport) error {
	_, err := d.w.ExecContext(ctx, `
		INSERT INTO reconcile_reports (day, upstream_quota, local_quota, snapshot_quota, checked_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (day) DO UPDATE SET upstream_quota = excluded.upstream_quota, local_quota = excluded.local_quota,
			snapshot_quota = excluded.snapshot_quota, checked_at = excluded.checked_at`,
		r.Day, r.UpstreamQuota, r.LocalQuota, r.SnapshotQuota, r.CheckedAt)
	return err
}

// Purge 按保留期删除明细和快照（§5.1）；每个令牌的最新快照保留，对账才能算出差值。
func (d *DB) Purge(ctx context.Context, logsBefore, snapshotsBefore int64) (int64, int64, error) {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM usage_logs WHERE created_at < ?`, logsBefore)
	if err != nil {
		return 0, 0, fmt.Errorf("清理明细: %w", err)
	}
	logs, _ := res.RowsAffected()
	res, err = tx.ExecContext(ctx, `
		DELETE FROM token_snapshots
		WHERE ts < ? AND ts < (SELECT MAX(s.ts) FROM token_snapshots s WHERE s.token_id = token_snapshots.token_id)`,
		snapshotsBefore)
	if err != nil {
		return 0, 0, fmt.Errorf("清理快照: %w", err)
	}
	snapshots, _ := res.RowsAffected()
	return logs, snapshots, tx.Commit()
}
