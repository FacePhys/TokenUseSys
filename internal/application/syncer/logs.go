package syncer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// syncNextWindow 同步游标之后的下一个封闭窗口（§4.3）：只取 lag 以前的数据，回填时一天一个窗口，
// 拉全后整段替换，并在同一个事务里推进游标。返回是否还没追上。
func (s *Syncer) syncNextWindow(ctx context.Context, now time.Time) (bool, error) {
	limit := now.Add(-s.cfg.Lag).Unix()
	end := usage.NextWindowEnd(s.cursor, now, s.cfg.Lag)
	if end <= s.cursor {
		return false, nil
	}
	entries, end, err := s.fetch(ctx, s.cursor, end)
	if err != nil {
		return false, err
	}
	if _, err := s.apply(ctx, s.cursor, end, entries, true, false); err != nil {
		return false, err
	}
	s.cursor = end
	return end < limit, nil
}

// fetch 拉全窗口 [start, end) 的日志，返回日志和实际的窗口结束位置：
// 条数超过上限（第 1 页的 total 或翻页数）时把窗口二分，先拉前一半，后一半留给下一个窗口。
func (s *Syncer) fetch(ctx context.Context, start, end int64) ([]usage.LogEntry, int64, error) {
	maxRows := int64(s.maxPages) * usage.PageSize
	for {
		entries, total, overflow, err := s.fetchPages(ctx, start, end, maxRows)
		if err != nil {
			return nil, 0, err
		}
		if !overflow {
			return entries, end, nil
		}
		mid := usage.Split(start, end)
		if mid <= start {
			return nil, 0, fmt.Errorf("1 秒内的日志超过 %d 条，无法拆分窗口", maxRows)
		}
		s.log.Info("窗口内日志太多，拆成两半", "start", start, "end", end, "total", total)
		end = mid
	}
}

// fetchPages 从第 1 页翻到某页不满为止（§4.3）。total 只做参考，但拉到的条数比它少时继续翻，
// 防止上游把 page_size 压到 100 以下时把不满一页误当成最后一页。
func (s *Syncer) fetchPages(ctx context.Context, start, end, maxRows int64) (entries []usage.LogEntry, total int64, overflow bool, err error) {
	for page := 1; ; page++ {
		if page > s.maxPages {
			return nil, total, true, nil
		}
		got, t, err := s.up.LogsPage(ctx, start, end-1, page)
		if err != nil {
			return nil, 0, false, err
		}
		if page == 1 {
			total = t
			if total > maxRows && end-start > 1 {
				return nil, total, true, nil
			}
		}
		entries = append(entries, got...)
		if len(got) == 0 || (len(got) < usage.PageSize && int64(len(entries)) >= total) {
			return entries, total, false, nil
		}
	}
}

// apply 把拉到的日志写进 [start, end)：先去掉窗口外的、归属到令牌，再整段替换。
// 上游返回 0 条而本地有数据时不替换（上游可能已清理旧日志，§4.3），force 为 true 时照样替换。
// advance 为 true 时在同一个事务里把游标推进到 end。返回是否因为防误删跳过了替换。
func (s *Syncer) apply(ctx context.Context, start, end int64, entries []usage.LogEntry, advance, force bool) (bool, error) {
	inWindow := entries[:0:0]
	for _, e := range entries {
		if e.CreatedAt >= start && e.CreatedAt < end {
			inWindow = append(inWindow, e)
		}
	}
	if dropped := len(entries) - len(inWindow); dropped > 0 {
		s.log.Warn("上游返回了窗口外的日志，已丢弃", "start", start, "end", end, "dropped", dropped)
	}
	if len(inWindow) == 0 && !force {
		has, err := s.store.HasLocalData(ctx, start, end)
		if err != nil {
			return false, err
		}
		if has {
			s.log.Warn("上游返回 0 条、本地有数据，不替换", "start", start, "end", end)
			if advance {
				return true, s.store.SetKV(ctx, port.KeyLogsCursor, strconv.FormatInt(end, 10))
			}
			return true, nil
		}
	}
	names, err := s.store.TokenIDsByName(ctx)
	if err != nil {
		return false, err
	}
	usage.Attribute(inWindow, names)
	if err := s.store.EnsureTokens(ctx, inWindow, s.now()); err != nil {
		return false, err
	}
	var cursor *int64
	if advance {
		cursor = &end
	}
	return false, s.store.ReplaceWindow(ctx, start, end, inWindow, cursor)
}

// RangeResult 是按小时重拉一段时间的结果。
type RangeResult struct {
	Rows    int
	Skipped int // 因为防误删没有替换的窗口数
}

// replaceRange 按 1 小时一个窗口重拉 [start, end)，不动游标。每日对账和 resync 用（§4.3）。
func (s *Syncer) replaceRange(ctx context.Context, start, end int64, force bool) (RangeResult, error) {
	var res RangeResult
	for hour := start; hour < end; hour += 3600 {
		hourEnd := min(hour+3600, end)
		for w := hour; w < hourEnd; {
			entries, wEnd, err := s.fetch(ctx, w, hourEnd)
			if err != nil {
				return res, err
			}
			skipped, err := s.apply(ctx, w, wEnd, entries, false, force)
			if err != nil {
				return res, err
			}
			if skipped {
				res.Skipped++
			} else {
				res.Rows += len(entries)
			}
			w = wEnd
		}
	}
	return res, nil
}

// ErrResyncToday 表示要重拉的日期不是昨天及以前：今天的窗口还在由同步器推进，不能和游标重叠（§4.6）。
var ErrResyncToday = errors.New("只能重拉昨天及以前")

// Resync 按 1 小时窗口重拉某一天并重算汇总（relayctl resync）。day 是当天任意时刻。
// 上游返回 0 条时默认不替换本地数据，force 为 true 时强制替换。比 data_since 早时把它提前。
func (s *Syncer) Resync(ctx context.Context, day time.Time, force bool) (RangeResult, error) {
	start := usage.DayStart(day, s.cfg.Location)
	if !start.Before(usage.DayStart(s.now(), s.cfg.Location)) {
		return RangeResult{}, ErrResyncToday
	}
	res, err := s.replaceRange(ctx, start.Unix(), start.AddDate(0, 0, 1).Unix(), force)
	if err != nil {
		return res, err
	}
	since, ok, err := s.store.GetKV(ctx, port.KeyDataSince)
	if err != nil {
		return res, err
	}
	if d := start.Format(usage.DayLayout); !ok || d < since {
		return res, s.store.SetKV(ctx, port.KeyDataSince, d)
	}
	return res, nil
}
