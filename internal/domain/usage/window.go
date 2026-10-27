package usage

import "time"

// 日志同步的分页和窗口限制（§4.3）。
const (
	// PageSize 是上游 page_size 的上限。
	PageSize = 100
	// MaxPages 是一个窗口最多翻的页数，超过就把窗口二分。
	MaxPages = 300
	// MaxWindow 是单个窗口的最长跨度（回填时按天推进）。
	MaxWindow = 24 * time.Hour
)

const secondsPerHour = 3600

// HourStart 返回 ts 所在的整点。server.timezone 的偏移是整小时（config 校验），本地整点和 UTC 整点一致。
func HourStart(ts int64) int64 {
	return ts - ((ts%secondsPerHour)+secondsPerHour)%secondsPerHour
}

// AffectedHours 返回窗口 [start, end) 涉及的整点范围 [first, last)，小时汇总按它重算。
func AffectedHours(start, end int64) (int64, int64) {
	return HourStart(start), HourStart(end-1) + secondsPerHour
}

// WholeHours 返回完全落在 [start, end) 里的整点范围 [first, last)；没有时 first >= last。
func WholeHours(start, end int64) (int64, int64) {
	first := HourStart(start)
	if first < start {
		first += secondsPerHour
	}
	return first, HourStart(end)
}

// NextWindowEnd 返回从 cursor 开始的下一个窗口的结束位置：最长 MaxWindow，且只到 now - lag（窗口已封闭）。
// 返回值不大于 cursor 时表示暂时没有可同步的。
func NextWindowEnd(cursor int64, now time.Time, lag time.Duration) int64 {
	return min(cursor+int64(MaxWindow/time.Second), now.Add(-lag).Unix())
}

// Split 返回把 [start, end) 分成两半的位置。跨度超过 2 小时时对齐到整点，
// 这样拆开后的每个窗口仍覆盖完整的小时，小时汇总不会只算到半个小时的明细。
func Split(start, end int64) int64 {
	mid := start + (end-start)/2
	if end-start > 2*secondsPerHour {
		if h := HourStart(mid); h > start {
			return h
		}
	}
	return mid
}

// DayStart 返回 t 在 loc 时区里当天 0 点。
func DayStart(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// DayLayout 是日期的格式，例如 2026-10-26。
const DayLayout = "2006-01-02"
