package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/FacePhys/TokenUseSys/internal/application/dashboard"
	"github.com/FacePhys/TokenUseSys/internal/application/port"
	"github.com/FacePhys/TokenUseSys/internal/domain/usage"
	"github.com/FacePhys/TokenUseSys/internal/infrastructure/sqlite"
)

func init() {
	gin.SetMode(gin.TestMode)
}

var cst = time.FixedZone("CST", 8*3600)

const (
	testUser     = "admin"
	testPassword = "correct horse battery"
	testSecret   = "0123456789abcdef0123456789abcdef"
)

type env struct {
	t    *testing.T
	db   *sqlite.DB
	r    *gin.Engine
	now  time.Time
	logs bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "usage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	e := &env{t: t, db: db, now: time.Date(2026, 10, 27, 12, 0, 0, 0, cst)}
	e.r = e.router(t, testPassword)
	return e
}

func (e *env) router(t *testing.T, password string) *gin.Engine {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	clock := func() time.Time { return e.now }
	r, err := NewRouter(Options{
		Log:       slog.New(slog.NewTextHandler(&e.logs, nil)),
		Dashboard: dashboard.New(e.db.Dashboard(), cst, 8*3600, clock),
		Auth:      AuthOptions{User: testUser, PasswordHash: string(hash), Secret: testSecret, TTL: 12 * time.Hour},
		Now:       clock,
	})
	require.NoError(t, err)
	return r
}

type reqOpt func(*http.Request)

func jsonBody(req *http.Request) { req.Header.Set("Content-Type", "application/json") }

func cookie(c *http.Cookie) reqOpt { return func(req *http.Request) { req.AddCookie(c) } }

func host(h string) reqOpt { return func(req *http.Request) { req.Host = h } }

// from 设置直接连上来的地址和 X-Forwarded-For。
func from(remote, forwarded string) reqOpt {
	return func(req *http.Request) {
		req.RemoteAddr = remote + ":40000"
		if forwarded != "" {
			req.Header.Set("X-Forwarded-For", forwarded)
		}
	}
}

func (e *env) do(r *gin.Engine, method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "usage.example.com"
	req.RemoteAddr = "127.0.0.1:40000" // 经本机 Nginx
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func loginBody(user, password string) string {
	b, _ := json.Marshal(map[string]string{"username": user, "password": password})
	return string(b)
}

func (e *env) login(opts ...reqOpt) *httptest.ResponseRecorder {
	return e.do(e.r, http.MethodPost, "/api/v1/login", loginBody(testUser, testPassword), append([]reqOpt{jsonBody}, opts...)...)
}

func sessionOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("没有会话 Cookie")
	return nil
}

func (e *env) seed() {
	e.t.Helper()
	ctx := context.Background()
	slot := e.now.Add(-time.Hour)
	require.NoError(e.t, e.db.SyncTokens(ctx, []usage.Token{
		{ID: 1, Name: "prod", Status: 1, UsedQuota: 10},
		{ID: 2, Name: "=HYPERLINK(\"x\")", Status: 1},
	}, true, slot, slot))
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, cst).Unix()
	require.NoError(e.t, e.db.ReplaceWindow(ctx, day, day+86400, []usage.LogEntry{
		{CreatedAt: day + 3600, TokenID: 1, ModelName: "gpt-4o", Quota: 1_000_000, PromptTokens: 100, CompletionTokens: 20},
		{CreatedAt: day + 7200, TokenID: 2, ModelName: "claude", Quota: 500_000, PromptTokens: 50, CompletionTokens: 10},
	}, nil))
	for k, v := range map[string]string{
		port.KeyCurrency:      `{"quota_per_unit":500000,"quota_display_type":"CNY","usd_exchange_rate":1}`,
		port.KeyLogsCursor:    fmt.Sprint(e.now.Add(-2 * time.Minute).Unix()),
		port.KeyBackfillUntil: fmt.Sprint(day),
		port.KeyDataSince:     "2026-10-26",
	} {
		require.NoError(e.t, e.db.SetKV(ctx, k, v))
	}
}

func TestLoginSetsSessionCookie(t *testing.T) {
	e := newEnv(t)
	w := e.login()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	c := sessionOf(t, w)
	require.True(t, c.HttpOnly)
	require.True(t, c.Secure, "经域名访问时加 Secure")
	require.Equal(t, http.SameSiteStrictMode, c.SameSite)
	require.Equal(t, "/", c.Path)
	require.Equal(t, 12*3600, c.MaxAge)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.Contains(t, w.Body.String(), `"expires_at":`)

	require.Equal(t, http.StatusOK, e.do(e.r, http.MethodGet, "/api/v1/summary", "", cookie(c)).Code)

	for _, h := range []string{"127.0.0.1:8200", "localhost:8200", "[::1]:8200"} {
		local := sessionOf(t, e.login(host(h)))
		require.False(t, local.Secure, "本机调试 %s 不加 Secure", h)
	}
}

