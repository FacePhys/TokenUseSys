package newapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

var _ port.Upstream = (*Client)(nil)

// maxTokenPages 防止翻页停不下来（比如上游忽略 p，每页内容都一样）：每页 100 条时即 1 万个令牌。
const maxTokenPages = 100

// Currency 返回 /api/status 的换算参数（§4.1）。
func (c *Client) Currency(ctx context.Context) (usage.Currency, error) {
	m, err := c.object(ctx, "/api/status", nil)
	if err != nil {
		return usage.Currency{}, err
	}
	r := c.reader("/api/status")
	defer r.warn()
	return usage.Currency{
		QuotaPerUnit: r.float(m, "quota_per_unit"),
		DisplayType:  r.str(m, "quota_display_type"),
		ExchangeRate: r.float(m, "usd_exchange_rate"),
	}, nil
}

// Account 返回账户余额。
func (c *Client) Account(ctx context.Context) (usage.Account, error) {
	m, err := c.object(ctx, "/api/user/self", nil)
	if err != nil {
		return usage.Account{}, err
	}
	r := c.reader("/api/user/self")
	defer r.warn()
	return usage.Account{
		Quota:        r.int(m, "quota"),
		UsedQuota:    r.int(m, "used_quota"),
		RequestCount: r.int(m, "request_count"),
	}, nil
}

// Tokens 按页拉全部令牌。完整 key 只在这里读一次，转成 key_hint，不出这个包（§8）。
// 翻页规则同日志（§4.3、§4.4）：某页为空，或某页不满 100 条且已拉到的条数不少于 total 时结束。
func (c *Client) Tokens(ctx context.Context) ([]usage.Token, error) {
	r := c.reader("/api/token/")
	defer r.warn()
	var tokens []usage.Token
	for p := 1; p <= maxTokenPages; p++ {
		q := url.Values{"p": {strconv.Itoa(p)}, "page_size": {strconv.Itoa(usage.PageSize)}}
		items, total, err := c.page(ctx, "/api/token/", q)
		if err != nil {
			return tokens, err
		}
		for _, m := range items {
			limits := ""
			if r.boolean(m, "model_limits_enabled") {
				limits = r.str(m, "model_limits")
			}
			tokens = append(tokens, usage.Token{
				ID:           r.int(m, "id"),
				Name:         r.str(m, "name"),
				KeyHint:      usage.KeyHint(r.str(m, "key")),
				Status:       int(r.int(m, "status")),
				Group:        r.str(m, "group"),
				Unlimited:    r.boolean(m, "unlimited_quota"),
				RemainQuota:  r.int(m, "remain_quota"),
				UsedQuota:    r.int(m, "used_quota"),
				ExpiredTime:  r.int(m, "expired_time"),
				CreatedTime:  r.int(m, "created_time"),
				AccessedTime: r.int(m, "accessed_time"),
				ModelLimits:  limits,
			})
		}
		// 不满一页不一定是最后一页：上游把 page_size 压到 100 以下时，只看这一条会漏掉后面的令牌，
		// 调用方还会把它们标成已删除。拉到的条数比 total 少就接着翻；total 只做参考，某页为空就结束。
		if len(items) == 0 || (len(items) < usage.PageSize && int64(len(tokens)) >= total) {
			return tokens, nil
		}
	}
	return tokens, fmt.Errorf("newapi: 令牌超过 %d 页，没有拉完", maxTokenPages)
}

// LogsPage 查一页消费日志（type=2），区间 [start, end] 两端都含。
func (c *Client) LogsPage(ctx context.Context, start, end int64, page int) ([]usage.LogEntry, int64, error) {
	q := url.Values{
		"type": {"2"}, "p": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(usage.PageSize)},
		"start_timestamp": {strconv.FormatInt(start, 10)}, "end_timestamp": {strconv.FormatInt(end, 10)},
	}
	items, total, err := c.page(ctx, "/api/log/self", q)
	if err != nil {
		return nil, 0, err
	}
	r := c.reader("/api/log/self")
	defer r.warn()
	entries := make([]usage.LogEntry, 0, len(items))
	for _, m := range items {
		e := usage.LogEntry{
			CreatedAt:        r.int(m, "created_at"),
			TokenID:          r.int(m, "token_id"),
			TokenName:        r.str(m, "token_name"),
			ModelName:        r.str(m, "model_name"),
			Quota:            r.int(m, "quota"),
			PromptTokens:     r.int(m, "prompt_tokens"),
			CompletionTokens: r.int(m, "completion_tokens"),
			UseTime:          r.int(m, "use_time"),
			IsStream:         r.boolean(m, "is_stream"),
			Group:            r.str(m, "group"),
			RequestID:        r.optionalStr(m, "request_id"),
		}
		// other 是 JSON 字符串，只取缓存读、缓存写和首字耗时，其余丢弃（§4.1）。
		if s := r.optionalStr(m, "other"); s != "" {
			if other, err := parseOther(s); err != nil {
				r.bad["other"] = true
			} else {
				e.CacheReadTokens = r.optionalInt(other, "cache_tokens")
				e.CacheWriteTokens = r.optionalInt(other, "cache_creation_tokens")
				e.FRTMillis = r.optionalInt(other, "frt")
			}
		}
		entries = append(entries, e)
	}
	return entries, total, nil
}

