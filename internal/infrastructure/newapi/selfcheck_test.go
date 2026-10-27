package newapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	fakeToken = "secret-access-token-0123456789"
	realKey   = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIjKl"
)

type fakeLog struct {
	CreatedAt int64
	Type      int
	TokenID   int
	TokenName string
	Quota     int64
}

// fakeUpstream 按 New API 的行为实现自检用到的接口：日志按时间倒序、区间两端都含、p 从 1 开始。
type fakeUpstream struct {
	logs []fakeLog
}

func newFakeUpstream(now time.Time) *fakeUpstream {
	u := &fakeUpstream{}
	base := now.Add(-3 * time.Minute).Unix()
	for i := range 230 { // 230 条消费日志，每 20 秒一条，跨 1 小时以上
		u.logs = append(u.logs, fakeLog{CreatedAt: base - int64(i)*20, Type: 2, TokenID: 1 + i%2,
			TokenName: []string{"facephys-prod-chat", "facephys-dev-chat"}[i%2], Quota: int64(1000 + i)})
	}
	u.logs = append(u.logs, fakeLog{CreatedAt: base - 86400*40, Type: 2, TokenID: 1, TokenName: "facephys-prod-chat", Quota: 5})
	u.logs = append(u.logs, fakeLog{CreatedAt: base - 3600, Type: 6, TokenID: 1, TokenName: "facephys-prod-chat", Quota: 300})
	slices.SortFunc(u.logs, func(a, b fakeLog) int { return int(b.CreatedAt - a.CreatedAt) })
	return u
}

func (u *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "", "data": data})
	}
	if r.URL.Path != "/api/status" {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，access token 无效"}`))
			return
		}
		if r.Header.Get("New-Api-User") != "42" {
			_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，未提供 New-Api-User"}`))
			return
		}
	}
	q := r.URL.Query()
	num := func(name string) int64 { v, _ := strconv.ParseInt(q.Get(name), 10, 64); return v }
	switch r.URL.Path {
	case "/api/status":
		reply(map[string]any{"quota_per_unit": 500000, "quota_display_type": "CNY", "usd_exchange_rate": 1, "version": "v-test"})
	case "/api/user/self":
		reply(map[string]any{"id": 42, "username": "alice", "display_name": "Alice", "email": "alice@example.com",
			"access_token": fakeToken, "quota": 1000000, "used_quota": 500000, "request_count": 230, "group": "default"})
	case "/api/token/":
		items := []any{
			map[string]any{"id": 1, "name": "facephys-prod-chat", "key": realKey, "status": 1, "group": "default",
				"remain_quota": 100, "used_quota": 200, "unlimited_quota": false, "expired_time": -1, "created_time": 1,
				"accessed_time": 2, "model_limits_enabled": false, "model_limits": "", "user_id": 42},
			map[string]any{"id": 2, "name": "facephys-dev-chat", "key": realKey[:47] + "Z", "status": 1, "group": "default",
				"remain_quota": 100, "used_quota": 200, "unlimited_quota": true, "expired_time": -1, "created_time": 1,
				"accessed_time": 2, "model_limits_enabled": false, "model_limits": "", "user_id": 42},
		}
		size := num("page_size")
		reply(map[string]any{"page": num("p"), "page_size": size, "total": len(items), "items": items[:min(int(size), len(items))]})
	case "/api/log/self", "/api/log/self/stat":
		typ, start, end := num("type"), num("start_timestamp"), num("end_timestamp")
		var matched []fakeLog
		for _, l := range u.logs {
			if (typ == 0 || int64(l.Type) == typ) && (start == 0 || l.CreatedAt >= start) && (end == 0 || l.CreatedAt <= end) {
				matched = append(matched, l)
			}
		}
		if r.URL.Path == "/api/log/self/stat" {
			var sum int64
			for _, l := range matched {
				sum += l.Quota
			}
			reply(map[string]any{"quota": sum, "rpm": 0, "tpm": 0})
			return
		}
		p, size := max(num("p"), 1), num("page_size")
		var items []any
		for i := (p - 1) * size; i < min(p*size, int64(len(matched))); i++ {
			l := matched[i]
			items = append(items, map[string]any{"id": i + 1, "created_at": l.CreatedAt, "type": l.Type, "user_id": 42,
				"username": "alice", "token_id": l.TokenID, "token_name": l.TokenName, "model_name": "gpt-x", "quota": l.Quota,
				"prompt_tokens": 10, "completion_tokens": 5, "use_time": 1, "is_stream": true, "group": "default",
				"ip": "198.51.100.7", "request_id": fmt.Sprintf("2026%06d", i),
				"other": `{"cache_tokens":3,"frt":120,"request_id":"inner-secret"}`})
		}
		reply(map[string]any{"page": p, "page_size": size, "total": len(matched), "items": items})
	default:
		http.NotFound(w, r)
	}
}