func TestLoginRejectsBadRequests(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		body   string
		opts   []reqOpt
		status int
	}{
		{loginBody(testUser, "wrong"), []reqOpt{jsonBody}, http.StatusUnauthorized},
		{loginBody("root", testPassword), []reqOpt{jsonBody}, http.StatusUnauthorized},
		{"username=admin&password=x", []reqOpt{func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}}, http.StatusUnsupportedMediaType},
		{"{", []reqOpt{jsonBody}, http.StatusBadRequest},
		{`{"username":"admin","password":"` + strings.Repeat("x", 5000) + `"}`, []reqOpt{jsonBody}, http.StatusBadRequest},
	}
	for i, c := range cases {
		w := e.do(e.r, http.MethodPost, "/api/v1/login", c.body, append(c.opts, from(fmt.Sprintf("10.0.0.%d", i+1), ""))...)
		require.Equal(t, c.status, w.Code, "第 %d 个：%s", i, w.Body.String())
		require.Empty(t, w.Result().Cookies())
	}
	require.NotContains(t, e.logs.String(), testPassword, "日志里没有密码")
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t)
	bad := func(opts ...reqOpt) *httptest.ResponseRecorder {
		return e.do(e.r, http.MethodPost, "/api/v1/login", loginBody(testUser, "wrong"), append([]reqOpt{jsonBody}, opts...)...)
	}
	// Nginx 转发的真实 IP：198.51.100.7。
	viaNginx := from("127.0.0.1", "198.51.100.7")
	for range 4 {
		require.Equal(t, http.StatusUnauthorized, bad(viaNginx).Code)
	}
	require.Equal(t, http.StatusUnauthorized, bad(viaNginx).Code, "第 5 次失败，开始锁定")
	w := e.login(viaNginx)
	require.Equal(t, http.StatusTooManyRequests, w.Code, "锁定期间密码对了也不行")
	require.Equal(t, "900", w.Header().Get("Retry-After"))
	require.Equal(t, http.StatusOK, e.login(from("127.0.0.1", "198.51.100.8")).Code, "别的 IP 不受影响")

	// 公网直连时 X-Forwarded-For 不采信，换不同的伪造 IP 也按对端地址累计。
	for i := range 5 {
		require.Equal(t, http.StatusUnauthorized, bad(from("203.0.113.5", fmt.Sprintf("1.2.3.%d", i))).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, e.login(from("203.0.113.5", "9.9.9.9")).Code)

	e.now = e.now.Add(15 * time.Minute)
	require.Equal(t, http.StatusOK, e.login(viaNginx).Code, "15 分钟后解锁")
}

func TestFailuresOutsideWindowDoNotLock(t *testing.T) {
	e := newEnv(t)
	for range 4 {
		e.do(e.r, http.MethodPost, "/api/v1/login", loginBody(testUser, "wrong"), jsonBody)
	}
	e.now = e.now.Add(16 * time.Minute)
	require.Equal(t, http.StatusUnauthorized, e.do(e.r, http.MethodPost, "/api/v1/login", loginBody(testUser, "wrong"), jsonBody).Code)
	require.Equal(t, http.StatusOK, e.login().Code, "15 分钟以前的失败不计")
}

func TestEveryAPIRouteRequiresSession(t *testing.T) {
	e := newEnv(t)
	e.seed()
	c := sessionOf(t, e.login())
	tampered := *c
	tampered.Value = c.Value[:len(c.Value)-2] + "xx"
	forged := &http.Cookie{Name: sessionCookie, Value: "YWRtaW58OTk5OTk5OTk5OQ.c2lnbmF0dXJl"}

	checked := 0
	for _, rt := range e.r.Routes() {
		if !strings.HasPrefix(rt.Path, "/api/v1/") || rt.Path == "/api/v1/login" || rt.Path == "/api/v1/logout" {
			continue
		}
		path := strings.ReplaceAll(rt.Path, ":id", "1")
		require.Equal(t, http.StatusUnauthorized, e.do(e.r, rt.Method, path, "").Code, "没登录：%s", path)
		require.Equal(t, http.StatusUnauthorized, e.do(e.r, rt.Method, path, "", cookie(&tampered)).Code, "篡改：%s", path)
		require.Equal(t, http.StatusUnauthorized, e.do(e.r, rt.Method, path, "", cookie(forged)).Code, "伪造：%s", path)
		require.Equal(t, http.StatusOK, e.do(e.r, rt.Method, path, "", cookie(c)).Code, "登录后：%s", path)
		checked++
	}
	require.Equal(t, 7, checked)

	e.now = e.now.Add(12 * time.Hour)
	require.Equal(t, http.StatusUnauthorized, e.do(e.r, http.MethodGet, "/api/v1/summary", "", cookie(c)).Code, "12 小时后过期")
}

func TestPasswordChangeInvalidatesSessions(t *testing.T) {
	e := newEnv(t)
	c := sessionOf(t, e.login())
	changed := e.router(t, "another password")
	require.Equal(t, http.StatusUnauthorized, e.do(changed, http.MethodGet, "/api/v1/summary", "", cookie(c)).Code)
}

