package newapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Level 是自检一项的结论。
type Level string

const (
	Pass Level = "通过"
	Note Level = "注意" // 与开发文档的假设不同，要改文档或代码
	Fail Level = "失败" // 后续步骤无法进行
	Info Level = "信息"
	Skip Level = "跳过"
)

type Check struct {
	Step   int
	Title  string
	Level  Level
	Detail string
}

type SelfCheckReport struct {
	Checks []Check
	// Fixtures 是脱敏后的响应体，文件名 → 内容，带 -fixtures 时写进 testdata/fixtures/。
	Fixtures map[string][]byte
}

// Failed 返回是否有失败项。
func (r *SelfCheckReport) Failed() bool {
	return slices.ContainsFunc(r.Checks, func(c Check) bool { return c.Level == Fail })
}

type SelfCheckOptions struct {
	Now time.Time
	// Lag 和同步一致：只取 Lag 以前的日志，保证窗口已封闭。
	Lag      time.Duration
	Location *time.Location
	// Progress 每步开始时写一行，可为 nil。
	Progress io.Writer
}

const (
	// maxWindowRows 是第 7 步窗口的条数上限：两种 page_size 各拉一遍，2000 条约 60 次请求、2 分钟。
	maxWindowRows = 2000
	// maxRefundPages 是第 9 步最多翻的页数。
	maxRefundPages = 10
)

// 开发文档 §4.1 用到的字段，缺了要改文档。
var (
	tokenFields = []string{"id", "name", "status", "group", "remain_quota", "used_quota", "unlimited_quota",
		"expired_time", "created_time", "accessed_time", "model_limits_enabled", "model_limits"}
	logFields = []string{"created_at", "type", "token_id", "token_name", "model_name", "quota", "prompt_tokens",
		"completion_tokens", "use_time", "is_stream", "group", "other"}
	otherFields = []string{"cache_tokens", "cache_creation_tokens", "frt"}
)

// SelfCheck 按开发文档 §10 的清单逐项核对上游接口（M1）。只读，耗时主要在限速：约 3～5 分钟。
func SelfCheck(ctx context.Context, c *Client, opts SelfCheckOptions) *SelfCheckReport {
	if opts.Location == nil {
		opts.Location = time.Local
	}
	k := &checker{c: c, opts: opts, san: newSanitizer(), report: &SelfCheckReport{Fixtures: map[string][]byte{}}}
	steps := []struct {
		title string
		run   func(context.Context) (Level, string)
	}{
		{"/api/status 换算参数", k.status},
		{"不带 New-Api-User 调 /api/user/self", k.selfWithoutUserHeader},
		{"带齐两个头调 /api/user/self", k.self},
		{"/api/token/ 分页与字段", k.tokens},
		{"/api/log/self 消费日志字段", k.logs},
		{"时间区间两端是否都含", k.closedInterval},
		{"封闭窗口内分页是否稳定", k.pagination},
		{"窗口合计与 /api/log/self/stat 一致", k.stat},
		{"近 30 天退款日志（type=6）", k.refunds},
		{"上游最早一条消费日志", k.earliest},
	}
	for i, s := range steps {
		step := i + 1
		if ctx.Err() != nil {
			k.add(step, s.title, Skip, "已中断")
			continue
		}
		if opts.Progress != nil {
			fmt.Fprintf(opts.Progress, "[%d/%d] %s…\n", step, len(steps), s.title)
		}
		level, detail := s.run(ctx)
		k.add(step, s.title, level, detail)
	}
	k.verifyFixtures()
	return k.report
}

// verifyFixtures 是脱敏的最后一道检查：样本里出现 access token 或任何原始 key 时，全部样本都不保存。
func (k *checker) verifyFixtures() {
	secrets := append([]string{k.c.token}, k.san.originalKeys()...)
	for name, data := range k.report.Fixtures {
		for _, secret := range secrets {
			if len(secret) >= 8 && bytes.Contains(data, []byte(secret)) {
				k.report.Fixtures = map[string][]byte{}
				k.add(0, "样本脱敏检查", Fail, name+" 里仍有密钥，全部样本都不保存")
				return
			}
		}
	}
}

type checker struct {
	c      *Client
	opts   SelfCheckOptions
	san    *sanitizer
	report *SelfCheckReport

	// 前面步骤的结果，后面的步骤依赖它们。
	conv       converter
	authOK     bool
	logsOK     bool  // 消费日志是 {items, total} 分页格式，且至少有一条
	newestLog  int64 // 第 5 步拿到的最新一条日志的 created_at
	window     [2]int64
	windowSum  int64
	windowRows int
}

