package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// 登录失败锁定（§8）：同一 IP 15 分钟内失败 5 次，锁 15 分钟。
const (
	loginWindow      = 15 * time.Minute
	maxLoginFailures = 5
	lockDuration     = 15 * time.Minute
	// maxTrackedIPs 超过时清理过期记录，防止被大量 IP 撑大内存。
	maxTrackedIPs  = 10000
	maxLoginBody   = 4 << 10
	sessionCookie  = "relayusage_session"
	sessionContext = "relayusage-session\x00"
)

// AuthOptions 是登录配置：单个管理员，密码只存 bcrypt 哈希。
type AuthOptions struct {
	User         string
	PasswordHash string
	// Secret 是会话签名密钥，至少 32 字节（启动时已检查）。
	Secret string
	TTL    time.Duration
}

func (o AuthOptions) validate() error {
	if o.User == "" || o.PasswordHash == "" || o.Secret == "" || o.TTL <= 0 {
		return errors.New("登录配置不完整")
	}
	return nil
}

// sessions 签发和校验会话 Cookie：base64url(用户名|过期时间).base64url(HMAC-SHA256)。服务端不存会话。
// 签名密钥由 session_secret 和当前密码哈希一起派生：改密码后旧会话全部失效。
type sessions struct {
	user string
	key  []byte
	ttl  time.Duration
}

func newSessions(o AuthOptions) *sessions {
	mac := hmac.New(sha256.New, []byte(o.Secret))
	mac.Write([]byte(sessionContext + o.PasswordHash))
	return &sessions{user: o.User, key: mac.Sum(nil), ttl: o.TTL}
}

func (s *sessions) sign(payload string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func (s *sessions) issue(now time.Time) (string, time.Time) {
	exp := now.Add(s.ttl)
	payload := base64.RawURLEncoding.EncodeToString([]byte(s.user + "|" + strconv.FormatInt(exp.Unix(), 10)))
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.sign(payload)), exp
}

func (s *sessions) valid(value string, now time.Time) bool {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(payload)) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(raw), '|')
	if i < 0 {
		return false
	}
	exp, err := strconv.ParseInt(string(raw[i+1:]), 10, 64)
	return err == nil && string(raw[:i]) == s.user && now.Unix() < exp
}

// loginLimiter 按客户端 IP 记录登录失败，进程内存里，重启后清空。
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	locked   map[string]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string][]time.Time{}, locked: map[string]time.Time{}}
}

func (l *loginLimiter) lockedUntil(ip string, now time.Time) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.locked[ip]
	if ok && now.Before(until) {
		return until, true
	}
	delete(l.locked, ip)
	return time.Time{}, false
}

// fail 记一次失败，返回是否因此被锁定。
func (l *loginLimiter) fail(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) > maxTrackedIPs {
		for k, ts := range l.failures {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > loginWindow {
				delete(l.failures, k)
			}
		}
	}
	recent := l.failures[ip][:0:0]
	for _, t := range l.failures[ip] {
		if now.Sub(t) < loginWindow {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	if len(recent) >= maxLoginFailures {
		delete(l.failures, ip)
		l.locked[ip] = now.Add(lockDuration)
		return true
	}
	l.failures[ip] = recent
	return false
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// login 校验用户名和密码，成功后设置会话 Cookie。只接受 JSON：跨站表单提交不了 JSON，加上 SameSite=Strict 防 CSRF。
// 用户名不对时也照样比对一次 bcrypt，响应时间不泄露用户名是否正确。
func (h *handler) login(c *gin.Context) {
	if c.ContentType() != "application/json" {
		abort(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "请求体必须是 JSON")
		return
	}
	now := h.now()
	ip := c.ClientIP()
	if until, locked := h.limiter.lockedUntil(ip, now); locked {
		wait := until.Sub(now)
		c.Header("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		abort(c, http.StatusTooManyRequests, "too_many_attempts",
			fmt.Sprintf("登录失败次数过多，请 %d 分钟后再试", int(math.Ceil(wait.Minutes()))))
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxLoginBody)).Decode(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid_request", "请求格式错误")
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(h.auth.User)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(h.auth.PasswordHash), []byte(req.Password)) == nil
	if !userOK || !passOK {
		locked := h.limiter.fail(ip, now)
		h.log.WarnContext(c.Request.Context(), "登录失败", "ip", ip, "locked", locked)
		abort(c, http.StatusUnauthorized, "invalid_credentials", "用户名或密码错误")
		return
	}
	h.limiter.reset(ip)
	value, exp := h.sessions.issue(now)
	h.setCookie(c, value, int(h.auth.TTL.Seconds()))
	h.log.InfoContext(c.Request.Context(), "登录成功", "ip", ip)
	c.JSON(http.StatusOK, gin.H{"user": h.auth.User, "expires_at": exp.Unix()})
}

// logout 清掉会话 Cookie。会话不存在服务端，Cookie 被复制走的话要等到过期；需要立即作废时改 session_secret 重启。
func (h *handler) logout(c *gin.Context) {
	h.setCookie(c, "", -1)
	c.Status(http.StatusNoContent)
}

func (h *handler) requireSession(c *gin.Context) {
	ck, err := c.Request.Cookie(sessionCookie)
	if err != nil || !h.sessions.valid(ck.Value, h.now()) {
		abort(c, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	c.Next()
}

// setCookie 写会话 Cookie（§8）：HttpOnly、SameSite=Strict；经域名访问时加 Secure。本机调试用
// http://127.0.0.1 或 localhost 时不加 Secure，否则部分浏览器不保存 Cookie。maxAge 为负时删除。
func (h *handler) setCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !isLocalHost(c.Request.Host),
		SameSite: http.SameSiteStrictMode,
	})
}

func isLocalHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
