package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile("../../../config.example.yaml")
	require.NoError(t, err)
	cfg, err := Parse(data)
	require.NoError(t, err)
	require.Equal(t, Default().Sync, cfg.Sync, "示例文件和默认值一致")
	require.Equal(t, int64(28800), cfg.TZOffset())
	require.Error(t, cfg.ValidateUpstream(), "示例文件没有填凭据")
	require.Error(t, cfg.ValidateServer())
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Parse([]byte("upstream:\n  base_url: https://4router.net\n  tokn: x\n"))
	require.ErrorContains(t, err, "tokn")
}

func TestInvalidValuesRejected(t *testing.T) {
	cases := map[string]string{
		"带路径的地址":   "upstream:\n  base_url: https://4router.net/api\n",
		"请求间隔太小":   "upstream:\n  min_request_interval: 100ms\n",
		"对账时间格式错误": "sync:\n  reconcile_at: \"3:30pm\"\n",
		"有夏令时的时区":  "server:\n  timezone: America/New_York\n",
		"偏移不是整小时":  "server:\n  timezone: Asia/Kolkata\n",
		"不存在的时区":   "server:\n  timezone: Mars/Base\n",
		"日志级别错误":   "log:\n  level: verbose\n",
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(yml))
			require.Error(t, err)
		})
	}
}

func TestBaseURLTrailingSlashTrimmed(t *testing.T) {
	cfg, err := Parse([]byte("upstream:\n  base_url: https://hk.4router.net/\n"))
	require.NoError(t, err)
	require.Equal(t, "https://hk.4router.net", cfg.Upstream.BaseURL)
}

func TestValidateServer(t *testing.T) {
	cfg, err := Parse([]byte(`
upstream:
  access_token: tok
  user_id: 42
auth:
  admin_password_hash: "$2a$12$abcdefghijklmnopqrstuuJ5c6lOa4yQnJH0yv0m8iGQ3E8mT0bGy"
  session_secret: "` + strings.Repeat("a", 31) + `"
`))
	require.NoError(t, err)
	require.NoError(t, cfg.ValidateUpstream())
	require.ErrorContains(t, cfg.ValidateServer(), "session_secret")

	cfg.Auth.SessionSecret = strings.Repeat("a", 32)
	require.NoError(t, cfg.ValidateServer())

	cfg.Auth.AdminPasswordHash = "plaintext"
	require.ErrorContains(t, cfg.ValidateServer(), "bcrypt")
}
