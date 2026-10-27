package usage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKeyHint(t *testing.T) {
	require.Equal(t, "sk-AbCd…9xYz", KeyHint("sk-AbCdEfGhIjKlMnOpQrStUvWx9xYz"))
	require.Equal(t, "sk-AbCd…9xYz", KeyHint("AbCdEfGhIjKlMnOpQrStUvWx9xYz"), "没有前缀的也加上 sk-")
	require.Equal(t, "sk-AbCd…9xYz", KeyHint("AbCd**********9xYz"), "已打码的同样只留首尾")
	require.Empty(t, KeyHint("sk-short"), "太短时不提示")
	require.Empty(t, KeyHint(""))
}

func TestAttribute(t *testing.T) {
	entries := []LogEntry{
		{TokenID: 7, TokenName: "a"},
		{TokenID: 0, TokenName: "a"},
		{TokenID: 0, TokenName: "dup"},
		{TokenID: 0, TokenName: "gone"},
		{TokenID: 0, TokenName: ""},
	}
	Attribute(entries, map[string][]int64{"a": {3}, "dup": {4, 5}})
	require.Equal(t, int64(7), entries[0].TokenID, "有 token_id 时直接用，不看名字")
	require.Equal(t, int64(3), entries[1].TokenID, "按唯一的名字归属")
	require.Equal(t, int64(0), entries[2].TokenID, "同名令牌无法区分")
	require.Equal(t, int64(0), entries[3].TokenID, "匹配不上")
	require.Equal(t, int64(0), entries[4].TokenID)
}

func TestCurrencyAmount(t *testing.T) {
	cny := Currency{QuotaPerUnit: 500000, DisplayType: "CNY", ExchangeRate: 1}
	require.InDelta(t, 2.0, cny.Amount(1000000), 1e-9)
	require.InDelta(t, 14.4, Currency{QuotaPerUnit: 500000, DisplayType: "CNY", ExchangeRate: 7.2}.Amount(1000000), 1e-9)
	require.InDelta(t, 2.0, Currency{QuotaPerUnit: 500000, DisplayType: "USD", ExchangeRate: 7.2}.Amount(1000000), 1e-9)
	require.InDelta(t, 1000000, Currency{QuotaPerUnit: 500000, DisplayType: "TOKENS"}.Amount(1000000), 1e-9)
	require.InDelta(t, 42, Currency{}.Amount(42), 1e-9, "参数无效时不除以 0")
}

func TestHours(t *testing.T) {
	require.Equal(t, int64(7200), HourStart(7200))
	require.Equal(t, int64(7200), HourStart(10799))
	first, last := AffectedHours(7300, 10900)
	require.Equal(t, [2]int64{7200, 14400}, [2]int64{first, last})
	first, last = AffectedHours(7200, 10800)
	require.Equal(t, [2]int64{7200, 10800}, [2]int64{first, last}, "结束位置不含")

	first, last = WholeHours(7300, 18000)
	require.Equal(t, [2]int64{10800, 18000}, [2]int64{first, last})
	first, last = WholeHours(7300, 10000)
	require.GreaterOrEqual(t, first, last, "不足一个完整小时")
}

func TestSplit(t *testing.T) {
	day := int64(1792512000) // 整点
	require.Equal(t, day+12*3600, Split(day, day+86400))
	require.Equal(t, day+3600, Split(day, day+3*3600-1), "超过 2 小时时对齐整点")
	require.Equal(t, day+1800, Split(day, day+3600), "2 小时以内直接取中点")
	require.Equal(t, day, Split(day, day+1), "只剩 1 秒时不再拆")
}

func TestNextWindowEnd(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	require.Equal(t, int64(1_000_000-120), NextWindowEnd(999_000, now, 2*time.Minute))
	require.Equal(t, int64(500_000+86400), NextWindowEnd(500_000, now, 2*time.Minute), "回填时一天一个窗口")
}

func TestDayStart(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	got := DayStart(time.Date(2026, 10, 27, 1, 30, 0, 0, time.UTC), loc)
	require.Equal(t, time.Date(2026, 10, 27, 0, 0, 0, 0, loc), got)
	require.Equal(t, int64(1792512000+6*86400), got.Unix())
}
