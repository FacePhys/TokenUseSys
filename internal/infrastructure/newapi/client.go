// Package newapi 是 4router（New API 魔改版）的只读客户端（开发文档 §4.2）。
//
// 旧版 access token 拥有账号全部权限，所以客户端只发 GET、只允许白名单路径、不跟随跳转。
// 错误信息里只有路径和状态码，不含请求头。
package newapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
)

// allowedPaths 是只读白名单：旧版 access token 拥有账号全部权限，客户端只允许调这些 GET。
var allowedPaths = map[string]bool{
	"/api/status": true, "/api/user/self": true, "/api/token/": true,
	"/api/log/self": true, "/api/log/self/stat": true,
}

// ErrAuth 表示 access token 失效或用户 ID 不对，同步器收到后暂停 30 分钟并告警。
// 它就是 port.ErrUpstreamAuth，应用层不用依赖这个包也能判断。
var ErrAuth = port.ErrUpstreamAuth

const (
	// maxBodyBytes 是单个响应体的上限，一页 100 条日志远小于它。
	maxBodyBytes = 8 << 20
	// maxRetries 是 429 和 5xx 的重试次数，等待时间依次是 backoff、2×backoff、4×backoff。
	maxRetries     = 3
	defaultBackoff = 5 * time.Second
)

type Options struct {
	BaseURL     string
	AccessToken string
	UserID      int
	Timeout     time.Duration
	// MinInterval 是两次请求的最小间隔，所有请求共用；为 0 时不限速（只用于测试）。
	MinInterval time.Duration
	// Log 记录字段缺失或类型不对的 warning，可为空。
	Log *slog.Logger
}

type Client struct {
	baseURL string
	token   string
	userID  int
	http    *http.Client
	limiter *rate.Limiter
	backoff time.Duration
	log     *slog.Logger
}

func New(o Options) *Client {
	return &Client{
		baseURL: strings.TrimRight(o.BaseURL, "/"),
		token:   o.AccessToken,
		userID:  o.UserID,
		http: &http.Client{
			Timeout: o.Timeout,
			// 跳转可能指向白名单以外的路径或别的域名，一律当作错误。
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("newapi: 上游要求跳转到 %s，不跟随", req.URL.Path)
			},
		},
		limiter: rate.NewLimiter(rate.Every(o.MinInterval), 1),
		backoff: defaultBackoff,
		log:     o.Log,
	}
}

type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Code    string          `json:"code"`
	Data    json.RawMessage `json:"data"`
}

// Response 是一次请求的原始结果。
type Response struct {
	Status int
	Body   []byte
}

// Raw 发一次 GET，不重试、不解析响应，只给自检用。withUserHeader 为 false 时不带 New-Api-User
// （自检第 2 步要确认这个头是否必需）。白名单和限速同样适用。
func (c *Client) Raw(ctx context.Context, path string, query url.Values, withUserHeader bool) (*Response, error) {
	return c.do(ctx, path, query, withUserHeader)
}

// get 请求 path 并把 data 解码到 out；429 和 5xx 退避重试，success 为 false 时返回错误。
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, path, query, true)
		if err != nil {
			return err
		}
		if resp.Status == http.StatusTooManyRequests || resp.Status >= 500 {
			if attempt == maxRetries {
				return fmt.Errorf("newapi: %s 返回 %d，重试 %d 次仍失败", path, resp.Status, maxRetries)
			}
			select {
			case <-time.After(c.backoff << attempt):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return decode(path, resp, out)
	}
}

func (c *Client) do(ctx context.Context, path string, query url.Values, withUserHeader bool) (*Response, error) {
	if !allowedPaths[path] {
		return nil, fmt.Errorf("newapi: 不允许的路径 %s", path)
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if withUserHeader {
		req.Header.Set("New-Api-User", strconv.Itoa(c.userID))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "relayusage/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("newapi: 请求 %s：%w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("newapi: 读取 %s 的响应：%w", path, err)
	}
	return &Response{Status: resp.StatusCode, Body: body}, nil
}

// decode 解析信封。token 无效时 HTTP 也是 200，只能看 success。
func decode(path string, resp *Response, out any) error {
	var env envelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return fmt.Errorf("newapi: %s 返回 %d，不是 JSON", path, resp.Status)
	}
	if !env.Success {
		if isAuthFailure(resp.Status, env) {
			return fmt.Errorf("%w：%s 返回 %d：%s", ErrAuth, path, resp.Status, strings.TrimSpace(env.Code+" "+env.Message))
		}
		return fmt.Errorf("newapi: %s 返回 %d：%s", path, resp.Status, env.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("newapi: 解析 %s 的 data：%w", path, err)
	}
	return nil
}

// isAuthFailure 判断失败是否来自鉴权：New API 鉴权失败的提示都以「无权进行此操作」开头，
// 并提到 access token 或 New-Api-User；升级到新版后旧 token 返回 401 ACCESS_TOKEN_LEGACY_RETIRED。
func isAuthFailure(status int, env envelope) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	for _, s := range []string{"access token", "New-Api-User", "无权进行此操作", "未登录"} {
		if strings.Contains(env.Message, s) {
			return true
		}
	}
	return strings.Contains(env.Code, "ACCESS_TOKEN")
}
