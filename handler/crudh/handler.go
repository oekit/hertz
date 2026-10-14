// Package crudh 提供基于 Hertz 的通用 CRUD HTTP Handler。
//
// 将 HTTP 层（Handler）与数据层（crud.Repository/crud.Service）分离，
// 避免 data/crud 包反向依赖 middleware 包。
package crudh

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/yi-nology/ekit-core/pkg/errors"
	"github.com/yi-nology/ekit-gorm/crud"
	"github.com/yi-nology/ekit-hertz/handler/validatorh"
	mw "github.com/yi-nology/ekit-middleware/middleware"
)

// sortColumnRe 严格匹配 SQL 标识符（字母/下划线开头，仅字母数字下划线）。
// 排序列名必须通过此校验，杜绝 ORDER BY 注入。
var sortColumnRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Handler 是通用 CRUD HTTP Handler。
type Handler[T any, CreateReq, UpdateReq any] struct {
	svc crud.Service[T, CreateReq, UpdateReq]
	// sortable 是排序白名单（小写列名）；nil 表示仅做标识符格式校验
	sortable map[string]struct{}
}

// HandlerOption 是 [NewHandler] 的配置选项。
type HandlerOption[T any, CreateReq, UpdateReq any] func(*Handler[T, CreateReq, UpdateReq])

// WithSortableColumns 设置 List 接口排序参数的白名单列名。
// 未设置时仅做标识符格式校验；设置后白名单外的列名返回 400。
func WithSortableColumns[T any, CreateReq, UpdateReq any](cols ...string) HandlerOption[T, CreateReq, UpdateReq] {
	return func(h *Handler[T, CreateReq, UpdateReq]) {
		h.sortable = make(map[string]struct{}, len(cols))
		for _, col := range cols {
			h.sortable[strings.ToLower(col)] = struct{}{}
		}
	}
}

// NewHandler 创建 CRUD Handler。
func NewHandler[T any, CreateReq, UpdateReq any](svc crud.Service[T, CreateReq, UpdateReq], opts ...HandlerOption[T, CreateReq, UpdateReq]) *Handler[T, CreateReq, UpdateReq] {
	h := &Handler[T, CreateReq, UpdateReq]{svc: svc}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Create 处理 POST 创建请求。
func (h *Handler[T, C, U]) Create(ctx context.Context, c *app.RequestContext) {
	var req C
	if err := validatorh.BindAndValidate(c, &req); err != nil {
		mw.Fail(ctx, c, err)
		return
	}

	entity, err := h.svc.Create(ctx, &req)
	if err != nil {
		mw.Fail(ctx, c, err)
		return
	}

	mw.SuccessResp(c, entity)
}

// Get 处理 GET 单个获取请求。
func (h *Handler[T, C, U]) Get(ctx context.Context, c *app.RequestContext) {
	id, err := parseID(c)
	if err != nil {
		mw.ErrorResp(c, 400, "invalid id")
		return
	}

	entity, err := h.svc.Get(ctx, id)
	if err != nil {
		mw.Fail(ctx, c, err)
		return
	}

	mw.SuccessResp(c, entity)
}

// List 处理 GET 列表查询请求（带分页）。
func (h *Handler[T, C, U]) List(ctx context.Context, c *app.RequestContext) {
	q := crud.NewQuery()

	// 绑定分页参数（无效值返回 400）
	page, pageErr := strconv.Atoi(c.Query("page"))
	pageSize, sizeErr := strconv.Atoi(c.Query("page_size"))
	if pageErr != nil && c.Query("page") != "" {
		mw.ErrorResp(c, 400, "invalid page parameter")
		return
	}
	if sizeErr != nil && c.Query("page_size") != "" {
		mw.ErrorResp(c, 400, "invalid page_size parameter")
		return
	}
	if page > 0 || pageSize > 0 {
		q.Page(page, pageSize)
	}

	// 绑定排序参数：逗号分隔多列，每列 "<column>[ ASC|DESC]"。
	// 列名经严格标识符正则 + 白名单校验后重建，用户原文不进入 SQL，
	// 防止 ORDER BY 注入（如 sort=(SELECT CASE WHEN ...)）。
	if sort := c.Query("sort"); sort != "" {
		for _, seg := range strings.Split(sort, ",") {
			order, err := parseSortParam(seg, h.sortable)
			if err != nil {
				mw.ErrorResp(c, 400, "invalid sort parameter")
				return
			}
			q.Order(order)
		}
	}

	resp, err := h.svc.List(ctx, q)
	if err != nil {
		mw.Fail(ctx, c, err)
		return
	}

	mw.SuccessResp(c, resp)
}

// Update 处理 PUT 更新请求。
func (h *Handler[T, C, U]) Update(ctx context.Context, c *app.RequestContext) {
	id, err := parseID(c)
	if err != nil {
		mw.ErrorResp(c, 400, "invalid id")
		return
	}

	var req U
	if err = validatorh.BindAndValidate(c, &req); err != nil {
		mw.Fail(ctx, c, err)
		return
	}

	entity, err := h.svc.Update(ctx, id, &req)
	if err != nil {
		mw.Fail(ctx, c, err)
		return
	}

	mw.SuccessResp(c, entity)
}

// Delete 处理 DELETE 删除请求。
func (h *Handler[T, C, U]) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := parseID(c)
	if err != nil {
		mw.ErrorResp(c, 400, "invalid id")
		return
	}

	if err := h.svc.Delete(ctx, id); err != nil {
		mw.Fail(ctx, c, err)
		return
	}

	mw.SuccessResp(c, nil)
}

// RegisterRoutes 注册 CRUD 路由到 Hertz Group。
func (h *Handler[T, C, U]) RegisterRoutes(rg *route.RouterGroup, prefix string) {
	rg.POST(prefix, h.Create)
	rg.GET(prefix, h.List)
	rg.GET(prefix+"/:id", h.Get)
	rg.PUT(prefix+"/:id", h.Update)
	rg.DELETE(prefix+"/:id", h.Delete)
}

// parseID 从 URL 参数中解析 ID。
func parseID(c *app.RequestContext) (int64, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, errors.NewBadRequestError("invalid id: " + idStr)
	}
	return id, nil
}

// parseSortParam 解析并校验单个排序片段 "<column>[ ASC|DESC]"。
// 列名必须匹配 sortColumnRe；提供白名单时列名（不区分大小写）必须在列。
// 返回值由白名单内列名与规范化方向重组而成，而非用户原文拼接。
func parseSortParam(seg string, allowlist map[string]struct{}) (string, error) {
	fields := strings.Fields(seg)
	if len(fields) == 0 || len(fields) > 2 {
		return "", errors.NewBadRequestError("invalid sort segment: " + seg)
	}
	col := fields[0]
	if !sortColumnRe.MatchString(col) {
		return "", errors.NewBadRequestError("invalid sort column: " + col)
	}
	if allowlist != nil {
		if _, ok := allowlist[strings.ToLower(col)]; !ok {
			return "", errors.NewBadRequestError("column not sortable: " + col)
		}
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
