package newapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(Options{BaseURL: srv.URL + "/", AccessToken: "sk-secret-access-9f3a", UserID: 42, Timeout: 5 * time.Second})
	c.backoff = time.Millisecond
	return c
}

func TestGetSendsHeadersAndDecodesData(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/user/self", r.URL.Path)
		assert.Equal(t, "Bearer sk-secret-access-9f3a", r.Header.Get("Authorization"))
		assert.Equal(t, "42", r.Header.Get("New-Api-User"))
		assert.Equal(t, "1", r.URL.Query().Get("p"))
		_, _ = w.Write([]byte(`{"success":true,"message":"","data":{"quota":123}}`))
	})
	var out struct{ Quota int64 }
	require.NoError(t, c.get(context.Background(), "/api/user/self", url.Values{"p": {"1"}}, &out))
	require.Equal(t, int64(123), out.Quota)
}

func TestPathOutsideWhitelistRejected(t *testing.T) {
	var called atomic.Bool
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { called.Store(true) })
	for _, path := range []string{"/api/token/1", "/api/user/token", "/api/data/self"} {
		_, err := c.Raw(context.Background(), path, nil, true)
		require.ErrorContains(t, err, "不允许的路径")
	}
	require.False(t, called.Load(), "白名单以外的路径不发请求")
}

func TestAuthFailureWithHTTP200(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，access token 无效"}`))
	})
	err := c.get(context.Background(), "/api/user/self", nil, nil)
	require.ErrorIs(t, err, ErrAuth)
	require.NotContains(t, err.Error(), "secret", "错误信息里没有 token")
}

func TestLegacyTokenRetired(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"code":"ACCESS_TOKEN_LEGACY_RETIRED","message":"legacy access token retired"}`))
	})
	require.ErrorIs(t, c.get(context.Background(), "/api/user/self", nil, nil), ErrAuth)
}

func TestBusinessFailureIsNotAuth(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"message":"参数错误"}`))
	})
	err := c.get(context.Background(), "/api/log/self", nil, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAuth)
}

func TestRetriesOn429And5xx(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"success":true,"data":null}`))
		}
	})
	require.NoError(t, c.get(context.Background(), "/api/status", nil, nil))
	require.Equal(t, int32(3), calls.Load())
}

func TestGivesUpAfterThreeRetries(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	err := c.get(context.Background(), "/api/status", nil, nil)
	require.ErrorContains(t, err, "返回 429，重试 3 次仍失败")
	require.Equal(t, int32(4), calls.Load())
}

func TestRedirectNotFollowed(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/token/" {
			http.Redirect(w, r, "/api/token/1", http.StatusMovedPermanently)
			return
		}
		t.Errorf("跟随了跳转：%s", r.URL.Path)
	})
	err := c.get(context.Background(), "/api/token/", nil, nil)
	require.ErrorContains(t, err, "不跟随")
}

func TestRawWithoutUserHeader(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Values("New-Api-User"))
		_, _ = w.Write([]byte(`{"success":false,"message":"无权进行此操作，未提供 New-Api-User"}`))
	})
	resp, err := c.Raw(context.Background(), "/api/user/self", nil, false)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.Status)
	require.Contains(t, string(resp.Body), "New-Api-User")
}

func TestCancelledContextStopsBackoff(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	c.backoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, c.get(ctx, "/api/status", nil, nil), context.DeadlineExceeded)
}
