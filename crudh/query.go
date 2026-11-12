package crudh

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/gormx/crud"
	"github.com/oekit/pkg/errors"
	"github.com/oekit/pkg/pagination"
)

// sortColumnRe 严格匹配 SQL 标识符（字母/下划线开头，仅字母数字下划线）。
// 排序列名必须通过此校验，杜绝 ORDER BY 注入。
var sortColumnRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// bindQuery 绑定 List 查询参数：自定义 [QueryBinder] 优先（钩子全权接管绑定，
// 缺省绑定不执行）；否则按 listParamsDisabled 决定是否走缺省绑定。
func (h *Handler[T, ID, C, U]) bindQuery(ctx context.Context, c *app.RequestContext, q *crud.Query) error {
	if h.queryBinder != nil {
		return h.queryBinder(ctx, c, q)
	}
	if h.listParamsDisabled {
		return nil
	}
	return h.BindDefaultQuery(c, q)
}

// BindDefaultQuery 把缺省查询参数（page/page_size/sort）绑定到 q。
//
// 供自定义 [QueryBinder] 复用缺省绑定口径（分页 + 排序白名单校验）：钩子内先调
// 本方法再叠加自定义条件，即可「保留缺省分页排序 + 增加自定义过滤」。
// 非法值返回 400 语义的 badRequest 错误（与缺省绑定逐字一致）。
func (h *Handler[T, ID, C, U]) BindDefaultQuery(c *app.RequestContext, q *crud.Query) error {
	// 绑定分页参数（无效值返回 400）
	page, pageErr := strconv.Atoi(c.Query("page"))
	pageSize, sizeErr := strconv.Atoi(c.Query("page_size"))
	if pageErr != nil && c.Query("page") != "" {
		return badRequest("invalid page parameter")
	}
	if sizeErr != nil && c.Query("page_size") != "" {
		return badRequest("invalid page_size parameter")
	}
	h.bindPage(q, page, pageSize)

	// 绑定排序参数：逗号分隔多列，每列 "<column>[ ASC|DESC]"。
	// 列名经严格标识符正则 + 白名单校验后重建，用户原文不进入 SQL，
	// 防止 ORDER BY 注入（如 sort=(SELECT CASE WHEN ...)）。
	if sort := c.Query("sort"); sort != "" {
		for _, seg := range strings.Split(sort, ",") {
			order, err := parseSortParam(seg, h.sortAllowlist())
			if err != nil {
				return badRequest("invalid sort parameter")
			}
			q.Order(order)
		}
	}
	return nil
}

// bindPage 绑定分页参数到 q。
//
// v0.9.0 缺省（pageNormalization=true）：分页参数先归一（页码缺省 1、
// 每页条数缺省 20、上限 500，与 pkg/pagination 口径一致），再用**归一后的值**
// 调 q.Page ⇒ 响应回显的 page/page_size 与 SQL 实际 LIMIT 恒等，且缺省
// 请求也带 LIMIT（不再全表扫描 + 全量序列化）。
//
// v0.8.14 逐字口径（pageNormalization=false，WithPageNormalization(false)）：
// 仅当 page>0 或 page_size>0 时才调 q.Page，且**不归一**（回显原值）。
func (h *Handler[T, ID, C, U]) bindPage(q *crud.Query, page, pageSize int) {
	if !h.pageNormalization {
		if page > 0 || pageSize > 0 {
			q.Page(page, pageSize)
		}
		return
	}
	q.Page(h.normalizePage(page), h.normalizePageSize(pageSize))
}

// normalizePage 归一页码：非正 → 1（与 pagination.DefaultPage 一致）。
func (h *Handler[T, ID, C, U]) normalizePage(page int) int {
	if page <= 0 {
		return pagination.DefaultPage
	}
	return page
}

// normalizePageSize 归一每页条数：非正 → 缺省值（WithDefaultPageSize，
// 缺省 pagination.DefaultPageSize=20），再按有效上限截断
// （WithPageSizeLimit，缺省 pagination.MaxPageSize=500）。
//
// 归一后的值恒落在 [1, 500]：下游 pkg/pagination 的 PageRequest.Limit()
// 对该区间是恒等映射（只把 <=0 提到 20、把 >500 截到 500）⇒ 响应回显
// 与 SQL 实际 LIMIT 恒等。
func (h *Handler[T, ID, C, U]) normalizePageSize(pageSize int) int {
	limit := h.pageSizeLimit
	if limit <= 0 || limit > pagination.MaxPageSize {
		limit = pagination.MaxPageSize
	}
	if pageSize <= 0 {
		pageSize = h.defaultPageSize
		if pageSize <= 0 {
			pageSize = pagination.DefaultPageSize
		}
	}
	if pageSize > limit {
		pageSize = limit
	}
	return pageSize
}

// parseSortParam 解析并校验单个排序片段 "<column>[ ASC|DESC]"。
// 列名必须匹配 sortColumnRe；提供白名单时列名（不区分大小写）必须命中
// 别名，且返回值用白名单的**规范化列名**（#165）而非用户原文——别名
// （如 json 名 "displayName"）与真列名（"display_name"）就此解耦，出
// SQL 的恒是 DB 里存在的列。白名单为 nil（WithSortValidation(false)）时
// 保留 v0.8.14 口径：用户原文透传（只做标识符正则校验）。
func parseSortParam(seg string, allowlist map[string]string) (string, error) {
	fields := strings.Fields(seg)
	if len(fields) == 0 || len(fields) > 2 {
		return "", errors.NewBadRequestError("invalid sort segment: " + seg)
	}
	col := fields[0]
	if !sortColumnRe.MatchString(col) {
		return "", errors.NewBadRequestError("invalid sort column: " + col)
	}
	if allowlist != nil {
		canon, ok := allowlist[strings.ToLower(col)]
		if !ok {
			return "", errors.NewBadRequestError("column not sortable: " + col)
		}
		col = canon
	}
	if len(fields) == 2 {
		switch strings.ToUpper(fields[1]) {
		case "ASC", "DESC":
			return col + " " + strings.ToUpper(fields[1]), nil
		default:
			return "", errors.NewBadRequestError("invalid sort direction: " + fields[1])
		}
	}
	return col, nil
}