// LogStat 返回 [start, end] 内消费日志的 quota 合计。
func (c *Client) LogStat(ctx context.Context, start, end int64) (int64, error) {
	q := url.Values{"type": {"2"}, "start_timestamp": {strconv.FormatInt(start, 10)}, "end_timestamp": {strconv.FormatInt(end, 10)}}
	m, err := c.object(ctx, "/api/log/self/stat", q)
	if err != nil {
		return 0, err
	}
	r := c.reader("/api/log/self/stat")
	defer r.warn()
	return r.int(m, "quota"), nil
}

// object 请求 path，返回 data 对象。
func (c *Client) object(ctx context.Context, path string, q url.Values) (map[string]any, error) {
	var raw json.RawMessage
	if err := c.get(ctx, path, q, &raw); err != nil {
		return nil, err
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("newapi: 解析 %s 的 data：%w", path, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("newapi: %s 的 data 不是对象", path)
	}
	return m, nil
}

// page 请求分页接口，data 必须是 {page, page_size, total, items}（§1）；旧版的数组格式直接报错，不猜。
func (c *Client) page(ctx context.Context, path string, q url.Values) ([]map[string]any, int64, error) {
	m, err := c.object(ctx, path, q)
	if err != nil {
		return nil, 0, err
	}
	raw, ok := m["items"]
	if !ok {
		return nil, 0, fmt.Errorf("newapi: %s 的 data 没有 items，分页格式与开发文档 §1 不同", path)
	}
	list, _ := raw.([]any) // items 为 null 时是空页
	items := make([]map[string]any, 0, len(list))
	for _, it := range list {
		if obj, ok := it.(map[string]any); ok {
			items = append(items, obj)
		}
	}
	total, _ := toInt(m["total"])
	return items, total, nil
}

// fieldReader 从解码后的 JSON 对象里取值：缺字段或类型不对时给零值，记下字段名，
// 处理完一批后写一条 warning（§4.7），不让一个字段的变化中断同步。
type fieldReader struct {
	log  *slog.Logger
	path string
	bad  map[string]bool
}

func (c *Client) reader(path string) *fieldReader {
	return &fieldReader{log: c.log, path: path, bad: map[string]bool{}}
}

func (r *fieldReader) warn() {
	if len(r.bad) == 0 || r.log == nil {
		return
	}
	fields := make([]string, 0, len(r.bad))
	for f := range r.bad {
		fields = append(fields, f)
	}
	slices.Sort(fields)
	r.log.Warn("上游字段缺失或类型不对，按零值处理", "path", r.path, "fields", fields)
}

func (r *fieldReader) int(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok {
		r.bad[key] = true
		return 0
	}
	if i, ok := toInt(v); ok {
		return i
	}
	if s, ok := v.(string); ok {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
	}
	if v != nil {
		r.bad[key] = true
	}
	return 0
}

// optionalInt 用于不一定出现的字段（other 里的缓存和首字耗时只在有时才写），缺了不告警，类型不对才告警。
func (r *fieldReader) optionalInt(m map[string]any, key string) int64 {
	if _, ok := m[key]; !ok {
		return 0
	}
	return r.int(m, key)
}

func (r *fieldReader) float(m map[string]any, key string) float64 {
	v, ok := m[key]
	if f, isNum := toFloat(v); ok && isNum {
		return f
	}
	r.bad[key] = true
	return 0
}

func (r *fieldReader) str(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		r.bad[key] = true
		return ""
	}
	return r.anyStr(key, v)
}

// optionalStr 用于魔改版可能没有的字段（request_id、other），缺了不告警。
func (r *fieldReader) optionalStr(m map[string]any, key string) string {
	return r.anyStr(key, m[key])
}

func (r *fieldReader) anyStr(key string, v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	}
	r.bad[key] = true
	return ""
}

func (r *fieldReader) boolean(m map[string]any, key string) bool {
	switch x := m[key].(type) {
	case bool:
		return x
	case json.Number:
		return x.String() != "0"
	case string:
		b, _ := strconv.ParseBool(x)
		return b
	case nil:
		if _, ok := m[key]; !ok {
			r.bad[key] = true
		}
		return false
	}
	r.bad[key] = true
	return false
}