func runSelfCheck(t *testing.T, token string) *SelfCheckReport {
	t.Helper()
	now := time.Date(2026, 10, 27, 15, 0, 0, 0, time.FixedZone("CST", 8*3600))
	srv := httptest.NewServer(newFakeUpstream(now))
	t.Cleanup(srv.Close)
	c := New(Options{BaseURL: srv.URL, AccessToken: token, UserID: 42, Timeout: 5 * time.Second})
	return SelfCheck(context.Background(), c, SelfCheckOptions{Now: now, Lag: 2 * time.Minute, Location: now.Location()})
}

func TestSelfCheckAgainstFakeUpstream(t *testing.T) {
	report := runSelfCheck(t, fakeToken)
	levels := map[int]Level{}
	for _, c := range report.Checks {
		levels[c.Step] = c.Level
		require.NotEqual(t, Fail, c.Level, "%d. %s：%s", c.Step, c.Title, c.Detail)
		require.NotEqual(t, Note, c.Level, "%d. %s：%s", c.Step, c.Title, c.Detail)
	}
	require.Len(t, report.Checks, 10)
	require.Equal(t, Info, levels[2], "缺 New-Api-User 时失败，这一步只是记录")
	require.Equal(t, Pass, levels[6])
	require.Equal(t, Pass, levels[7])
	require.Equal(t, Pass, levels[8])
	require.Contains(t, report.Checks[8].Detail, "近 30 天 1 条")
	require.Contains(t, report.Checks[9].Detail, "（40 天前）：backfill_days 设到 40 就能取全")
	require.Contains(t, report.Checks[3].Detail, "返回完整 key")
	require.False(t, report.Failed())

	for _, name := range []string{"status.json", "user_self.json", "user_self_without_user_header.json", "tokens.json",
		"logs.json", "log_stat.json", "logs_refund.json"} {
		require.Contains(t, report.Fixtures, name)
	}
	all := ""
	for _, data := range report.Fixtures {
		all += string(data)
	}
	for _, secret := range []string{fakeToken, realKey, "alice", "Alice", "facephys-prod-chat", "198.51.100.7", "2026000", "inner-secret"} {
		require.NotContains(t, all, secret)
	}
	// 令牌名在令牌列表和日志里换成同一个假值，归属关系不变。
	tokens := string(report.Fixtures["tokens.json"])
	logs := string(report.Fixtures["logs.json"])
	require.Contains(t, tokens, `"name": "token-1"`)
	require.Contains(t, logs, `"token_name": "token-1"`)
	require.True(t, strings.Contains(logs, `"token_id": 1`) && strings.Contains(logs, `"token_id": 2`))
	require.Contains(t, logs, `\"cache_tokens\":3`, "other 脱敏后仍是 JSON 字符串")
}

func TestSelfCheckStopsWhenAuthFails(t *testing.T) {
	report := runSelfCheck(t, "wrong-token-value")
	require.True(t, report.Failed())
	require.Equal(t, Fail, report.Checks[2].Level)
	for _, c := range report.Checks[3:] {
		require.Equal(t, Skip, c.Level, "%d. %s", c.Step, c.Title)
	}
}