func (k *checker) add(step int, title string, level Level, detail string) {
	k.report.Checks = append(k.report.Checks, Check{Step: step, Title: title, Level: level, Detail: detail})
}

func (k *checker) needAuth() (Level, string, bool) {
	if !k.authOK {
		return Skip, "第 3 步鉴权没通过", false
	}
	return "", "", true
}

func (k *checker) needLogs() (Level, string, bool) {
	if l, d, ok := k.needAuth(); !ok {
		return l, d, false
	}
	if !k.logsOK {
		return Skip, "第 5 步没有拿到可用的消费日志", false
	}
	return "", "", true
}

// 第 1 步：换算参数。/api/status 免鉴权，客户端照常带头，不影响结果。
func (k *checker) status(ctx context.Context) (Level, string) {
	resp, err := k.c.Raw(ctx, "/api/status", nil, true)
	if err != nil {
		return Fail, err.Error()
	}
	env, data, err := parseEnvelope(resp)
	if err != nil {
		return Fail, err.Error()
	}
	k.report.Fixtures["status.json"] = mustIndent(resp.Body)
	if !env.Success {
		return Fail, "success=false：" + env.Message
	}
	m, _ := data.(map[string]any)
	qpu, _ := toFloat(m["quota_per_unit"])
	display := toString(m["quota_display_type"])
	rate, hasRate := toFloat(m["usd_exchange_rate"])
	k.conv = converter{perUnit: qpu, display: display, rate: rate}
	detail := fmt.Sprintf("quota_per_unit=%v，quota_display_type=%q，usd_exchange_rate=%v，version=%q",
		m["quota_per_unit"], display, m["usd_exchange_rate"], toString(m["version"]))
	switch {
	case qpu <= 0:
		return Fail, detail + "；quota_per_unit 无效，无法换算金额"
	case display == "" || !hasRate:
		return Note, detail + "；缺少 quota_display_type 或 usd_exchange_rate，§4.1 的换算规则要改"
	case !slices.Contains([]string{"CNY", "USD", "TOKENS"}, display):
		return Note, detail + "；quota_display_type 不是 CNY / USD / TOKENS"
	}
	return Pass, detail
}

// 第 2 步：故意不带 New-Api-User，看这个头是否必需。两种结果都可以，客户端总是带上。
func (k *checker) selfWithoutUserHeader(ctx context.Context) (Level, string) {
	resp, err := k.c.Raw(ctx, "/api/user/self", nil, false)
	if err != nil {
		return Fail, err.Error()
	}
	env, _, err := parseEnvelope(resp)
	if err != nil {
		return Note, err.Error()
	}
	k.report.Fixtures["user_self_without_user_header.json"] = k.san.envelope(resp.Body, k.san.user)
	if env.Success {
		return Info, fmt.Sprintf("HTTP %d，success=true：New-Api-User 不是必需的（客户端照样带上）", resp.Status)
	}
	return Info, fmt.Sprintf("HTTP %d，success=false，%q：New-Api-User 是必需的", resp.Status, env.Message)
}

// 第 3 步：带齐两个头，必须成功，否则后面全部跳过。
func (k *checker) self(ctx context.Context) (Level, string) {
	resp, err := k.c.Raw(ctx, "/api/user/self", nil, true)
	if err != nil {
		return Fail, err.Error()
	}
	env, data, err := parseEnvelope(resp)
	if err != nil {
		return Fail, err.Error()
	}
	if !env.Success {
		return Fail, fmt.Sprintf("HTTP %d，%q：检查 upstream.access_token 和 upstream.user_id", resp.Status, env.Message)
	}
	k.authOK = true
	k.report.Fixtures["user_self.json"] = k.san.envelope(resp.Body, k.san.user)
	m, _ := data.(map[string]any)
	missing := missingKeys(m, []string{"quota", "used_quota", "request_count"})
	quota, _ := toInt(m["quota"])
	used, _ := toInt(m["used_quota"])
	detail := fmt.Sprintf("余额 %s，已用 %s，请求 %v 次", k.conv.money(quota), k.conv.money(used), m["request_count"])
	if len(missing) > 0 {
		return Note, detail + "；缺少字段 " + strings.Join(missing, "、")
	}
	return Pass, detail
}

