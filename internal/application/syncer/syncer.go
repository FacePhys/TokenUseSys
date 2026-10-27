// Package syncer 是后台同步（开发文档 §4）：一个 goroutine 串行执行全部任务——换算参数、令牌和账户快照、
// 日志窗口同步（含首次回填）、每日对账、清理和备份，不会并发写库。
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// 固定的时刻和间隔（§4.3、§4.7、§5.1）。
const (
	statusInterval = time.Hour
	authPause      = 30 * time.Minute
	purgeAt        = 4 * time.Hour
	backupAt       = 4*time.Hour + 10*time.Minute
	// dailyRetry 是每日任务失败或还不能执行（回填中）时的重试间隔。
	dailyRetry = 10 * time.Minute
	// lateSlot 以内的快照算按时执行，记在对齐的时刻上；更晚的（比如刚启动）记实际时间。
	lateSlot = time.Minute
	// lagAlertAfter：回填完成后日志同步落后超过它就发告警（§4.7）。
	lagAlertAfter = 30 * time.Minute
)

// 告警的 key（同一 key 6 小时内只发一次，见 alert 包）。
const (
	alertAuth = "auth"
	alertLag  = "lag"
)

type Config struct {
	LogInterval           time.Duration
	SnapshotInterval      time.Duration
	Lag                   time.Duration
	BackfillDays          int
	ReconcileAt           time.Duration // 当地 0 点之后多久，例如 3h30m
	LogRetentionDays      int
	SnapshotRetentionDays int
	BackupDir             string
	BackupKeepDays        int
	Location              *time.Location
}

type Syncer struct {
	up    port.Upstream
	store port.Store
	cfg   Config
	log   *slog.Logger
	alert port.Alerter
	now   func() time.Time
	// maxPages 是一个窗口最多翻的页数，测试里改小。
	maxPages int

	cursor        int64
	backfillUntil int64
	pausedUntil   time.Time
	next          map[string]time.Time
	states        map[string]port.TaskState
	lastDay       map[string]string // 每日任务 → 最后完成的日期
	tasks         []task
}

type task struct {
	name string
	// upstream 为 true 的任务要访问上游，鉴权失败暂停期间不执行。
	upstream bool
	// run 执行一次并返回下次执行的时刻。
	run func(ctx context.Context, now time.Time) (time.Time, error)
}

type Option func(*Syncer)

// WithClock 替换时钟，测试用。
func WithClock(now func() time.Time) Option { return func(s *Syncer) { s.now = now } }

// WithMaxPages 改小单窗口的页数上限，测试窗口二分用。
func WithMaxPages(n int) Option { return func(s *Syncer) { s.maxPages = n } }

// WithAlerter 设置告警；不设置时不发。
func WithAlerter(a port.Alerter) Option { return func(s *Syncer) { s.alert = a } }

type noAlert struct{}

func (noAlert) Notify(context.Context, port.Alert) {}

func New(up port.Upstream, store port.Store, cfg Config, log *slog.Logger, opts ...Option) *Syncer {
	s := &Syncer{
		up: up, store: store, cfg: cfg, log: log, alert: noAlert{}, now: time.Now, maxPages: usage.MaxPages,
		next: map[string]time.Time{}, states: map[string]port.TaskState{}, lastDay: map[string]string{},
	}
	for _, o := range opts {
		o(s)
	}
	// 顺序即同一轮里的执行顺序：先拿到令牌列表，日志才能按名字归属。
	s.tasks = []task{
		{port.TaskStatus, true, s.statusTask},
		{port.TaskSnapshot, true, s.snapshotTask},
		{port.TaskLogs, true, s.logsTask},
		{port.TaskPurge, false, s.dailyTask(port.KeyPurgeDay, purgeAt, s.purge)},
		{port.TaskBackup, false, s.dailyTask(port.KeyBackupDay, backupAt, s.backup)},
		// 对账要重拉 24 个小时窗口，最慢，放在最后。
		{port.TaskReconcile, true, s.dailyTask(port.KeyReconcileDay, s.cfg.ReconcileAt, s.reconcileYesterday)},
	}
	return s
}

