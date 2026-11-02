package dashboard

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// 日报的长度限制：前 8 个令牌、前 5 个模型，其余合并；名字最长 48 字节。
// 企业微信的文本消息上限是 2048 字节，这样最长也放得下。
const (
	reportTopTokens = 8
	reportTopModels = 5
	reportNameBytes = 48
)

var weekdays = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// DailyReport 生成某一天的用量日报（§4.7）：纯文本，飞书、钉钉、企业微信都能显示。
// 用和页面相同的查询，数字和看板上选这一天时一致。
func (s *Service) DailyReport(ctx context.Context, day time.Time) (string, error) {
	date := day.In(s.loc).Format(usage.DayLayout)
	r, err := s.ParseRange(date, date)
	if err != nil {
		return "", err
	}
	cur, _, err := s.currency(ctx)
	if err != nil {
		return "", err
	}
	tokens, err := s.Tokens(ctx, r)
	if err != nil {
		return "", err
	}
	models, err := s.Models(ctx, r, nil)
	if err != nil {
		return "", err
	}
	summary, err := s.Summary(ctx)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "用量日报（%s %s）\n", date, weekdays[r.from.Weekday()])
	t := tokens.Total
	if t.Requests == 0 {
		b.WriteString("当天没有用量。\n")
	} else {
		change, err := s.changeFromPreviousDay(ctx, r)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "总消费 %s%s\n", cur.Format(t.Amount), change)
		fmt.Fprintf(&b, "请求 %s 次，Token %s（输入 %s，输出 %s）\n", usage.FormatInt(t.Requests),
			compactCount(t.PromptTokens+t.CompletionTokens), compactCount(t.PromptTokens), compactCount(t.CompletionTokens))

		var rows []reportRow
		for _, it := range tokens.Items {
			if it.Usage.Requests > 0 {
				rows = append(rows, reportRow{it.Name, it.Usage.Amount, it.Usage.Requests})
			}
		}
		b.WriteString("\n按令牌：\n")
		writeRows(&b, rows, reportTopTokens, t.Amount, cur, true)

		rows = rows[:0]
		for _, m := range models.Items {
			name := m.Model
			if name == "" {
				name = "（未知模型）"
			}
			rows = append(rows, reportRow{name, m.Amount, m.Requests})
		}
		b.WriteString("\n按模型：\n")
		writeRows(&b, rows, reportTopModels, t.Amount, cur, false)
	}

	var footer []string
	if rc := summary.Reconcile; rc != nil && rc.Day == date && rc.Warn {
		note := fmt.Sprintf("注意：当天对账有差异（上游 %s，本地 %s", cur.Format(rc.Upstream), cur.Format(rc.Local))
		if rc.Snapshot != nil {
			note += "，令牌快照 " + cur.Format(*rc.Snapshot)
		}
		footer = append(footer, note+"），以上数字可能不准。")
	}
	if summary.Account != nil {
		footer = append(footer, "账户余额 "+cur.Format(summary.Account.Balance))
	}
	if len(footer) > 0 {
		b.WriteString("\n" + strings.Join(footer, "\n"))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// changeFromPreviousDay 返回和前一天比的变化，用原始 quota 算；前一天没有用量（比如还没开始同步）时为空。
func (s *Service) changeFromPreviousDay(ctx context.Context, r Range) (string, error) {
	current, err := s.totalQuota(ctx, r.from, r.to)
	if err != nil {
		return "", err
	}
	previous, err := s.totalQuota(ctx, r.from.AddDate(0, 0, -1), r.from)
	if err != nil || previous <= 0 {
		return "", err
	}
	change := (float64(current) - float64(previous)) / float64(previous) * 100
	if math.Abs(change) < 0.05 {
		return "，和前一天持平", nil
	}
	return fmt.Sprintf("，较前一天 %+.1f%%", change), nil
}

func (s *Service) totalQuota(ctx context.Context, from, to time.Time) (int64, error) {
	groups, err := s.store.Usage(ctx, port.UsageQuery{Start: from.Unix(), End: to.Unix()})
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, g := range groups {
		sum += g.Quota
	}
	return sum, nil
}

type reportRow struct {
	name     string
	amount   float64
	requests int64
}

// writeRows 输出前 top 行（rows 已按金额从高到低排），其余合并成一行「其他 N 个」。
func writeRows(b *strings.Builder, rows []reportRow, top int, total float64, cur usage.Currency, withRequests bool) {
	write := func(r reportRow) {
		fmt.Fprintf(b, "%s  %s", shortName(r.name), cur.Format(r.amount))
		if total > 0 {
			fmt.Fprintf(b, "（%s）", sharePct(r.amount/total))
		}
		if withRequests {
			fmt.Fprintf(b, "，%s 次", usage.FormatInt(r.requests))
		}
		b.WriteByte('\n')
	}
	for i, r := range rows {
		if i == top {
			break
		}
		write(r)
	}
	if len(rows) > top {
		rest := reportRow{name: fmt.Sprintf("其他 %d 个", len(rows)-top)}
		for _, r := range rows[top:] {
			rest.amount += r.amount
			rest.requests += r.requests
		}
		write(rest)
	}
}

// shortName 按字节截断过长的名字，不切断字符。
func shortName(name string) string {
	if len(name) <= reportNameBytes {
		return name
	}
	cut := reportNameBytes
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + "…"
}

func sharePct(x float64) string {
	if x != 0 && x < 0.01 {
		return fmt.Sprintf("%.2f%%", x*100)
	}
	return fmt.Sprintf("%.1f%%", x*100)
}

// compactCount 把大数缩写成 1.2万、3.4亿，和页面上的写法一致。
func compactCount(n int64) string {
	trim := func(x float64) string {
		digits := 2
		switch {
		case x >= 100:
			digits = 0
		case x >= 10:
			digits = 1
		}
		pow := math.Pow10(digits)
		return strconv.FormatFloat(math.Round(x*pow)/pow, 'f', -1, 64)
	}
	switch {
	case n >= 100_000_000:
		return trim(float64(n)/1e8) + "亿"
	case n >= 10_000:
		return trim(float64(n)/1e4) + "万"
	}
	return usage.FormatInt(n)
}
