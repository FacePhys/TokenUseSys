package newapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
)

func newUpstreamClient(t *testing.T, h http.HandlerFunc) (*Client, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	c := New(Options{BaseURL: srv.URL, AccessToken: "t", UserID: 1, Timeout: 5 * time.Second,
		Log: slog.New(slog.NewTextHandler(&logs, nil))})
	return c, &logs
}

func ok(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "", "data": data})
}

func TestTokensPagesUntilShortPage(t *testing.T) {
	c, logs := newUpstreamClient(t, func(w http.ResponseWriter, r *http.Request) {
		p, _ := strconv.Atoi(r.URL.Query().Get("p"))
		assert.Equal(t, "100", r.URL.Query().Get("page_size"))
		n := map[int]int{1: 100, 2: 5}[p]
		var items []any
		for i := range n {
			id := (p-1)*100 + i + 1
			items = append(items, map[string]any{"id": id, "name": fmt.Sprintf("tok-%d", id),
				"key": "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGh9xYz", "status": 1, "group": "default",
				"remain_quota": 10, "used_quota": 20, "unlimited_quota": id == 1, "expired_time": -1,
				"created_time": 1, "accessed_time": 2, "model_limits_enabled": id == 2, "model_limits": "gpt-4o,claude"})
		}
		ok(w, map[string]any{"page": p, "page_size": 100, "total": 105, "items": items})
	})
	tokens, err := c.Tokens(context.Background())
	require.NoError(t, err)
	require.Len(t, tokens, 105)
	require.Equal(t, "sk-AbCd…9xYz", tokens[0].KeyHint)
	require.True(t, tokens[0].Unlimited)
	require.Empty(t, tokens[0].ModelLimits, "没启用模型限制时不存")
	require.Equal(t, "gpt-4o,claude", tokens[1].ModelLimits)
	require.Equal(t, int64(-1), tokens[2].ExpiredTime)
	require.Empty(t, logs.String(), "字段齐全时不告警")
}

func TestTokensReturnsPartialOnFailure(t *testing.T) {
	c, _ := newUpstreamClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("p") == "2" {
			_, _ = w.Write([]byte(`{"success":false,"message":"数据库错误"}`))
			return
		}
		items := make([]any, 100)
		for i := range items {
			items[i] = map[string]any{"id": i + 1, "name": "x"}
		}
		ok(w, map[string]any{"page": 1, "total": 150, "items": items})
	})
	tokens, err := c.Tokens(context.Background())
	require.Error(t, err)
	require.Len(t, tokens, 100, "返回已拉到的令牌，由调用方决定不标删除")
}

// 上游把 page_size 压到 100 以下时，第 1 页不满 100 条也不是最后一页：只拉第 1 页的话，
// 后面的令牌会被当成已删除（§4.4）。
func TestTokensKeepsPagingWhenPageSizeCapped(t *testing.T) {
	var calls atomic.Int32
	c, _ := newUpstreamClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		p, _ := strconv.Atoi(r.URL.Query().Get("p"))
		var items []any
		for id := (p-1)*20 + 1; id <= min(p*20, 30); id++ {
			items = append(items, map[string]any{"id": id, "name": fmt.Sprintf("tok-%d", id)})
		}
		ok(w, map[string]any{"page": p, "page_size": 20, "total": 30, "items": items})
	})
	tokens, err := c.Tokens(context.Background())
	require.NoError(t, err)
	require.Len(t, tokens, 30)
	require.Equal(t, int64(30), tokens[29].ID)
	require.Equal(t, int32(2), calls.Load(), "拉够 total 就停，不多翻一页")
}

func TestTokensStopRule(t *testing.T) {
	for name, tc := range map[string]struct {
		total any // nil 表示不返回 total
		calls int32
	}{
		"total 偏大时翻到空页为止":   {total: 5, calls: 2},
		"没有 total 时不满一页就结束": {total: nil, calls: 1},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := newUpstreamClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var items []any // 第 2 页起 items 是 null
				if r.URL.Query().Get("p") == "1" {
					items = []any{map[string]any{"id": 1, "name": "a"}, map[string]any{"id": 2, "name": "b"}}
				}
				data := map[string]any{"page": 1, "items": items}
				if tc.total != nil {
					data["total"] = tc.total
				}
				ok(w, data)
			})
			tokens, err := c.Tokens(context.Background())
			require.NoError(t, err)
			require.Len(t, tokens, 2)
			require.Equal(t, tc.calls, calls.Load())
		})
	}
}

