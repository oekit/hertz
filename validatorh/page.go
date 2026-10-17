package validatorh

import (
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/pkg/pagination"
)

// BindPage 从 Hertz RequestContext 绑定分页参数。
// 支持 query 参数: page, page_size
func BindPage(c *app.RequestContext) *pagination.PageRequest {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

	req := pagination.NewPageRequest(page, pageSize)
	req.Normalize()
	return req
}

// BindPageWithDefault 从 Hertz RequestContext 绑定分页参数，使用自定义默认值。
func BindPageWithDefault(c *app.RequestContext, defaultPage, defaultPageSize int) *pagination.PageRequest {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))

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
