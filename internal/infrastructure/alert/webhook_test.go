package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FacePhys/TokenUseSys/internal/application/port"
)

type robot struct {
	mu       sync.Mutex
	messages []map[string]any
	reply    string
	status   int
}

func (r *robot) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	r.mu.Lock()
	r.messages = append(r.messages, m)
	r.mu.Unlock()
	if r.status != 0 {
		w.WriteHeader(r.status)
	}
	_, _ = w.Write([]byte(r.reply))
}

func newTestWebhook(t *testing.T, r *robot, query string) (*Webhook, *bytes.Buffer, *time.Time) {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	w, err := NewWebhook(srv.URL+"/robot/send"+query, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	return w, &logs, &now
}

func TestFormatByHost(t *testing.T) {
	require.Equal(t, formatFeishu, formatFor("open.feishu.cn"))
	require.Equal(t, formatFeishu, formatFor("open.larksuite.com"))
	require.Equal(t, formatText, formatFor("oapi.dingtalk.com"))
	require.Equal(t, formatText, formatFor("qyapi.weixin.qq.com"))
	require.Equal(t, formatText, formatFor("notfeishu.cn"), "只认 feishu.cn 及其子域名")
}

func TestTextAndFeishuPayloads(t *testing.T) {
	r := &robot{reply: `{"errcode":0,"errmsg":"ok"}`}
	w, _, _ := newTestWebhook(t, r, "?access_token=secret-robot-token")
	require.NoError(t, w.Send(context.Background(), "测试"))
	w.format = formatFeishu
	r.reply = `{"code":0,"msg":"success"}`
	require.NoError(t, w.Send(context.Background(), "测试"))

	require.Len(t, r.messages, 2)
	require.Equal(t, "text", r.messages[0]["msgtype"])
	require.Equal(t, Prefix+"测试", r.messages[0]["text"].(map[string]any)["content"], "钉钉 / 企业微信格式，带关键词前缀")
	require.Equal(t, "text", r.messages[1]["msg_type"])
	require.Equal(t, Prefix+"测试", r.messages[1]["content"].(map[string]any)["text"], "飞书格式")
}

func TestRobotErrorsAreReported(t *testing.T) {
	r := &robot{reply: `{"errcode":310000,"errmsg":"keywords not in content"}`}
	w, _, _ := newTestWebhook(t, r, "?access_token=secret-robot-token")
	require.ErrorContains(t, w.Send(context.Background(), "x"), "keywords not in content")
	r.reply = `{"code":19024,"msg":"Key Words Not Found"}`
	require.ErrorContains(t, w.Send(context.Background(), "x"), "Key Words Not Found")
	r.reply, r.status = ``, http.StatusBadGateway
	require.ErrorContains(t, w.Send(context.Background(), "x"), "HTTP 502")
}

func TestNetworkErrorDoesNotLeakToken(t *testing.T) {
	var logs bytes.Buffer
	w, err := NewWebhook("http://127.0.0.1:1/robot/send?access_token=secret-robot-token", slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	err = w.Send(context.Background(), "x")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-robot-token")
	w.Notify(context.Background(), port.Alert{Key: "auth", Text: "x"})
	require.Contains(t, logs.String(), "发送告警失败")
	require.NotContains(t, logs.String(), "secret-robot-token", "日志里只有域名")
}

func TestDedupeAndRecovery(t *testing.T) {
	r := &robot{reply: `{"errcode":0}`}
	w, _, now := newTestWebhook(t, r, "")
	ctx := context.Background()
	w.Notify(ctx, port.Alert{Key: "lag", Resolved: true, Text: "已恢复"})
	require.Empty(t, r.messages, "没告过警，不发恢复")

	w.Notify(ctx, port.Alert{Key: "lag", Text: "落后 31 分钟"})
	w.Notify(ctx, port.Alert{Key: "lag", Text: "落后 40 分钟"})
	w.Notify(ctx, port.Alert{Key: "auth", Text: "鉴权失败"})
	require.Len(t, r.messages, 2, "同一告警 6 小时内只发一次，不同告警各发各的")

	*now = now.Add(6 * time.Hour)
	w.Notify(ctx, port.Alert{Key: "lag", Text: "落后 6 小时"})
	require.Len(t, r.messages, 3, "6 小时后再提醒一次")

	w.Notify(ctx, port.Alert{Key: "lag", Resolved: true, Text: "已追上"})
	w.Notify(ctx, port.Alert{Key: "lag", Resolved: true, Text: "已追上"})
	require.Len(t, r.messages, 4, "恢复只发一次")
	w.Notify(ctx, port.Alert{Key: "lag", Text: "又落后了"})
	require.Len(t, r.messages, 5, "恢复后再出问题马上告警")
	assert.Equal(t, Prefix+"又落后了", r.messages[4]["text"].(map[string]any)["content"])
}

func TestSendSurvivesCancelledContext(t *testing.T) {
	r := &robot{reply: `{"errcode":0}`}
	w, _, _ := newTestWebhook(t, r, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, w.Send(ctx, "停机时的告警"), "停机时正在发的告警照样发完")
}
