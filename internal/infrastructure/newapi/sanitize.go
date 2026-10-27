package newapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// sanitizer 给自检样本脱敏（开发文档 §10 第 11 步）：去掉完整 key 和账号信息，令牌名、用户名、IP、
// request_id 换成假值。同一个原值在所有样本里换成同一个假值，令牌列表和日志之间的归属关系不变。
type sanitizer struct {
	tokenNames map[string]string
	users      map[string]string
	keys       map[string]string
	requests   map[string]string
}

func newSanitizer() *sanitizer {
	return &sanitizer{
		tokenNames: map[string]string{},
		users:      map[string]string{},
		keys:       map[string]string{},
		requests:   map[string]string{},
	}
}

// userFields 是用户信息样本保留的字段，其余（邮箱、第三方账号、通知设置等）一律不存。
var userFields = []string{"id", "username", "display_name", "role", "status", "group", "quota", "used_quota", "request_count"}

// blankedFields 的值换成空字符串。
var blankedFields = map[string]bool{
	"access_token": true, "password": true, "email": true, "github_id": true, "oidc_id": true, "wechat_id": true,
	"telegram_id": true, "linux_do_id": true, "discord_id": true, "aff_code": true, "stripe_customer": true,
	"setting": true, "allow_ips": true, "remark": true, "channel_name": true,
}

// envelope 解析响应体，用 clean 处理 data 后格式化输出。解析失败时不保存原文。
func (s *sanitizer) envelope(body []byte, clean func(any) any) []byte {
	v, err := decodeJSON(body)
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		return []byte("{\"_note\": \"响应不是 JSON 对象，未保存\"}\n")
	}
	if data, ok := m["data"]; ok {
		m["data"] = clean(data)
	}
	return marshalIndent(m)
}

func (s *sanitizer) user(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return s.generic(v)
	}
	kept := map[string]any{}
	for _, k := range userFields {
		if x, ok := m[k]; ok {
			kept[k] = x
		}
	}
	return s.generic(kept)
}

func (s *sanitizer) tokenPage(v any) any { return s.generic(v) }

func (s *sanitizer) logPage(v any) any { return s.generic(v) }

func (s *sanitizer) generic(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = s.field(k, val)
		}
	case []any:
		for i := range x {
			x[i] = s.generic(x[i])
		}
	}
	return v
}

func (s *sanitizer) field(key string, v any) any {
	str, isStr := v.(string)
	switch {
	case !isStr:
		if key == "user_id" {
			if _, ok := v.(json.Number); ok {
				return json.Number("1")
			}
		}
		return s.generic(v)
	case blankedFields[key]:
		return ""
	}
	switch key {
	case "name", "token_name":
		return mapValue(s.tokenNames, "token", str)
	case "username", "display_name":
		return mapValue(s.users, "user", str)
	case "request_id":
		return mapValue(s.requests, "req", str)
	case "key":
		return s.fakeKey(str)
	case "ip":
		if str != "" {
			return "203.0.113.1"
		}
	case "other":
		return s.other(str)
	}
	return str
}

// fakeKey 生成和原值格式相同（是否带 sk- 前缀、是否打码）的假 key。
func (s *sanitizer) fakeKey(orig string) string {
	if orig == "" {
		return ""
	}
	if fake, ok := s.keys[orig]; ok {
		return fake
	}
	n := len(s.keys) + 1
	prefix := ""
	if strings.HasPrefix(orig, "sk-") {
		prefix = "sk-"
	}
	fake := prefix + fmt.Sprintf("fake%044d", n)
	if strings.Contains(orig, "*") {
		fake = prefix + fmt.Sprintf("fake%04d**********%04d", n, n)
	}
	s.keys[orig] = fake
	return fake
}

// other 是 JSON 字符串，解析后同样脱敏再写回；解析失败时清空。
func (s *sanitizer) other(str string) string {
	if str == "" {
		return ""
	}
	v, err := decodeJSON([]byte(str))
	if err != nil {
		return ""
	}
	b, err := json.Marshal(s.generic(v))
	if err != nil {
		return ""
	}
	return string(b)
}

// originalKeys 返回脱敏前的 key，用于最后检查样本里没有残留。
func (s *sanitizer) originalKeys() []string {
	keys := make([]string, 0, len(s.keys))
	for k := range s.keys {
		keys = append(keys, strings.TrimPrefix(k, "sk-"))
	}
	return keys
}

func mapValue(m map[string]string, prefix, orig string) string {
	if orig == "" {
		return ""
	}
	if fake, ok := m[orig]; ok {
		return fake
	}
	fake := fmt.Sprintf("%s-%d", prefix, len(m)+1)
	m[orig] = fake
	return fake
}

func mustIndent(body []byte) []byte {
	v, err := decodeJSON(body)
	if err != nil {
		return []byte("{\"_note\": \"响应不是 JSON，未保存\"}\n")
	}
	return marshalIndent(v)
}

func marshalIndent(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return buf.Bytes()
}