// 第 4 步：令牌列表的分页格式和字段；再拉一页 100 条作为样本。
func (k *checker) tokens(ctx context.Context) (Level, string) {
	if l, d, ok := k.needAuth(); !ok {
		return l, d
	}
	page, _, err := k.page(ctx, "/api/token/", url.Values{"p": {"1"}, "page_size": {"1"}})
	if err != nil {
		return Fail, err.Error()
	}
	var notes []string
	if !page.paged {
		return Note, "data 是数组，不是 {page, page_size, total, items}：§1 的分页结论和同步代码都要改"
	}
	if page.page != 1 {
		notes = append(notes, fmt.Sprintf("p=1 时 data.page=%d，p 可能从 0 开始", page.page))
	}
	if len(page.items) > 1 {
		notes = append(notes, fmt.Sprintf("page_size=1 却返回 %d 条，page_size 没生效", len(page.items)))
	}
	if !page.hasTotal {
		notes = append(notes, "没有 total")
	}
	detail := fmt.Sprintf("共 %d 个令牌", page.total)
	if len(page.items) > 0 {
		item := page.items[0]
		if missing := missingKeys(item, tokenFields); len(missing) > 0 {
			notes = append(notes, "缺少字段 "+strings.Join(missing, "、"))
		}
		detail += "；字段：" + strings.Join(sortedKeys(item), " ")
		switch key := toString(item["key"]); {
		case key == "":
			detail += "；不返回 key"
		case strings.Contains(key, "*"):
			detail += "；key 已打码"
		default:
			detail += "；**返回完整 key**（解码时不定义这个字段，只存 key_hint）"
		}
	}

	resp, err := k.c.Raw(ctx, "/api/token/", url.Values{"p": {"1"}, "page_size": {"100"}}, true)
	if err == nil {
		k.report.Fixtures["tokens.json"] = k.san.envelope(resp.Body, k.san.tokenPage)
	}
	if len(notes) > 0 {
		return Note, detail + "；" + strings.Join(notes, "；")
	}
	return Pass, detail
}

// 第 5 步：消费日志的字段、token_id、request_id、id 是否为序号。
func (k *checker) logs(ctx context.Context) (Level, string) {
	if l, d, ok := k.needAuth(); !ok {
		return l, d
	}
	page, _, err := k.logPage(ctx, 2, 0, 0, 1, 5)
	if err != nil {
		return Fail, err.Error()
	}
	if !page.paged {
		return Note, "data 是数组，不是 {page, page_size, total, items}：§1 的分页结论和同步代码都要改，第 6～10 步跳过"
	}
	if len(page.items) == 0 {
		return Info, "还没有消费日志，第 6～8、10 步跳过；有调用后再跑一次"
	}
	k.logsOK = true
	k.newestLog, _ = toInt(page.items[0]["created_at"])

	var notes []string
	item := page.items[0]
	if missing := missingKeys(item, logFields); len(missing) > 0 {
		notes = append(notes, "缺少字段 "+strings.Join(missing, "、"))
	}
	detail := "字段：" + strings.Join(sortedKeys(item), " ")
	var ids []string
	zeroTokenID := 0
	for _, it := range page.items {
		ids = append(ids, fmt.Sprint(it["id"]))
		if id, _ := toInt(it["token_id"]); id == 0 {
			zeroTokenID++
		}
	}
	detail += fmt.Sprintf("；%d 条的 id：%s（应是展示序号，不能去重）", len(page.items), strings.Join(ids, ","))
	if zeroTokenID > 0 {
		notes = append(notes, fmt.Sprintf("%d 条 token_id 为 0，要靠 token_name 归属", zeroTokenID))
	}
	if _, ok := item["request_id"]; ok {
		detail += "；有 request_id"
	} else {
		detail += "；没有 request_id"
	}
	other, err := parseOther(item["other"])
	switch {
	case err != nil:
		notes = append(notes, "other 不是 JSON 字符串："+err.Error())
	default:
		detail += "；other 里有：" + strings.Join(presentKeys(other, otherFields), " ")
	}
	if len(notes) > 0 {
		return Note, detail + "；" + strings.Join(notes, "；")
	}
	return Pass, detail
}

