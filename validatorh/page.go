package validatorh

import (
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/pkg/pagination"
)

// parsePageParam 解析分页 query 参数：strconv.Atoi 的错误不丢——越界输入
// （如 page=99999999999999999999）返回**钳位值 + ErrRange**（不是 0），错误
// 被丢弃会让 MaxInt64 一路传给消费方（拼游标/塞 SQL），而 page=abc 走默认
// 值——同一个「非法输入」两种下场（#236）。err != nil 时一律按非法值处理
// （返回 0，由 Normalize / 默认值兜底），与 page=abc 的口径统一。
func parsePageParam(c *app.RequestContext, key string) int {
	v, err := strconv.Atoi(c.Query(key))
	if err != nil {
		return 0
	}
	return v
}

// BindPage 从 Hertz RequestContext 绑定分页参数。
// 支持 query 参数: page, page_size；非法值（含越界）回落默认值。
func BindPage(c *app.RequestContext) *pagination.PageRequest {
	req := pagination.NewPageRequest(parsePageParam(c, "page"), parsePageParam(c, "page_size"))
	req.Normalize()
	return req
}

// BindPageWithDefault 从 Hertz RequestContext 绑定分页参数，使用自定义默认值。
func BindPageWithDefault(c *app.RequestContext, defaultPage, defaultPageSize int) *pagination.PageRequest {
	page := parsePageParam(c, "page")
	pageSize := parsePageParam(c, "page_size")

	if page <= 0 {
		page = defaultPage
	}
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}

	req := pagination.NewPageRequest(page, pageSize)
	req.Normalize()
	return req
}
