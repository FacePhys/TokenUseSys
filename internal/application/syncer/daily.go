package syncer

import (
	"context"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// reconcileYesterday 是每日对账（§4.3）：把昨天按 1 小时窗口重拉一遍补上迟到的日志，
// 再分别和上游 stat、令牌快照的 used_quota 差值比较，结果写进 reconcile_reports。
// 回填中、或游标还没过今天 0 点时稍后再做：刚回填的数据不用马上重拉，先追上进度。
func (s *Syncer) reconcileYesterday(ctx context.Context, now time.Time) error {
	today := usage.DayStart(now, s.cfg.Location)
	if s.Backfilling() || s.cursor < today.Unix() {
		return errNotYet
	}
	day := today.AddDate(0, 0, -1)
	start, end := day.Unix(), today.Unix()
	res, err := s.replaceRange(ctx, start, end, false)
	if err != nil {
		return err
	}
	upstream, err := s.up.LogStat(ctx, start, end-1)
	if err != nil {
		return err
	}
	local, err := s.store.LocalQuota(ctx, start, end)
	if err != nil {
		return err
	}
	report := usage.ReconcileReport{Day: day.Format(usage.DayLayout), UpstreamQuota: upstream, LocalQuota: local, CheckedAt: now.Unix()}
	delta, ok, err := s.store.SnapshotQuotaDelta(ctx, start, end)
	if err != nil {
		return err
	}
	if ok {
		report.SnapshotQuota = &delta
	}
	if err := s.store.SaveReconcile(ctx, report); err != nil {
		return err
	}
	args := []any{"day", report.Day, "upstream", upstream, "local", local, "rows", res.Rows, "skipped_windows", res.Skipped}
	if ok {
		args = append(args, "snapshot", delta)
	}
	if upstream != local {
		s.log.Warn("对账不一致", args...)
	} else {
		s.log.Info("对账完成", args...)
	}
	return nil
}

// purge 按保留期清理明细和快照（§5.1）。回填中不清理：回填拆开的窗口要靠还在的明细重算整小时的汇总。
func (s *Syncer) purge(ctx context.Context, now time.Time) error {
	if s.Backfilling() {
		return errNotYet
	}
	today := usage.DayStart(now, s.cfg.Location)
	logs, snapshots, err := s.store.Purge(ctx,
		today.AddDate(0, 0, -s.cfg.LogRetentionDays).Unix(),
		today.AddDate(0, 0, -s.cfg.SnapshotRetentionDays).Unix())
	if err != nil {
		return err
	}
	s.log.Info("清理完成", "logs", logs, "snapshots", snapshots)
	return nil
}

// backup 每天备份一次，文件名带当地时间（§12.6）。
func (s *Syncer) backup(ctx context.Context, now time.Time) error {
	path, err := s.store.Backup(ctx, s.cfg.BackupDir, now.In(s.cfg.Location), s.cfg.BackupKeepDays)
	if err != nil {
		return err
	}
	s.log.Info("备份完成", "path", path)
	return nil
}