// 第 6 步：用一条日志的 created_at 查 [t, t]，判断区间两端是否都含（§4.3 用 end-1 依赖这一点）。
func (k *checker) closedInterval(ctx context.Context) (Level, string) {
	if l, d, ok := k.needLogs(); !ok {
		return l, d
	}
	t := k.newestLog
	found := func(start, end int64) (bool, error) {
		page, _, err := k.logPage(ctx, 2, start, end, 1, 100)
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(page.items, func(it map[string]any) bool {
			v, _ := toInt(it["created_at"])
			return v == t
		}), nil
	}
	ok, err := found(t, t)
	if err != nil {
		return Fail, err.Error()
	}
	if ok {
		return Pass, fmt.Sprintf("start=end=%d 能查到这条日志：两端都含", t)
	}
	startOpen, err1 := found(t-1, t)
	endOpen, err2 := found(t, t+1)
	if err1 != nil || err2 != nil {
		return Fail, fmt.Sprint(err1, err2)
	}
	return Note, fmt.Sprintf("[t, t] 查不到；[t-1, t] 查到=%v，[t, t+1] 查到=%v：§4.3 的区间写法要改", startOpen, endOpen)
}

// 第 7 步：取一个已封闭的窗口，分别用 page_size=100 和 50 拉全，条数、合计和每条内容都应一致。
func (k *checker) pagination(ctx context.Context) (Level, string) {
	if l, d, ok := k.needLogs(); !ok {
		return l, d
	}
	end := k.opts.Now.Add(-k.opts.Lag).Unix()
	start := end - 3600
	first, resp, err := k.logPage(ctx, 2, start, end-1, 1, 100)
	if err != nil {
		return Fail, err.Error()
	}
	if first.total == 0 {
		start = end - 86400
		if first, resp, err = k.logPage(ctx, 2, start, end-1, 1, 100); err != nil {
			return Fail, err.Error()
		}
	}
	if first.total == 0 {
		return Info, "最近 24 小时没有消费日志，第 7、8 步跳过；有调用后再跑一次"
	}
	// 条数太多时按比例缩小窗口，控制请求次数。
	for first.total > maxWindowRows && end-start > 120 {
		start = end - (end-start)*maxWindowRows/first.total
		if first, resp, err = k.logPage(ctx, 2, start, end-1, 1, 100); err != nil {
			return Fail, err.Error()
		}
	}
	k.report.Fixtures["logs.json"] = k.san.envelope(resp.Body, k.san.logPage)

	a, err := k.fetchWindow(ctx, 2, start, end-1, 100, first.total)
	if err != nil {
		return Fail, err.Error()
	}
	b, err := k.fetchWindow(ctx, 2, start, end-1, 50, first.total)
	if err != nil {
		return Fail, err.Error()
	}
	sumA, sumB := sumQuota(a), sumQuota(b)
	k.window, k.windowSum, k.windowRows = [2]int64{start, end - 1}, sumA, len(a)
	detail := fmt.Sprintf("窗口 %s～%s，total=%d；page_size=100 拉到 %d 条、合计 %s；page_size=50 拉到 %d 条、合计 %s",
		k.clock(start), k.clock(end-1), first.total, len(a), k.conv.money(sumA), len(b), k.conv.money(sumB))
	switch {
	case len(a) != len(b) || sumA != sumB || !sameRows(a, b):
		return Note, detail + "：两次结果不同，offset 分页不可靠，§4.3 要改"
	case int64(len(a)) != first.total:
		return Note, detail + "：条数和 total 不同（total 只做参考，同步按「某页不满就结束」）"
	}
	return Pass, detail
}

// 第 8 步：第 7 步窗口的本地合计与上游 stat 一致，每日对账才有意义。
func (k *checker) stat(ctx context.Context) (Level, string) {
	if k.windowRows == 0 {
		return Skip, "第 7 步没有可用的窗口"
	}
	q := url.Values{"type": {"2"}, "start_timestamp": {strconv.FormatInt(k.window[0], 10)},
		"end_timestamp": {strconv.FormatInt(k.window[1], 10)}}
	resp, err := k.c.Raw(ctx, "/api/log/self/stat", q, true)
	if err != nil {
		return Fail, err.Error()
	}
	env, data, err := parseEnvelope(resp)
	if err != nil {
		return Fail, err.Error()
	}
	if !env.Success {
		return Fail, "success=false：" + env.Message
	}
	k.report.Fixtures["log_stat.json"] = mustIndent(resp.Body)
	m, _ := data.(map[string]any)
	quota, ok := toInt(m["quota"])
	if !ok {
		return Note, "data 里没有 quota，字段：" + strings.Join(sortedKeys(m), " ")
	}
	detail := fmt.Sprintf("stat=%s，逐条相加=%s", k.conv.money(quota), k.conv.money(k.windowSum))
	if quota != k.windowSum {
		return Note, detail + "：不一致，每日对账的口径要查清"
	}
	return Pass, detail
}