func TestLogsPageParsesFieldsLeniently(t *testing.T) {
	c, logs := newUpstreamClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		assert.Equal(t, "2", q.Get("type"))
		assert.Equal(t, "1000", q.Get("start_timestamp"))
		assert.Equal(t, "1999", q.Get("end_timestamp"))
		assert.Equal(t, "3", q.Get("p"))
		ok(w, map[string]any{"page": 3, "page_size": 100, "total": 202, "items": []any{
			map[string]any{"id": 1, "created_at": 1500, "type": 2, "token_id": 7, "token_name": "a", "model_name": "m",
				"quota": 1234, "prompt_tokens": 10, "completion_tokens": 5, "use_time": 3, "is_stream": true, "group": "g",
				"request_id": "r1", "other": `{"cache_tokens":8,"cache_creation_tokens":2,"frt":321.7}`},
			map[string]any{"id": 2, "created_at": "1600", "type": 2, "token_id": 0, "token_name": "b", "model_name": "m",
				"quota": "oops", "prompt_tokens": nil, "completion_tokens": 1, "use_time": 1, "is_stream": 0,
				"group": "g", "other": "not-json"},
			map[string]any{"id": 3, "created_at": 1700, "type": 2, "token_id": 1, "token_name": "a", "model_name": "m",
				"quota": 1, "prompt_tokens": 1, "completion_tokens": 1, "use_time": 1, "is_stream": false, "group": "g",
				"other": `{"cache_tokens":4}`},
		}})
	})
	entries, total, err := c.LogsPage(context.Background(), 1000, 1999, 3)
	require.NoError(t, err)
	require.Equal(t, int64(202), total)
	require.Len(t, entries, 3)
	e := entries[0]
	require.Equal(t, int64(1500), e.CreatedAt)
	require.Equal(t, int64(1234), e.Quota)
	require.Equal(t, int64(8), e.CacheReadTokens)
	require.Equal(t, int64(2), e.CacheWriteTokens)
	require.Equal(t, int64(321), e.FRTMillis)
	require.True(t, e.IsStream)
	require.Equal(t, "r1", e.RequestID)

	e = entries[1]
	require.Equal(t, int64(1600), e.CreatedAt, "数字写成字符串也能解析")
	require.Zero(t, e.Quota, "类型不对给零值")
	require.False(t, e.IsStream)
	require.Contains(t, logs.String(), "上游字段缺失或类型不对")
	require.Contains(t, logs.String(), "quota")
	require.Contains(t, logs.String(), "other")
	require.NotContains(t, logs.String(), "request_id", "魔改版可能没有 request_id，缺了不告警")
	require.Equal(t, int64(4), entries[2].CacheReadTokens)
	require.NotContains(t, logs.String(), "cache_creation_tokens", "other 里的缓存字段只在有缓存时才有，缺了不告警")
}

func TestLogsPageRejectsArrayFormat(t *testing.T) {
	c, _ := newUpstreamClient(t, func(w http.ResponseWriter, _ *http.Request) { ok(w, []any{}) })
	_, _, err := c.LogsPage(context.Background(), 1, 2, 1)
	require.ErrorContains(t, err, "不是对象")
}

func TestCurrencyAccountAndStat(t *testing.T) {
	c, _ := newUpstreamClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			ok(w, map[string]any{"quota_per_unit": 500000, "quota_display_type": "CNY", "usd_exchange_rate": 1})
		case "/api/user/self":
			ok(w, map[string]any{"quota": 9, "used_quota": 8, "request_count": 7})
		case "/api/log/self/stat":
			assert.Equal(t, "2", r.URL.Query().Get("type"))
			ok(w, map[string]any{"quota": 4242, "rpm": 0, "tpm": 0})
		}
	})
	ctx := context.Background()
	cur, err := c.Currency(ctx)
	require.NoError(t, err)
	require.InDelta(t, 2.0, cur.Amount(1000000), 1e-9)
	acc, err := c.Account(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(9), acc.Quota)
	sum, err := c.LogStat(ctx, 1, 2)
	require.NoError(t, err)
	require.Equal(t, int64(4242), sum)
}

func TestAuthErrorIsPortError(t *testing.T) {
	c, _ := newUpstreamClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，access token 无效"}`))
	})
	_, err := c.Account(context.Background())
	require.ErrorIs(t, err, port.ErrUpstreamAuth)
}
