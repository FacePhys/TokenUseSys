// Package dashboard 是看板查询（开发文档 §6）：令牌列表和区间用量、按天 / 按小时的序列、模型分布、最近请求、
// 导出、总览和健康状态。用量都从 usage_hourly 汇总，金额按上游的显示币种换算，保留 4 位小数。
package dashboard

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
)

// UnattributedName 是未归属用量（token_id = 0，§4.5）的显示名。
const UnattributedName = "未归属"

// OthersName 是前 N 名以外合并的序列名。
const OthersName = "其他"

type Service struct {
	store    port.DashboardStore
	loc      *time.Location
	tzOffset int64
	now      func() time.Time
}

func New(store port.DashboardStore, loc *time.Location, tzOffset int64, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, loc: loc, tzOffset: tzOffset, now: now}
}

// Usage 是一组用量，金额已换算。
type Usage struct {
	Amount           float64 `json:"amount"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
}

func newUsage(t usage.Totals, cur usage.Currency) Usage {
	return Usage{Amount: amount(cur, t.Quota), Requests: t.Requests, PromptTokens: t.PromptTokens,
		CompletionTokens: t.CompletionTokens, CacheReadTokens: t.CacheReadTokens, CacheWriteTokens: t.CacheWriteTokens}
}

// amount 换算金额并保留 4 位小数（§6）。
func amount(cur usage.Currency, quota int64) float64 {
	return math.Round(cur.Amount(quota)*1e4) / 1e4
}

// currency 读取同步器保存的换算参数；还没同步过时金额就是 quota 原值，币种显示 QUOTA。
func (s *Service) currency(ctx context.Context) (usage.Currency, string, error) {
	v, ok, err := s.store.GetKV(ctx, port.KeyCurrency)
	if err != nil || !ok {
		return usage.Currency{}, "QUOTA", err
	}
	var cur usage.Currency
	if json.Unmarshal([]byte(v), &cur) != nil || cur.QuotaPerUnit <= 0 {
		return usage.Currency{}, "QUOTA", nil
	}
	switch cur.DisplayType {
	case "CNY", "USD", "TOKENS":
		return cur, cur.DisplayType, nil
	}
	return cur, "QUOTA", nil
}

// TokenInfo 是令牌的基本信息。未归属（id 0）没有额度和过期时间。
type TokenInfo struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	KeyHint    string   `json:"key_hint"`
	Status     string   `json:"status"`
	Group      string   `json:"group"`
	Unlimited  bool     `json:"unlimited"`
	Remain     *float64 `json:"remain"`     // 无限额度和未归属为 null
	UsedTotal  *float64 `json:"used_total"` // 未归属为 null
	ExpiresAt  *int64   `json:"expires_at"` // 永不过期为 null
	LastUsedAt *int64   `json:"last_used_at"`
}

func tokenInfo(r usage.TokenRecord, cur usage.Currency) TokenInfo {
	info := TokenInfo{ID: r.ID, Name: r.Name, KeyHint: r.KeyHint, Status: r.StatusLabel(), Group: r.Group,
		Unlimited: r.Unlimited}
	if !r.Unlimited && r.Status != usage.StatusUnknown {
		remain := amount(cur, r.RemainQuota)
		info.Remain = &remain
	}
	if r.Status != usage.StatusUnknown {
		used := amount(cur, r.UsedQuota)
		info.UsedTotal = &used
	}
	if r.ExpiredTime > 0 {
		info.ExpiresAt = &r.ExpiredTime
	}
	if r.AccessedTime > 0 {
		info.LastUsedAt = &r.AccessedTime
	}
	return info
}

func unattributedInfo() TokenInfo {
	return TokenInfo{ID: 0, Name: UnattributedName, Status: "unattributed"}
}

// TokenItem 是令牌列表的一项：基本信息加区间用量。
type TokenItem struct {
	TokenInfo
	Usage Usage `json:"usage"`
}

type TokenList struct {
	Currency string `json:"currency"`
	Range    Range  `json:"range"`
	// SyncedAt 是数据截止的时刻（日志同步的游标）。
	SyncedAt int64       `json:"synced_at"`
	Items    []TokenItem `json:"items"`
	Total    Usage       `json:"total"`
}

// Tokens 返回令牌列表和区间用量（§6）：全部未删除的令牌，加上区间内有用量的已删除令牌；
// 有未归属用量时加一行 id 0，合计和柱状图一致。按金额从高到低排。
func (s *Service) Tokens(ctx context.Context, r Range) (TokenList, error) {
	cur, code, err := s.currency(ctx)
	if err != nil {
		return TokenList{}, err
	}
	records, err := s.store.Tokens(ctx)
	if err != nil {
		return TokenList{}, err
	}
	totals, err := s.totalsByToken(ctx, r)
	if err != nil {
		return TokenList{}, err
	}
	list := TokenList{Currency: code, Range: r, Items: []TokenItem{}}
	if list.SyncedAt, err = s.cursor(ctx); err != nil {
		return TokenList{}, err
	}
	seen := map[int64]bool{}
	var sum usage.Totals
	for _, rec := range records {
		t, used := totals[rec.ID]
		seen[rec.ID] = true
		if rec.DeletedAt != 0 && !used {
			continue
		}
		list.Items = append(list.Items, TokenItem{TokenInfo: tokenInfo(rec, cur), Usage: newUsage(t, cur)})
		sum.Add(t)
	}
	for id, t := range totals {
		if seen[id] {
			continue
		}
		info := unattributedInfo()
		if id != 0 { // 日志里有、令牌表里还没有（同步器会补占位行），先按已删除显示
			info = TokenInfo{ID: id, Name: fmt.Sprintf("#%d", id), Status: "deleted"}
		}
		list.Items = append(list.Items, TokenItem{TokenInfo: info, Usage: newUsage(t, cur)})
		sum.Add(t)
	}
	quota := func(it TokenItem) int64 { return totals[it.ID].Quota }
	slices.SortFunc(list.Items, func(a, b TokenItem) int {
		return cmp.Or(cmp.Compare(quota(b), quota(a)), cmp.Compare(a.ID, b.ID))
	})
	list.Total = newUsage(sum, cur)
	return list, nil
}

func (s *Service) totalsByToken(ctx context.Context, r Range) (map[int64]usage.Totals, error) {
	groups, err := s.store.Usage(ctx, port.UsageQuery{Start: r.From(), End: r.To(), ByToken: true})
	if err != nil {
		return nil, err
	}
	m := make(map[int64]usage.Totals, len(groups))
	for _, g := range groups {
		m[g.TokenID] = g.Totals
	}
	return m, nil
}

// TokenDetail 是令牌详情页的基本信息（§7）。
type TokenDetail struct {
	TokenInfo
	Currency    string   `json:"currency"`
	ModelLimits []string `json:"model_limits"` // 未启用模型限制时为空
	CreatedAt   *int64   `json:"created_at"`
	FirstSeenAt int64    `json:"first_seen_at"`
	DeletedAt   *int64   `json:"deleted_at"`
}

// Token 返回令牌基本信息；id 0 是未归属。
func (s *Service) Token(ctx context.Context, id int64) (TokenDetail, error) {
	cur, code, err := s.currency(ctx)
	if err != nil {
		return TokenDetail{}, err
	}
	if id == 0 {
		return TokenDetail{TokenInfo: unattributedInfo(), Currency: code, ModelLimits: []string{}}, nil
	}
	rec, ok, err := s.store.Token(ctx, id)
	if err != nil {
		return TokenDetail{}, err
	}
	if !ok {
		return TokenDetail{}, fmt.Errorf("令牌 %d %w", id, ErrNotFound)
	}
	d := TokenDetail{TokenInfo: tokenInfo(rec, cur), Currency: code, ModelLimits: []string{}, FirstSeenAt: rec.FirstSeenAt}
	for _, m := range strings.Split(rec.ModelLimits, ",") {
		if m = strings.TrimSpace(m); m != "" {
			d.ModelLimits = append(d.ModelLimits, m)
		}
	}
	if rec.CreatedTime > 0 {
		d.CreatedAt = &rec.CreatedTime
	}
	if rec.DeletedAt > 0 {
		d.DeletedAt = &rec.DeletedAt
	}
	return d, nil
}

// SeriesLine 是一条序列，各指标的长度都和 Series.Buckets 一致。TokenID 为 null 是「其他」。
type SeriesLine struct {
	TokenID          *int64    `json:"token_id"`
	Name             string    `json:"name"`
	Amount           []float64 `json:"amount"`
	Requests         []int64   `json:"requests"`
	PromptTokens     []int64   `json:"prompt_tokens"`
	CompletionTokens []int64   `json:"completion_tokens"`
	CacheReadTokens  []int64   `json:"cache_read_tokens"`
	CacheWriteTokens []int64   `json:"cache_write_tokens"`
}

func newLine(id *int64, name string, n int) *SeriesLine {
	return &SeriesLine{TokenID: id, Name: name, Amount: make([]float64, n), Requests: make([]int64, n),
		PromptTokens: make([]int64, n), CompletionTokens: make([]int64, n), CacheReadTokens: make([]int64, n),
		CacheWriteTokens: make([]int64, n)}
}

type Series struct {
	Currency string `json:"currency"`
	Range    Range  `json:"range"`
	Bucket   string `json:"bucket"`
	// Buckets 是每个时间段的标签（2026-10-27，或 2026-10-27 13:00），Starts 是对应的起点（Unix 秒）。
	Buckets []string      `json:"buckets"`
	Starts  []int64       `json:"starts"`
	Series  []*SeriesLine `json:"series"`
}

// ParseBucket 解析 bucket 参数，默认按天。
func ParseBucket(v string) (port.Bucket, error) {
	switch v {
	case "", "day":
		return port.BucketDay, nil
	case "hour":
		return port.BucketHour, nil
	}
	return 0, invalid("bucket 只能是 day 或 hour")
}

func bucketName(b port.Bucket) string {
	if b == port.BucketHour {
		return "hour"
	}
	return "day"
}

// Series 返回按天（或按小时）的用量序列，按令牌拆分（§6）：区间内金额前 top 名各一条，其余合并为「其他」；
// tokenID 非空时只返回这个令牌。没有数据的时间段补 0。
func (s *Service) Series(ctx context.Context, r Range, bucket port.Bucket, top int, tokenID *int64) (Series, error) {
	if bucket == port.BucketHour && r.Days() > MaxHourSeriesDays {
		return Series{}, invalid("按小时查看时日期区间最长 %d 天", MaxHourSeriesDays)
	}
	if top == 0 {
		top = DefaultTop
	}
	if top < 1 || top > MaxTop {
		return Series{}, invalid("top 必须在 1～%d 之间", MaxTop)
	}
	cur, code, err := s.currency(ctx)
	if err != nil {
		return Series{}, err
	}
	groups, err := s.store.Usage(ctx, port.UsageQuery{Start: r.From(), End: r.To(), Bucket: bucket,
		TZOffset: s.tzOffset, ByToken: true, TokenID: tokenID})
	if err != nil {
		return Series{}, err
	}
	names, err := s.tokenNames(ctx)
	if err != nil {
		return Series{}, err
	}

	out := Series{Currency: code, Range: r, Bucket: bucketName(bucket), Buckets: []string{}, Starts: []int64{}, Series: []*SeriesLine{}}
	index := map[int64]int{}
	for t := r.from; t.Before(r.to); t = nextBucket(t, bucket) {
		index[t.Unix()] = len(out.Starts)
		out.Starts = append(out.Starts, t.Unix())
		out.Buckets = append(out.Buckets, bucketLabel(t, bucket))
	}
	n := len(out.Starts)

	// 按区间合计排名，决定哪些令牌单独一条。
	totals := map[int64]int64{}
	for _, g := range groups {
		totals[g.TokenID] += g.Quota
	}
	var ranked []int64
	if tokenID != nil {
		ranked = []int64{*tokenID}
	} else {
		for id := range totals {
			ranked = append(ranked, id)
		}
		slices.SortFunc(ranked, func(a, b int64) int {
			return cmp.Or(cmp.Compare(totals[b], totals[a]), cmp.Compare(a, b))
		})
	}
	lines := map[int64]*SeriesLine{}
	for i, id := range ranked {
		if i == top && tokenID == nil {
			break
		}
		lines[id] = newLine(&id, names.of(id), n)
		out.Series = append(out.Series, lines[id])
	}
	var others *SeriesLine
	for _, g := range groups {
		i, ok := index[g.Bucket]
		if !ok {
			continue
		}
		line := lines[g.TokenID]
		if line == nil {
			if others == nil {
				others = newLine(nil, OthersName, n)
			}
			line = others
		}
		line.Requests[i] += g.Requests
		line.PromptTokens[i] += g.PromptTokens
		line.CompletionTokens[i] += g.CompletionTokens
		line.CacheReadTokens[i] += g.CacheReadTokens
		line.CacheWriteTokens[i] += g.CacheWriteTokens
		// 金额先累加 quota 再换算，避免逐项四舍五入的误差。
		line.Amount[i] += float64(g.Quota)
	}
	if others != nil {
		out.Series = append(out.Series, others)
	}
	for _, line := range out.Series {
		for i, q := range line.Amount {
			line.Amount[i] = amount(cur, int64(q))
		}
	}
	return out, nil
}

func nextBucket(t time.Time, b port.Bucket) time.Time {
	if b == port.BucketHour {
		return t.Add(time.Hour)
	}
	return t.AddDate(0, 0, 1)
}

func bucketLabel(t time.Time, b port.Bucket) string {
	if b == port.BucketHour {
		return t.Format("2006-01-02 15:00")
	}
	return t.Format(usage.DayLayout)
}

type nameMap map[int64]string

func (m nameMap) of(id int64) string {
	if id == 0 {
		return UnattributedName
	}
	if name, ok := m[id]; ok {
		return name
	}
	return fmt.Sprintf("#%d", id)
}

func (s *Service) tokenNames(ctx context.Context) (nameMap, error) {
	records, err := s.store.Tokens(ctx)
	if err != nil {
		return nil, err
	}
	m := make(nameMap, len(records))
	for _, r := range records {
		m[r.ID] = r.Name
	}
	return m, nil
}

// ModelItem 是一个模型的区间合计。
type ModelItem struct {
	Model string `json:"model"`
	Usage
}

type ModelList struct {
	Currency string      `json:"currency"`
	Range    Range       `json:"range"`
	Items    []ModelItem `json:"items"`
	Total    Usage       `json:"total"`
}

// Models 返回按模型的区间合计，按金额从高到低排（§6）；tokenID 非空时只看这个令牌。
func (s *Service) Models(ctx context.Context, r Range, tokenID *int64) (ModelList, error) {
	cur, code, err := s.currency(ctx)
	if err != nil {
		return ModelList{}, err
	}
	groups, err := s.store.Usage(ctx, port.UsageQuery{Start: r.From(), End: r.To(), ByModel: true, TokenID: tokenID})
	if err != nil {
		return ModelList{}, err
	}
	slices.SortFunc(groups, func(a, b port.UsageGroup) int {
		return cmp.Or(cmp.Compare(b.Quota, a.Quota), strings.Compare(a.Model, b.Model))
	})
	list := ModelList{Currency: code, Range: r, Items: []ModelItem{}}
	var sum usage.Totals
	for _, g := range groups {
		list.Items = append(list.Items, ModelItem{Model: g.Model, Usage: newUsage(g.Totals, cur)})
		sum.Add(g.Totals)
	}
	list.Total = newUsage(sum, cur)
	return list, nil
}

// LogItem 是一条请求明细。
type LogItem struct {
	CreatedAt        int64   `json:"created_at"`
	Model            string  `json:"model"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	UseTime          int64   `json:"use_time"` // 秒
	FRTMillis        int64   `json:"frt_ms"`
	IsStream         bool    `json:"is_stream"`
	Amount           float64 `json:"amount"`
	Group            string  `json:"group"`
	RequestID        string  `json:"request_id"`
}

