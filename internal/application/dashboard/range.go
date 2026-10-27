package dashboard

import (
	"errors"
	"fmt"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// 日期区间和分页的限制（§6）。
const (
	MaxRangeDays       = 400
	MaxHourSeriesDays  = 7
	MaxHourExportDays  = 31
	DefaultRangeDays   = 7
	DefaultTop         = 5
	MaxTop             = 20
	DefaultLogPageSize = 50
	MaxLogPageSize     = 100
)

var (
	// ErrInvalidArgument 表示请求参数不合法，接口返回 400。
	ErrInvalidArgument = errors.New("参数错误")
	// ErrNotFound 表示令牌不存在，接口返回 404。
	ErrNotFound = errors.New("不存在")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w：%s", ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// Range 是日期区间：Start、End 是当地日期，两端都含。
type Range struct {
	Start string `json:"start"`
	End   string `json:"end"`
	// from 是 Start 当天 0 点，to 是 End 第二天 0 点：查询用 [from, to)。
	from, to time.Time
}

// Days 返回区间的天数。
func (r Range) Days() int { return int(r.to.Sub(r.from).Hours() / 24) }

// From 和 To 返回查询用的时间范围 [From, To)，Unix 秒，都对齐当地 0 点。
func (r Range) From() int64 { return r.from.Unix() }
func (r Range) To() int64   { return r.to.Unix() }

// ParseRange 解析日期区间（§6）：两端都含；都省略时是近 7 天（今天往前推 6 天到今天）；
// 只给 start 时到今天为止，只给 end 时往前 7 天。跨度最长 400 天。
func (s *Service) ParseRange(start, end string) (Range, error) {
	today := usage.DayStart(s.now(), s.loc)
	parse := func(name, v string) (time.Time, error) {
		t, err := time.ParseInLocation(usage.DayLayout, v, s.loc)
		if err != nil {
			return time.Time{}, invalid("%s 格式应为 2026-10-27", name)
		}
		return t, nil
	}
	var from, to time.Time
	var err error
	switch {
	case start == "" && end == "":
		to = today
		from = today.AddDate(0, 0, -(DefaultRangeDays - 1))
	case start == "":
		if to, err = parse("end", end); err != nil {
			return Range{}, err
		}
		from = to.AddDate(0, 0, -(DefaultRangeDays - 1))
	default:
		if from, err = parse("start", start); err != nil {
			return Range{}, err
		}
		to = today
		if end != "" {
			if to, err = parse("end", end); err != nil {
				return Range{}, err
			}
		}
	}
	if from.After(to) {
		return Range{}, invalid("start 不能晚于 end")
	}
	r := Range{Start: from.Format(usage.DayLayout), End: to.Format(usage.DayLayout), from: from, to: to.AddDate(0, 0, 1)}
	if r.Days() > MaxRangeDays {
		return Range{}, invalid("日期区间最长 %d 天", MaxRangeDays)
	}
	return r, nil
}
