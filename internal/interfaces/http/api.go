package httpapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/FacePhys/TokenUseSys/internal/application/dashboard"
	"github.com/FacePhys/TokenUseSys/internal/application/port"
)

// healthz 免登录，Nginx 不对外暴露（§12.4）：回填中或落后不超过 10 分钟返回 200，否则 503。
func (h *handler) healthz(c *gin.Context) {
	health, err := h.dash.Health(c.Request.Context())
	if err != nil {
		h.fail(c, err)
		return
	}
	status := http.StatusOK
	if !health.OK {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, health)
}

func (h *handler) summary(c *gin.Context) {
	out, err := h.dash.Summary(c.Request.Context())
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) tokens(c *gin.Context) {
	r, err := h.dash.ParseRange(c.Query("start"), c.Query("end"))
	if err != nil {
		h.fail(c, err)
		return
	}
	out, err := h.dash.Tokens(c.Request.Context(), r)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) token(c *gin.Context) {
	id, err := pathID(c)
	if err != nil {
		h.fail(c, err)
		return
	}
	out, err := h.dash.Token(c.Request.Context(), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// logs 返回令牌的最近请求；带 start / end 时只看这个区间（详情页随所选区间变化，§7）。
func (h *handler) logs(c *gin.Context) {
	id, err := pathID(c)
	if err != nil {
		h.fail(c, err)
		return
	}
	var r *dashboard.Range
	if c.Query("start") != "" || c.Query("end") != "" {
		parsed, err := h.dash.ParseRange(c.Query("start"), c.Query("end"))
		if err != nil {
			h.fail(c, err)
			return
		}
		r = &parsed
	}
	page, err := intQuery(c, "page")
	if err != nil {
		h.fail(c, err)
		return
	}
	size, err := intQuery(c, "page_size")
	if err != nil {
		h.fail(c, err)
		return
	}
	out, err := h.dash.Logs(c.Request.Context(), id, r, c.Query("model"), page, size)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) series(c *gin.Context) {
	r, bucket, tokenID, err := h.usageParams(c)
	if err != nil {
		h.fail(c, err)
		return
	}
	top, err := topQuery(c)
	if err != nil {
		h.fail(c, err)
		return
	}
	out, err := h.dash.Series(c.Request.Context(), r, bucket, top, tokenID)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) models(c *gin.Context) {
	r, err := h.dash.ParseRange(c.Query("start"), c.Query("end"))
	if err != nil {
		h.fail(c, err)
		return
	}
	tokenID, err := tokenIDQuery(c)
	if err != nil {
		h.fail(c, err)
		return
	}
	out, err := h.dash.Models(c.Request.Context(), r, tokenID)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// utf8BOM 让 Excel 按 UTF-8 打开，中文令牌名不乱码（§6）。
const utf8BOM = "\uFEFF"

// export 按 日期（或小时）× 令牌 × 模型 导出 CSV（§6）。
func (h *handler) export(c *gin.Context) {
	r, bucket, tokenID, err := h.usageParams(c)
	if err != nil {
		h.fail(c, err)
		return
	}
	table, err := h.dash.Export(c.Request.Context(), r, bucket, tokenID)
	if err != nil {
		h.fail(c, err)
		return
	}
	name := fmt.Sprintf("relayusage-%s_%s", r.Start, r.End)
	timeHeader := "日期"
	if bucket == port.BucketHour {
		name += "-hourly"
		timeHeader = "时间"
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	c.Status(http.StatusOK)
	_, _ = c.Writer.WriteString(utf8BOM)
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{timeHeader, "令牌 ID", "令牌", "模型", "请求数", "输入 token", "输出 token", "缓存读 token",
		"缓存写 token", "金额（" + table.Currency + "）"})
	for _, row := range table.Rows {
		_ = w.Write([]string{row.Bucket, strconv.FormatInt(row.TokenID, 10), csvText(row.TokenName), csvText(row.Model),
			strconv.FormatInt(row.Requests, 10), strconv.FormatInt(row.PromptTokens, 10),
			strconv.FormatInt(row.CompletionTokens, 10), strconv.FormatInt(row.CacheReadTokens, 10),
			strconv.FormatInt(row.CacheWriteTokens, 10), strconv.FormatFloat(row.Amount, 'f', 4, 64)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		h.log.WarnContext(c.Request.Context(), "写出 CSV 失败", "error", err)
	}
}

// csvText 处理令牌名、模型名这类自由文本：以 = + - @ 开头的会被 Excel 当成公式执行，前面加一个单引号。
func csvText(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (h *handler) usageParams(c *gin.Context) (dashboard.Range, port.Bucket, *int64, error) {
	r, err := h.dash.ParseRange(c.Query("start"), c.Query("end"))
	if err != nil {
		return dashboard.Range{}, 0, nil, err
	}
	bucket, err := dashboard.ParseBucket(c.Query("bucket"))
	if err != nil {
		return dashboard.Range{}, 0, nil, err
	}
	tokenID, err := tokenIDQuery(c)
	return r, bucket, tokenID, err
}

func pathID(c *gin.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("%w：令牌 id 必须是非负整数", dashboard.ErrInvalidArgument)
	}
	return id, nil
}

// tokenIDQuery 解析可选的 token_id，0 是未归属。
func tokenIDQuery(c *gin.Context) (*int64, error) {
	v := c.Query("token_id")
	if v == "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 0 {
		return nil, fmt.Errorf("%w：token_id 必须是非负整数", dashboard.ErrInvalidArgument)
	}
	return &id, nil
}

// topQuery 解析 top：all 表示每个令牌单独一条、不合并「其他」；省略时返回 0，由查询层取默认值。
func topQuery(c *gin.Context) (int, error) {
	if c.Query("top") == "all" {
		return dashboard.AllTop, nil
	}
	n, err := intQuery(c, "top")
	if err == nil && n < 0 {
		err = fmt.Errorf("%w：top 必须是正整数或 all", dashboard.ErrInvalidArgument)
	}
	return n, err
}

// intQuery 解析可选的整数参数，没有时返回 0，由查询层取默认值。
func intQuery(c *gin.Context, name string) (int, error) {
	v := c.Query(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%w：%s 必须是整数", dashboard.ErrInvalidArgument, name)
	}
	return n, nil
}