type LogPage struct {
	Currency string    `json:"currency"`
	Page     int       `json:"page"`
	PageSize int       `json:"page_size"`
	Total    int64     `json:"total"`
	Items    []LogItem `json:"items"`
}

// Logs 返回一个令牌的最近请求（只有 90 天内的明细），按时间倒序；r 非空时只看这个区间。
func (s *Service) Logs(ctx context.Context, tokenID int64, r *Range, model string, page, pageSize int) (LogPage, error) {
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = DefaultLogPageSize
	}
	if page < 1 {
		return LogPage{}, invalid("page 必须大于 0")
	}
	if pageSize < 1 || pageSize > MaxLogPageSize {
		return LogPage{}, invalid("page_size 必须在 1～%d 之间", MaxLogPageSize)
	}
	cur, code, err := s.currency(ctx)
	if err != nil {
		return LogPage{}, err
	}
	q := port.LogQuery{TokenID: tokenID, Model: model, Limit: pageSize, Offset: (page - 1) * pageSize}
	if r != nil {
		q.Start, q.End = r.From(), r.To()
	}
	entries, total, err := s.store.Logs(ctx, q)
	if err != nil {
		return LogPage{}, err
	}
	out := LogPage{Currency: code, Page: page, PageSize: pageSize, Total: total, Items: make([]LogItem, 0, len(entries))}
	for _, e := range entries {
		out.Items = append(out.Items, LogItem{CreatedAt: e.CreatedAt, Model: e.ModelName, PromptTokens: e.PromptTokens,
			CompletionTokens: e.CompletionTokens, CacheReadTokens: e.CacheReadTokens, CacheWriteTokens: e.CacheWriteTokens,
			UseTime: e.UseTime, FRTMillis: e.FRTMillis, IsStream: e.IsStream, Amount: amount(cur, e.Quota), Group: e.Group,
			RequestID: e.RequestID})
	}
	return out, nil
}