// Run 加载状态后循环执行到期的任务，直到 ctx 取消。取消时正在写的窗口事务回滚、游标不动，下次启动重做。
func (s *Syncer) Run(ctx context.Context) error {
	if err := s.Init(ctx); err != nil {
		return err
	}
	for {
		wait := s.Step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// Init 读取游标和各任务状态。第一次启动时游标设为「今天 0 点 − backfill_days 天」，开始回填（§4.3）。
func (s *Syncer) Init(ctx context.Context) error {
	now := s.now()
	cursor, ok, err := s.kvInt(ctx, port.KeyLogsCursor)
	if err != nil {
		return err
	}
	if !ok {
		start := usage.DayStart(now, s.cfg.Location).AddDate(0, 0, -s.cfg.BackfillDays)
		cursor = start.Unix()
		for k, v := range map[string]string{
			port.KeyLogsCursor:    strconv.FormatInt(cursor, 10),
			port.KeyDataSince:     start.Format(usage.DayLayout),
			port.KeyBackfillUntil: strconv.FormatInt(now.Add(-s.cfg.Lag).Unix(), 10),
		} {
			if err := s.store.SetKV(ctx, k, v); err != nil {
				return err
			}
		}
		s.log.Info("首次启动，开始回填", "from", start.Format(usage.DayLayout), "days", s.cfg.BackfillDays)
	}
	s.cursor = cursor
	if s.backfillUntil, _, err = s.kvInt(ctx, port.KeyBackfillUntil); err != nil {
		return err
	}
	for _, t := range s.tasks {
		v, ok, err := s.store.GetKV(ctx, port.KeyTaskPrefix+t.name)
		if err != nil {
			return err
		}
		if ok {
			var st port.TaskState
			if json.Unmarshal([]byte(v), &st) == nil {
				s.states[t.name] = st
			}
		}
	}
	for _, key := range []string{port.KeyReconcileDay, port.KeyPurgeDay, port.KeyBackupDay} {
		v, _, err := s.store.GetKV(ctx, key)
		if err != nil {
			return err
		}
		s.lastDay[key] = v
	}
	return nil
}

// Backfilling 返回是否还在首次回填。
func (s *Syncer) Backfilling() bool { return s.cursor < s.backfillUntil }

// Step 执行所有到期的任务，返回距离下一个任务到期的时间。
func (s *Syncer) Step(ctx context.Context) time.Duration {
	for _, t := range s.tasks {
		if ctx.Err() != nil {
			return 0
		}
		now := s.now()
		if now.Before(s.next[t.name]) {
			continue
		}
		if t.upstream && now.Before(s.pausedUntil) {
			s.next[t.name] = s.pausedUntil
			continue
		}
		next, err := t.run(ctx, now)
		s.next[t.name] = next
		if !errors.Is(err, errIdle) {
			s.record(ctx, t, now, err)
		}
	}
	now := s.now()
	s.checkLag(ctx, now)
	wait := time.Hour
	for _, t := range s.tasks {
		wait = min(wait, s.next[t.name].Sub(now))
	}
	return max(wait, 0)
}

// record 把任务结果写进 kv；鉴权失败时暂停所有访问上游的任务 30 分钟，不重试轰炸，并发告警（§4.7）。
func (s *Syncer) record(ctx context.Context, t task, now time.Time, err error) {
	if err != nil && ctx.Err() != nil {
		return // 停机时被中断，不算失败
	}
	name := t.name
	st := s.states[name]
	if err == nil {
		st.LastOKAt, st.LastError, st.LastErrorAt = now.Unix(), "", 0
		if t.upstream {
			s.alert.Notify(ctx, port.Alert{Key: alertAuth, Resolved: true, Text: "上游鉴权已恢复，同步继续。"})
		}
	} else {
		st.LastError, st.LastErrorAt = err.Error(), now.Unix()
		if errors.Is(err, port.ErrUpstreamAuth) {
			s.pausedUntil = now.Add(authPause)
			s.log.Error("上游鉴权失败，暂停同步 30 分钟；检查 upstream.access_token 和 user_id", "task", name, "error", err)
			if e := s.store.SetKV(ctx, port.KeyAuthPausedUntil, strconv.FormatInt(s.pausedUntil.Unix(), 10)); e != nil {
				s.log.Error("写入暂停时间失败", "error", e)
			}
			s.alert.Notify(ctx, port.Alert{Key: alertAuth, Text: "上游鉴权失败，同步暂停 30 分钟后重试。检查 config.yaml 的 " +
				"upstream.access_token 和 user_id；在 4router 上重新生成过 access token 的话，改配置后重启。错误：" + err.Error()})
		} else {
			s.log.Warn("任务失败", "task", name, "error", err)
		}
	}
	s.states[name] = st
	b, _ := json.Marshal(st)
	if e := s.store.SetKV(ctx, port.KeyTaskPrefix+name, string(b)); e != nil {
		s.log.Error("写入任务状态失败", "task", name, "error", e)
	}
}

func (s *Syncer) statusTask(ctx context.Context, now time.Time) (time.Time, error) {
	cur, err := s.up.Currency(ctx)
	if err != nil {
		return now.Add(time.Minute), err
	}
	b, _ := json.Marshal(cur)
	return now.Add(statusInterval), s.store.SetKV(ctx, port.KeyCurrency, string(b))
}

// snapshotTask 按整 5 分钟执行（§4.4）。按时执行的记在对齐的时刻上，保证每天 0 点有一个点；
// 刚启动等不在整点的，记实际时间。
func (s *Syncer) snapshotTask(ctx context.Context, now time.Time) (time.Time, error) {
	slot := s.next[port.TaskSnapshot]
	if slot.IsZero() || now.Sub(slot) > lateSlot {
		slot = now
	}
	next := now.Truncate(s.cfg.SnapshotInterval).Add(s.cfg.SnapshotInterval)
	return next, s.snapshot(ctx, slot, now)
}

func (s *Syncer) snapshot(ctx context.Context, slot, now time.Time) error {
	tokens, err := s.up.Tokens(ctx)
	if errors.Is(err, port.ErrUpstreamAuth) {
		return err
	}
	// 某页失败时只更新已拉到的令牌，不标删除（§4.4）。
	if e := s.store.SyncTokens(ctx, tokens, err == nil, slot, now); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("令牌列表没拉完（已更新 %d 个，不标删除）：%w", len(tokens), err)
	}
	acc, err := s.up.Account(ctx)
	if err != nil {
		return err
	}
	acc.UpdatedAt = now.Unix()
	b, _ := json.Marshal(acc)
	return s.store.SetKV(ctx, port.KeyAccount, string(b))
}

func (s *Syncer) logsTask(ctx context.Context, now time.Time) (time.Time, error) {
	more, err := s.syncNextWindow(ctx, now)
	switch {
	case err != nil:
		return now.Add(s.cfg.LogInterval), err
	case more:
		return now, nil // 还没追上：马上继续，中间照样检查别的任务
	}
	return now.Add(s.cfg.LogInterval), nil
}

// dailyTask 生成每天 at（当地时间）执行一次的任务。按 kv 里最后完成的日期判断，重启后没做的会补做。
// work 返回 errNotYet 时表示条件还不满足（比如回填中），稍后再试，不算失败。
func (s *Syncer) dailyTask(key string, at time.Duration, work func(ctx context.Context, now time.Time) error) func(context.Context, time.Time) (time.Time, error) {
	return func(ctx context.Context, now time.Time) (time.Time, error) {
		today := usage.DayStart(now, s.cfg.Location)
		due := today.Add(at)
		switch {
		case now.Before(due):
			return due, errIdle
		case s.lastDay[key] == today.Format(usage.DayLayout):
			return due.AddDate(0, 0, 1), errIdle
		}
		err := work(ctx, now)
		if errors.Is(err, errNotYet) {
			return now.Add(dailyRetry), errIdle
		}
		if err != nil {
			return now.Add(dailyRetry), err
		}
		s.lastDay[key] = today.Format(usage.DayLayout)
		if err := s.store.SetKV(ctx, key, s.lastDay[key]); err != nil {
			return now.Add(dailyRetry), err
		}
		return due.AddDate(0, 0, 1), nil
	}
}

var (
	// errNotYet 表示每日任务的前提还不满足，稍后再试。
	errNotYet = errors.New("条件还不满足")
	// errIdle 表示任务这次什么都没做（没到时间、今天已做过、条件不满足），不更新执行状态。
	errIdle = errors.New("没有执行")
)

func (s *Syncer) kvInt(ctx context.Context, key string) (int64, bool, error) {
	v, ok, err := s.store.GetKV(ctx, key)
	if err != nil || !ok {
		return 0, false, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("kv %s 不是整数：%q", key, v)
	}
	return n, true, nil
}

// checkLag：回填完成后日志同步落后超过 30 分钟时告警，追上后发恢复通知（§4.7）。鉴权暂停期间已经发过鉴权告警，不重复。
func (s *Syncer) checkLag(ctx context.Context, now time.Time) {
	if s.Backfilling() || now.Before(s.pausedUntil) || ctx.Err() != nil {
		return
	}
	lag := now.Sub(time.Unix(s.cursor, 0))
	if lag <= lagAlertAfter {
		s.alert.Notify(ctx, port.Alert{Key: alertLag, Resolved: true, Text: "日志同步已追上。"})
		return
	}
	text := fmt.Sprintf("日志同步已落后 %d 分钟，数据截止 %s。", int(lag.Minutes()),
		time.Unix(s.cursor, 0).In(s.cfg.Location).Format("01-02 15:04"))
	if st := s.states[port.TaskLogs]; st.LastError != "" && st.LastErrorAt > st.LastOKAt {
		text += "最近的错误：" + st.LastError
	}
	s.alert.Notify(ctx, port.Alert{Key: alertLag, Text: text})
}
