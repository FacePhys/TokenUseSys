package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func run(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func writeConfig(t *testing.T, yml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yml), 0o600))
	return path
}

func TestHashPasswordFromPipe(t *testing.T) {
	stdout, _, err := run(t, "correct horse battery\n", "hash-password")
	require.NoError(t, err)
	hash := strings.TrimSpace(stdout)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse battery")))
	cost, err := bcrypt.Cost([]byte(hash))
	require.NoError(t, err)
	require.Equal(t, bcryptCost, cost)

	_, _, err = run(t, "short\n", "hash-password")
	require.ErrorContains(t, err, "至少 8 个字符")
}

func TestUnknownSubcommand(t *testing.T) {
	_, stderr, err := run(t, "", "frobnicate")
	require.ErrorContains(t, err, "未知的子命令")
	require.Contains(t, stderr, "selfcheck")
}

func TestSelfCheckRequiresCredentials(t *testing.T) {
	path := writeConfig(t, "upstream:\n  base_url: https://4router.net\n")
	_, _, err := run(t, "", "-config", path, "selfcheck")
	require.ErrorContains(t, err, "upstream.access_token")
}

func TestSelfCheckReportsAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"CNY","usd_exchange_rate":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，access token 无效"}`))
	}))
	defer srv.Close()
	path := writeConfig(t, "upstream:\n  base_url: "+srv.URL+"\n  access_token: secret-access-token-0123456789\n  user_id: 42\n  min_request_interval: 1s\n")
	dir := filepath.Join(t.TempDir(), "fixtures")

	stdout, stderr, err := run(t, "", "-config", path, "selfcheck", "-fixtures", dir)
	require.ErrorContains(t, err, "失败项")
	require.Contains(t, stderr, "[3/10]", "进度写到标准错误")
	require.Contains(t, stdout, " 1. [通过] /api/status 换算参数")
	require.Contains(t, stdout, " 3. [失败] 带齐两个头调 /api/user/self")
	require.Contains(t, stdout, "10. [跳过]")
	require.Contains(t, stdout, "11. [通过] 样本")
	require.Contains(t, stdout, "结论：通过 1、注意 0、失败 1、信息 1、跳过 7。")

	data, err := os.ReadFile(filepath.Join(dir, "status.json"))
	require.NoError(t, err)
	require.Contains(t, string(data), `"quota_per_unit": 500000`)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		require.NotContains(t, string(b), "secret-access-token")
	}
}

func TestAlertTest(t *testing.T) {
	path := writeConfig(t, "upstream:\n  base_url: https://4router.net\n")
	_, _, err := run(t, "", "-config", path, "alert-test")
	require.ErrorContains(t, err, "alert.webhook 没有配置")

	var got string
	robot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer robot.Close()
	path = writeConfig(t, "alert:\n  webhook: "+robot.URL+"/robot/send?access_token=x\n")
	stdout, _, err := run(t, "", "-config", path, "alert-test")
	require.NoError(t, err)
	require.Contains(t, stdout, "已发送")
	require.Contains(t, got, "【RelayUsage】测试消息")
}