// ExportRow 是导出的一行：时间段 × 令牌 × 模型。
type ExportRow struct {
	Bucket    string
	TokenID   int64
	TokenName string
	Model     string
	usage.Totals
	Amount float64
}

type ExportTable struct {
	Currency string
	Range    Range
	Bucket   port.Bucket
	Rows     []ExportRow
}

// Export 返回按 日期（或小时）× 令牌 × 模型 的明细，按时间、令牌、模型排序（§6）。
func (s *Service) Export(ctx context.Context, r Range, bucket port.Bucket, tokenID *int64) (ExportTable, error) {
	if bucket == port.BucketHour && r.Days() > MaxHourExportDays {
		return ExportTable{}, invalid("按小时导出时日期区间最长 %d 天", MaxHourExportDays)
	}
	cur, code, err := s.currency(ctx)
	if err != nil {
		return ExportTable{}, err
	}
	groups, err := s.store.Usage(ctx, port.UsageQuery{Start: r.From(), End: r.To(), Bucket: bucket,
		TZOffset: s.tzOffset, ByToken: true, ByModel: true, TokenID: tokenID})
	if err != nil {
		return ExportTable{}, err
	}
	names, err := s.tokenNames(ctx)
	if err != nil {
		return ExportTable{}, err
	}
	t := ExportTable{Currency: code, Range: r, Bucket: bucket, Rows: make([]ExportRow, 0, len(groups))}
	for _, g := range groups {
		t.Rows = append(t.Rows, ExportRow{Bucket: bucketLabel(time.Unix(g.Bucket, 0).In(s.loc), bucket),
			TokenID: g.TokenID, TokenName: names.of(g.TokenID), Model: g.Model, Totals: g.Totals, Amount: amount(cur, g.Quota)})
	}
	return t, nil
}