// 第 9 步：统计近 30 天退款日志（v1 不同步，§4.1），确认 type 参数生效。
func (k *checker) refunds(ctx context.Context) (Level, string) {
	if l, d, ok := k.needAuth(); !ok {
		return l, d
	}
	end := k.opts.Now.Unix()
	start := end - 30*86400
	first, resp, err := k.logPage(ctx, 6, start, end, 1, 100)
	if err != nil {
		return Fail, err.Error()
	}
	if !first.paged {
		return Skip, "分页格式与文档不同，见第 5 步"
	}
	if first.total == 0 {
		return Info, "近 30 天没有退款日志"
	}
	k.report.Fixtures["logs_refund.json"] = k.san.envelope(resp.Body, k.san.logPage)
	rows, err := k.fetchWindow(ctx, 6, start, end, 100, min(first.total, maxRefundPages*100))
	if err != nil {
		return Fail, err.Error()
	}
	if slices.ContainsFunc(rows, func(it map[string]any) bool { t, ok := toInt(it["type"]); return ok && t != 6 }) {
		return Note, "type=6 的查询返回了其他类型的日志：type 参数没生效"
	}
	detail := fmt.Sprintf("近 30 天 %d 条，合计 %s", first.total, k.conv.money(sumQuota(rows)))
	if int64(len(rows)) < first.total {
		detail += fmt.Sprintf("（只统计了前 %d 条）", len(rows))
	}
	return Info, detail + "；数量不可忽略时按 §14 支持退款"
}

// 第 10 步：上游保留多久的日志，决定 backfill_days 能设多大。列表按时间倒序，最后一页就是最早一条。
func (k *checker) earliest(ctx context.Context) (Level, string) {
	if l, d, ok := k.needLogs(); !ok {
		return l, d
	}
	end := k.opts.Now.Unix()
	first, _, err := k.logPage(ctx, 2, 0, end, 1, 1)
	if err != nil {
		return Fail, err.Error()
	}
	if first.total == 0 || len(first.items) == 0 {
		return Info, "没有消费日志"
	}
	last, _, err := k.logPage(ctx, 2, 0, end, first.total, 1)
	if err != nil {
		return Fail, err.Error()
	}
	if len(last.items) == 0 {
		return Note, fmt.Sprintf("total=%d，但第 %d 页是空的：total 不准，换个办法确认", first.total, first.total)
	}
	newest, _ := toInt(first.items[0]["created_at"])
	oldest, _ := toInt(last.items[0]["created_at"])
	if oldest > newest {
		return Note, "最后一页比第一页还新：列表不是按时间倒序，§4.3 要改"
	}
	// 回填从「今天 0 点 − backfill_days 天」开始（§4.3），按自然日相减就是能取全的天数。
	at := time.Unix(oldest, 0).In(k.opts.Location)
	days := int(math.Round(startOfDay(k.opts.Now, k.opts.Location).Sub(startOfDay(at, k.opts.Location)).Hours() / 24))
	return Info, fmt.Sprintf("共 %d 条，最早一条在 %s（%d 天前）：backfill_days 设到 %d 就能取全",
		first.total, at.Format("2006-01-02 15:04"), days, days)
}

// rawPage 是解析后的一页数据。paged 为 false 表示 data 直接是数组（旧版分页）。
type rawPage struct {
	paged    bool
	page     int64
	total    int64
	hasTotal bool
	items    []map[string]any
}

func (k *checker) page(ctx context.Context, path string, q url.Values) (*rawPage, *Response, error) {
	resp, err := k.c.Raw(ctx, path, q, true)
	if err != nil {
		return nil, nil, err
	}
	env, data, err := parseEnvelope(resp)
	if err != nil {
		return nil, nil, err
	}
	if !env.Success {
		return nil, nil, fmt.Errorf("%s 返回 success=false：%s", path, env.Message)
	}
	p := &rawPage{}
	var items []any
	switch d := data.(type) {
	case []any:
		items = d
	case map[string]any:
		p.paged = true
		p.page, _ = toInt(d["page"])
		p.total, p.hasTotal = toInt(d["total"])
		items, _ = d["items"].([]any)
	case nil:
	default:
		return nil, nil, fmt.Errorf("%s 的 data 类型是 %T", path, data)
	}
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			p.items = append(p.items, m)
		}
	}
	return p, resp, nil
}