func TestLogoutClearsCookie(t *testing.T) {
	e := newEnv(t)
	w := e.do(e.r, http.MethodPost, "/api/v1/logout", "")
	require.Equal(t, http.StatusNoContent, w.Code)
	c := sessionOf(t, w)
	require.Empty(t, c.Value)
	require.Negative(t, c.MaxAge)
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	w := e.do(e.r, http.MethodGet, "/healthz", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "还没同步过")
	e.seed()
	w = e.do(e.r, http.MethodGet, "/healthz", "")
	require.Equal(t, http.StatusOK, w.Code)
	var h dashboard.Health
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &h))
	require.True(t, h.OK)
	require.Equal(t, int64(120), h.LagSeconds)
}

func TestAPIResponsesAndErrors(t *testing.T) {
	e := newEnv(t)
	e.seed()
	c := cookie(sessionOf(t, e.login()))

	w := e.do(e.r, http.MethodGet, "/api/v1/tokens?start=2026-10-26&end=2026-10-26", "", c)
	require.Equal(t, http.StatusOK, w.Code)
	var list dashboard.TokenList
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Equal(t, "CNY", list.Currency)
	require.Len(t, list.Items, 2)
	require.InDelta(t, 3.0, list.Total.Amount, 1e-9)

	w = e.do(e.r, http.MethodGet, "/api/v1/usage/series?start=2026-10-25&end=2026-10-26&top=1", "", c)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"buckets":["2026-10-25","2026-10-26"]`)
	require.Contains(t, w.Body.String(), `"name":"其他"`)
	w = e.do(e.r, http.MethodGet, "/api/v1/usage/series?start=2026-10-25&end=2026-10-26&top=all", "", c)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), `"name":"其他"`, "top=all 时每个令牌单独一条")
	w = e.do(e.r, http.MethodGet, "/api/v1/usage/series?top=-1", "", c)
	require.Equal(t, http.StatusBadRequest, w.Code, "负数不能当成 all")

	w = e.do(e.r, http.MethodGet, "/api/v1/tokens/1/logs?page_size=1", "", c)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"total":1`)

	for path, status := range map[string]int{
		"/api/v1/tokens?start=2026-10-27&end=2026-10-01":                 http.StatusBadRequest,
		"/api/v1/tokens/abc":                                             http.StatusBadRequest,
		"/api/v1/tokens/99":                                              http.StatusNotFound,
		"/api/v1/tokens/1/logs?page_size=1000":                           http.StatusBadRequest,
		"/api/v1/usage/series?bucket=week":                               http.StatusBadRequest,
		"/api/v1/usage/series?bucket=hour&start=2026-10-01":              http.StatusBadRequest,
		"/api/v1/usage/models?token_id=-1":                               http.StatusBadRequest,
		"/api/v1/export.csv?bucket=hour&start=2026-01-01&end=2026-10-27": http.StatusBadRequest,
		"/api/v1/nope": http.StatusNotFound,
	} {
		w := e.do(e.r, http.MethodGet, path, "", c)
		require.Equal(t, status, w.Code, path)
		var body apiError
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), path)
		require.NotEmpty(t, body.Message, path)
	}
}

func TestExportCSV(t *testing.T) {
	e := newEnv(t)
	e.seed()
	w := e.do(e.r, http.MethodGet, "/api/v1/export.csv?start=2026-10-26&end=2026-10-26", "", cookie(sessionOf(t, e.login())))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "text/csv; charset=utf-8", w.Header().Get("Content-Type"))
	require.Equal(t, `attachment; filename="relayusage-2026-10-26_2026-10-26.csv"`, w.Header().Get("Content-Disposition"))
	body := w.Body.String()
	require.True(t, strings.HasPrefix(body, "\uFEFF日期,令牌 ID,令牌,模型,"), "带 BOM 和表头")
	require.Contains(t, body, "金额（CNY）")
	require.Contains(t, body, "2026-10-26,1,prod,gpt-4o,1,100,20,0,0,2.0000")
	require.Contains(t, body, `2026-10-26,2,"'=HYPERLINK(""x"")",claude`, "公式开头的单元格加单引号")
}

func TestNoAutomaticRedirects(t *testing.T) {
	e := newEnv(t)
	c := cookie(sessionOf(t, e.login()))
	for _, path := range []string{"/api/v1/summary/", "/API/v1/summary", "/healthz/"} {
		w := e.do(e.r, http.MethodGet, path, "", c)
		require.Equal(t, http.StatusNotFound, w.Code, path)
		require.Empty(t, w.Header().Get("Location"), path)
	}
}

func TestPanicIsLoggedNotPrinted(t *testing.T) {
	e := newEnv(t)
	e.r.GET("/boom", func(*gin.Context) { panic("出错了") })
	w := e.do(e.r, http.MethodGet, "/boom", "", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "secret-cookie"}) })
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, e.logs.String(), "处理请求时发生 panic")
	require.NotContains(t, e.logs.String(), "secret-cookie", "不记请求头")
}
