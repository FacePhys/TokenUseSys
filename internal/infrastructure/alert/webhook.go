// Package alert 把同步异常发到群机器人（开发文档 §4.7）：飞书、钉钉、企业微信，按 webhook 的域名选消息格式。
// 同一告警 6 小时内只发一次；恢复时补发一条。webhook 地址里有机器人的 token，日志里只记域名。
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
)

const (
	// dedupeWindow 内同一告警只发一次（§4.7）。
	dedupeWindow = 6 * time.Hour
	sendTimeout  = 5 * time.Second
	// Prefix 加在每条消息前面。机器人用「自定义关键词」做安全设置时，关键词填 RelayUsage。
	Prefix = "【RelayUsage】"
)

// Noop 是没配置 webhook 时的告警：什么都不做。
type Noop struct{}

func (Noop) Notify(context.Context, port.Alert) {}

// format 是机器人的消息格式。
type format int

const (
	// formatText：钉钉和企业微信群机器人，{"msgtype":"text","text":{"content":...}}
	formatText format = iota
	// formatFeishu：飞书（Lark）自定义机器人，{"msg_type":"text","content":{"text":...}}
	formatFeishu
)

func formatFor(host string) format {
	host = strings.ToLower(host)
	for _, suffix := range []string{"feishu.cn", "larksuite.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return formatFeishu
		}
	}
	return formatText
}

// Webhook 发告警到群机器人。
type Webhook struct {
	url    string
	host   string
	format format
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	sent   map[string]time.Time // 告警 key → 最后一次发送时间
	active map[string]bool      // 告警 key → 发过告警、还没恢复
}

var _ port.Alerter = (*Webhook)(nil)

func NewWebhook(rawURL string, log *slog.Logger) (*Webhook, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("alert.webhook 必须是 http(s) 地址")
	}
	return &Webhook{
		url: rawURL, host: u.Host, format: formatFor(u.Hostname()),
		client: &http.Client{Timeout: sendTimeout},
		log:    log, now: time.Now,
		sent: map[string]time.Time{}, active: map[string]bool{},
	}, nil
}

// Notify 按去重规则发送：同一告警 6 小时内只发一次；恢复通知只在告过警之后发一次。
func (w *Webhook) Notify(ctx context.Context, a port.Alert) {
	w.mu.Lock()
	now := w.now()
	switch {
	case a.Resolved && !w.active[a.Key]:
		w.mu.Unlock()
		return
	case a.Resolved:
		delete(w.active, a.Key)
		delete(w.sent, a.Key)
	default:
		if last, ok := w.sent[a.Key]; ok && now.Sub(last) < dedupeWindow {
			w.mu.Unlock()
			return
		}
		w.sent[a.Key] = now
		w.active[a.Key] = true
	}
	w.mu.Unlock()
	if err := w.Send(ctx, a.Text); err != nil {
		w.log.WarnContext(ctx, "发送告警失败", "host", w.host, "key", a.Key, "error", err)
		return
	}
	w.log.InfoContext(ctx, "已发送告警", "host", w.host, "key", a.Key, "resolved", a.Resolved)
}

// Send 直接发一条消息，不去重；relayctl alert-test 用它检查配置。停机时也能发完（不跟随 ctx 取消）。
func (w *Webhook) Send(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	text = Prefix + text
	var payload any = map[string]any{"msgtype": "text", "text": map[string]string{"content": text}}
	if w.format == formatFeishu {
		payload = map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("创建请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		// *url.Error 的文字里有完整地址（含机器人 token），只保留底层原因。
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("请求 %s：%w", w.host, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s 返回 HTTP %d", w.host, resp.StatusCode)
	}
	// 钉钉、企业微信返回 {"errcode":0,"errmsg":"ok"}，飞书返回 {"code":0,"msg":"success"}；非 0 是失败（关键词不对、签名不对等）。
	var r struct {
		ErrCode *int   `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Code    *int   `json:"code"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(raw, &r) == nil {
		if r.ErrCode != nil && *r.ErrCode != 0 {
			return fmt.Errorf("%s 返回错误 %d：%s", w.host, *r.ErrCode, r.ErrMsg)
		}
		if r.Code != nil && *r.Code != 0 {
			return fmt.Errorf("%s 返回错误 %d：%s", w.host, *r.Code, r.Msg)
		}
	}
	return nil
}