// logPage 查一页日志；start、end 为 0 时不限。
func (k *checker) logPage(ctx context.Context, typ int, start, end int64, p, size int64) (*rawPage, *Response, error) {
	q := url.Values{"type": {strconv.Itoa(typ)}, "p": {strconv.FormatInt(p, 10)}, "page_size": {strconv.FormatInt(size, 10)}}
	if start > 0 {
		q.Set("start_timestamp", strconv.FormatInt(start, 10))
	}
	if end > 0 {
		q.Set("end_timestamp", strconv.FormatInt(end, 10))
	}
	return k.page(ctx, "/api/log/self", q)
}

// fetchWindow 从第 1 页翻到某页不满为止（§4.3 的规则），total 只用来限制最多翻几页。
func (k *checker) fetchWindow(ctx context.Context, typ int, start, end, size, total int64) ([]map[string]any, error) {
	maxPages := total/size + 2
	var rows []map[string]any
	for p := int64(1); p <= maxPages; p++ {
		page, _, err := k.logPage(ctx, typ, start, end, p, size)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page.items...)
		if int64(len(page.items)) < size {
			return rows, nil
		}
	}
	return rows, nil
}

func startOfDay(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

func (k *checker) clock(ts int64) string {
	return time.Unix(ts, 0).In(k.opts.Location).Format("01-02 15:04:05")
}

// converter 按 /api/status 的参数把 quota 换成显示金额（§4.1）。
type converter struct {
	perUnit float64
	display string
	rate    float64
}

func (c converter) money(quota int64) string {
	if c.perUnit <= 0 {
		return fmt.Sprintf("quota %d", quota)
	}
	switch c.display {
	case "CNY":
		return fmt.Sprintf("¥%.4f", float64(quota)/c.perUnit*c.rate)
	case "USD":
		return fmt.Sprintf("$%.4f", float64(quota)/c.perUnit)
	default:
		return fmt.Sprintf("quota %d", quota)
	}
}

func parseEnvelope(resp *Response) (envelope, any, error) {
	var env envelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return env, nil, fmt.Errorf("HTTP %d，响应不是 JSON", resp.Status)
	}
	if len(env.Data) == 0 {
		return env, nil, nil
	}
	data, err := decodeJSON(env.Data)
	return env, data, err
}

// decodeJSON 用 json.Number 保留整数精度，样本原样写回时不会变成浮点数。
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// parseOther 解析日志的 other 字段：它是 JSON 字符串，要再解析一次（§4.1）。
func parseOther(v any) (map[string]any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("类型是 %T", v)
	}
	if s == "" {
		return map[string]any{}, nil
	}
	parsed, err := decodeJSON([]byte(s))
	if err != nil {
		return nil, err
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("不是 JSON 对象")
	}
	return m, nil
}

func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, true
		}
		if f, err := x.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(x), true
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	}
	return 0, false
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

func sumQuota(rows []map[string]any) int64 {
	var sum int64
	for _, r := range rows {
		q, _ := toInt(r["quota"])
		sum += q
	}
	return sum
}

// sameRows 比较两次拉取的内容是否相同（不看 id：它是展示序号）。
func sameRows(a, b []map[string]any) bool {
	count := map[string]int{}
	sig := func(r map[string]any) string {
		return fmt.Sprint(r["created_at"], "|", r["token_id"], "|", r["token_name"], "|", r["model_name"], "|",
			r["quota"], "|", r["prompt_tokens"], "|", r["completion_tokens"])
	}
	for _, r := range a {
		count[sig(r)]++
	}
	for _, r := range b {
		count[sig(r)]--
	}
	for _, n := range count {
		if n != 0 {
			return false
		}
	}
	return true
}

func missingKeys(m map[string]any, want []string) []string {
	var missing []string
	for _, k := range want {
		if _, ok := m[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}

func presentKeys(m map[string]any, want []string) []string {
	present := []string{}
	for _, k := range want {
		if _, ok := m[k]; ok {
			present = append(present, k)
		}
	}
	if len(present) == 0 {
		return []string{"（都没有）"}
	}
	return present
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
