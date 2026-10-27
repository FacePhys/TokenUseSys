package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/FacePhys/TokenUseSys/internal/application/dashboard"
	"github.com/FacePhys/TokenUseSys/web"
)

func (e *env) webRouter(t *testing.T) *gin.Engine {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	require.NoError(t, err)
	r, err := NewRouter(Options{
		Log:       slog.New(slog.NewTextHandler(&e.logs, nil)),
		Dashboard: dashboard.New(e.db.Dashboard(), cst, 8*3600, func() time.Time { return e.now }),
		Auth:      AuthOptions{User: testUser, PasswordHash: string(hash), Secret: testSecret, TTL: time.Hour},
		Web:       web.FS,
	})
	require.NoError(t, err)
	return r
}

func TestPagesServedWithoutLogin(t *testing.T) {
	e := newEnv(t)
	r := e.webRouter(t)
	for path, script := range map[string]string{
		"/":          "/assets/overview.js",
		"/login":     "/assets/login.js",
		"/tokens/12": "/assets/token.js",
	} {
		w := e.do(r, http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, w.Code, path)
		require.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"), path)
		require.Equal(t, pageCSP, w.Header().Get("Content-Security-Policy"), path)
		require.Equal(t, "no-cache", w.Header().Get("Cache-Control"), path)
		require.NotEmpty(t, w.Header().Get("ETag"), path)
		require.Contains(t, w.Body.String(), `src="`+script+`"`, path)
	}
}

func TestAssetsAndCaching(t *testing.T) {
	e := newEnv(t)
	r := e.webRouter(t)
	w := e.do(r, http.MethodGet, "/assets/app.js", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "text/javascript; charset=utf-8", w.Header().Get("Content-Type"))
	require.Empty(t, w.Header().Get("Content-Security-Policy"), "CSP 只加在页面上")
	etag := w.Header().Get("ETag")

	w = e.do(r, http.MethodGet, "/assets/app.js", "", func(req *http.Request) { req.Header.Set("If-None-Match", etag) })
	require.Equal(t, http.StatusNotModified, w.Code)
	require.Empty(t, w.Body.String())

	w = e.do(r, http.MethodGet, "/vendor/echarts.min.js", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Greater(t, w.Body.Len(), 500_000)
	require.Equal(t, "text/css; charset=utf-8", e.do(r, http.MethodGet, "/assets/styles.css", "").Header().Get("Content-Type"))

	for _, path := range []string{"/assets/", "/assets/../embed.go", "/assets/nope.js", "/embed.go", "/vendor", "/index.html"} {
		w := e.do(r, http.MethodGet, path, "")
		require.Equal(t, http.StatusNotFound, w.Code, path)
		require.NotContains(t, w.Body.String(), "package web", path)
	}
	require.Equal(t, "application/json; charset=utf-8", e.do(r, http.MethodGet, "/api/v1/nope", "").Header().Get("Content-Type"),
		"接口路径的 404 仍是 JSON")
}

// 页面只引用本站的脚本和样式，没有内联脚本和内联事件（CSP 只允许 'self'），引用的文件都存在。
func TestPagesComplyWithCSP(t *testing.T) {
	ref := regexp.MustCompile(`<(?:script[^>]*\ssrc|link[^>]*\shref)="([^"]+)"`)
	scripts := regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
	inlineHandler := regexp.MustCompile(`\son[a-z]+\s*=`)
	pages, err := fs.Glob(web.FS, "*.html")
	require.NoError(t, err)
	require.Len(t, pages, 3)
	for _, page := range pages {
		b, err := fs.ReadFile(web.FS, page)
		require.NoError(t, err)
		html := string(b)
		for _, m := range scripts.FindAllStringSubmatch(html, -1) {
			require.Empty(t, strings.TrimSpace(m[1]), "%s 有内联脚本", page)
		}
		require.False(t, inlineHandler.MatchString(html), "%s 有内联事件", page)
		for _, m := range ref.FindAllStringSubmatch(html, -1) {
			target := m[1]
			if strings.HasPrefix(target, "data:") {
				continue
			}
			require.True(t, strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//"), "%s 引用了外部资源 %s", page, target)
			_, err := fs.Stat(web.FS, strings.TrimPrefix(target, "/"))
			require.NoError(t, err, "%s 引用的 %s 不存在", page, target)
		}
	}
}
