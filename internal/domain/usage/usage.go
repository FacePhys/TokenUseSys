// Package usage 是令牌用量的领域模型：令牌、账户、消费日志、金额换算、日志归属和同步窗口的切分规则。
// 不依赖任何外部库。
package usage

import (
	"strings"
	"unicode/utf8"
)

// 上游令牌状态（§4.1）。StatusUnknown 是只在日志里见过、令牌列表里没有的令牌。
const (
	StatusUnknown   = 0
	StatusEnabled   = 1
	StatusDisabled  = 2
	StatusExpired   = 3
	StatusExhausted = 4
)

// Token 是令牌列表里的一项。完整 key 不出 newapi 包，这里只有 KeyHint。
type Token struct {
	ID           int64
	Name         string
	KeyHint      string
	Status       int
	Group        string
	Unlimited    bool
	RemainQuota  int64
	UsedQuota    int64
	ExpiredTime  int64 // -1 永不过期
	CreatedTime  int64
	AccessedTime int64
	ModelLimits  string // 未启用模型限制时为空
}

// Account 是账户余额，只存最新值（§4.4）。
type Account struct {
	Quota        int64 `json:"quota"`
	UsedQuota    int64 `json:"used_quota"`
	RequestCount int64 `json:"request_count"`
	UpdatedAt    int64 `json:"updated_at"`
}

// LogEntry 是一条消费日志（type=2）。日志的 id 是展示序号，不唯一，不保存。
type LogEntry struct {
	CreatedAt        int64
	TokenID          int64 // 0 = 未归属
	TokenName        string
	ModelName        string
	Quota            int64
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	UseTime          int64 // 秒
	FRTMillis        int64 // 首字耗时
	IsStream         bool
	Group            string
	RequestID        string
}

// ReconcileReport 是一天的对账结果（§4.3）。SnapshotQuota 为 nil 表示当天 0 点前还没有令牌快照，无法比较。
type ReconcileReport struct {
	Day           string // 2026-10-26
	UpstreamQuota int64
	LocalQuota    int64
	SnapshotQuota *int64
	CheckedAt     int64
}

// KeyHint 把令牌 key 变成 sk-前4…后4（§8），太短时不提示，避免露出大部分内容。
func KeyHint(key string) string {
	k := strings.TrimPrefix(key, "sk-")
	if utf8.RuneCountInString(k) < 12 {
		return ""
	}
	r := []rune(k)
	return "sk-" + string(r[:4]) + "…" + string(r[len(r)-4:])
}

// Attribute 给日志找令牌（§4.5）：token_id 非 0 时直接用；为 0 时按 token_name 找唯一的令牌；
// 同名令牌无法区分或匹配不上时保持 0，页面显示「未归属」。
func Attribute(entries []LogEntry, idsByName map[string][]int64) {
	for i := range entries {
		e := &entries[i]
		if e.TokenID != 0 || e.TokenName == "" {
			continue
		}
		if ids := idsByName[e.TokenName]; len(ids) == 1 {
			e.TokenID = ids[0]
		}
	}
}

// Currency 是 /api/status 的换算参数，运行时读取，不写死（§1）。
type Currency struct {
	QuotaPerUnit float64 `json:"quota_per_unit"`
	DisplayType  string  `json:"quota_display_type"` // CNY、USD、TOKENS
	ExchangeRate float64 `json:"usd_exchange_rate"`
}

// Amount 按 §4.1 把 quota 换成显示金额；TOKENS 或参数无效时返回 quota 原值。
func (c Currency) Amount(quota int64) float64 {
	if c.QuotaPerUnit <= 0 {
		return float64(quota)
	}
	switch c.DisplayType {
	case "CNY":
		return float64(quota) / c.QuotaPerUnit * c.ExchangeRate
	case "USD":
		return float64(quota) / c.QuotaPerUnit
	default:
		return float64(quota)
	}
}

// Totals 是一组用量的合计。
type Totals struct {
	Requests         int64
	Quota            int64
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Add 把 o 累加进来。
func (t *Totals) Add(o Totals) {
	t.Requests += o.Requests
	t.Quota += o.Quota
	t.PromptTokens += o.PromptTokens
	t.CompletionTokens += o.CompletionTokens
	t.CacheReadTokens += o.CacheReadTokens
	t.CacheWriteTokens += o.CacheWriteTokens
}

// TokenRecord 是本地令牌表的一行。
type TokenRecord struct {
	Token
	FirstSeenAt int64
	UpdatedAt   int64
	DeletedAt   int64 // 0 表示未删除
}

// StatusLabel 返回接口和页面用的状态名（§6）：上游已删除的是 deleted，只在日志里见过的占位令牌也算已删除。
func (r TokenRecord) StatusLabel() string {
	if r.DeletedAt != 0 {
		return "deleted"
	}
	switch r.Status {
	case StatusEnabled:
		return "enabled"
	case StatusDisabled:
		return "disabled"
	case StatusExpired:
		return "expired"
	case StatusExhausted:
		return "exhausted"
	}
	return "unknown"
}
