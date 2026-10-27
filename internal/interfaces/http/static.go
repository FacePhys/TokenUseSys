package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// pageCSP 和 Nginx 的一致（§12.4），另加 frame-ancestors、base-uri、form-action；本机直连调试时同样生效，
// 页面里违反 CSP 的写法（内联脚本、外链资源）在开发时就会暴露。ECharts 的提示框用内联样式，style-src 放开 'unsafe-inline'。
const pageCSP = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// contentTypes 固定常用扩展名的类型，不依赖系统的 mime 表（scratch 镜像里没有）。
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".ico":  "image/x-icon",
}

// staticFile 是一个内嵌的静态文件，启动时读进内存并算好 ETag。
type staticFile struct {
	body  []byte
	etag  string
	ctype string
}

// loadStatic 读入全部静态文件，键是不带开头斜杠的路径（assets/app.js）。只按完整路径查找，不存在路径穿越。
func loadStatic(fsys fs.FS) (map[string]staticFile, error) {
	files := map[string]staticFile{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		ctype, ok := contentTypes[path.Ext(p)]
		if !ok {
			if ctype = mime.TypeByExtension(path.Ext(p)); ctype == "" {
				ctype = "application/octet-stream"
			}
		}
		sum := sha256.Sum256(body)
		files[p] = staticFile{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: ctype}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("读取静态文件: %w", err)
	}
	for _, page := range []string{"index.html", "login.html", "token.html"} {
		if _, ok := files[page]; !ok {
			return nil, fmt.Errorf("静态文件缺少 %s", page)
		}
	}
	return files, nil
}

// registerStatic 注册页面和静态资源（§7）：页面里没有数据，不需要登录；数据都从 /api/v1/* 取，收到 401 时页面跳转登录。
func registerStatic(r *gin.Engine, files map[string]staticFile) {
	page := func(name string) gin.HandlerFunc {
		f := files[name]
		return func(c *gin.Context) { serveStatic(c, f, true) }
	}
	asset := func(dir string) gin.HandlerFunc {
		return func(c *gin.Context) {
			f, ok := files[dir+c.Param("filepath")]
			if !ok {
				notFound(c)
				return
			}
			serveStatic(c, f, false)
		}
	}
	r.GET("/", page("index.html"))
	r.GET("/login", page("login.html"))
	r.GET("/tokens/:id", page("token.html"))
	r.GET("/assets/*filepath", asset("assets"))
	r.GET("/vendor/*filepath", asset("vendor"))
}

// serveStatic 输出静态文件。每次都向服务端确认（no-cache + ETag），升级后浏览器不会用旧脚本。
func serveStatic(c *gin.Context, f staticFile, html bool) {
	h := c.Writer.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", f.etag)
	h.Set("X-Content-Type-Options", "nosniff")
	if html {
		h.Set("Content-Security-Policy", pageCSP)
		h.Set("Referrer-Policy", "same-origin")
	}
	if c.GetHeader("If-None-Match") == f.etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, f.ctype, f.body)
}

// notFound：接口路径返回 JSON，页面路径返回纯文本。
func notFound(c *gin.Context) {
	if strings.HasPrefix(c.Request.URL.Path, "/api/") {
		abort(c, http.StatusNotFound, "not_found", "接口不存在")
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.String(http.StatusNotFound, "页面不存在")
}
