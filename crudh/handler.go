// Package crudh 提供基于 Hertz 的通用 CRUD HTTP Handler。
//
// 将 HTTP 层（Handler）与数据层（crud.Repository/crud.Service）分离，
// 避免 data/crud 包反向依赖 middleware 包。
package crudh

import (
	"context"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/oekit/gormx/crud"
	"github.com/oekit/hertz/validatorh"
	mw "github.com/oekit/middleware"
	"github.com/oekit/pkg/errors"
)

// sortColumnRe 严格匹配 SQL 标识符（字母/下划线开头，仅字母数字下划线）。
// 排序列名必须通过此校验，杜绝 ORDER BY 注入。
var sortColumnRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Handler 是通用 CRUD HTTP Handler。
// ID 为实体主键类型（int64 自增 / string 文本主键等，见 crud.Service），
// 路径参数经 IDParser 解析为 ID。
type Handler[T any, ID comparable, CreateReq, UpdateReq any] struct {
	svc crud.Service[T, ID, CreateReq, UpdateReq]
	// idParser 是路径参数 → ID 的解析函数（WithIDParser 注入）；
	// nil 时用缺省解析（见 defaultParseID）。
	idParser func(string) (ID, error)
	// sortable 是排序白名单（小写列名）；nil 表示仅做标识符格式校验
	sortable map[string]struct{}
}

// HandlerOption 是 [NewHandler] 的配置选项。
type HandlerOption[T any, ID comparable, CreateReq, UpdateReq any] func(*Handler[T, ID, CreateReq, UpdateReq])

// WithIDParser 注入路径参数 → ID 的解析函数。
//
// 缺省解析：int 系主键按 strconv.ParseInt（宽度随 ID 类型，与 v0.6 之前
// int64 行为一致）、string 主键直通；其余 comparable 类型必须显式注入，
// 缺省解析失败按 400 拒绝（fail-closed，绝不猜测）。
// 文本主键项目传恒等解析即可：`WithIDParser[Doc, string, C, U](func(s string) (string, error) { return s, nil })`。
func WithIDParser[T any, ID comparable, CreateReq, UpdateReq any](fn func(string) (ID, error)) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.idParser = fn }
}

// WithSortableColumns 设置 List 接口排序参数的白名单列名。
// 未设置时仅做标识符格式校验；设置后白名单外的列名返回 400。
func WithSortableColumns[T any, ID comparable, CreateReq, UpdateReq any](cols ...string) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) {
		h.sortable = make(map[string]struct{}, len(cols))
		for _, col := range cols {
			h.sortable[strings.ToLower(col)] = struct{}{}
		}
	}
}

// NewHandler 创建 CRUD Handler。
func NewHandler[T any, ID comparable, CreateReq, UpdateReq any](svc crud.Service[T, ID, CreateReq, UpdateReq], opts ...HandlerOption[T, ID, CreateReq, UpdateReq]) *Handler[T, ID, CreateReq, UpdateReq] {
	h := &Handler[T, ID, CreateReq, UpdateReq]{svc: svc}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Create 处理 POST 创建请求。
func (h *Handler[T, ID, C, U]) Create(ctx context.Context, c *app.RequestContext) {
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
func (h *Handler[T, ID, C, U]) Get(ctx context.Context, c *app.RequestContext) {
	id, err := h.parseID(c)
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
func (h *Handler[T, ID, C, U]) List(ctx context.Context, c *app.RequestContext) {
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
func (h *Handler[T, ID, C, U]) Update(ctx context.Context, c *app.RequestContext) {
	id, err := h.parseID(c)
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
func (h *Handler[T, ID, C, U]) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := h.parseID(c)
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
func (h *Handler[T, ID, C, U]) RegisterRoutes(rg *route.RouterGroup, prefix string) {
	rg.POST(prefix, h.Create)
	rg.GET(prefix, h.List)
	rg.GET(prefix+"/:id", h.Get)
	rg.PUT(prefix+"/:id", h.Update)
	rg.DELETE(prefix+"/:id", h.Delete)
}

// parseID 从 URL 参数中解析 ID（WithIDParser 或缺省解析）。
func (h *Handler[T, ID, C, U]) parseID(c *app.RequestContext) (ID, error) {
	idStr := c.Param("id")
	if h.idParser != nil {
		return h.idParser(idStr)
	}
	return defaultParseID[ID](idStr)
}

// defaultParseID 缺省路径参数解析：int 系主键 strconv.ParseInt（宽度随
// ID 类型，int64 与 v0.6 之前行为一致）、string 主键直通；其余 comparable
// 类型不支持缺省解析（须 WithIDParser 注入），一律按 400 拒绝。
func defaultParseID[ID comparable](s string) (ID, error) {
	var zero ID
	t := reflect.TypeOf(zero)
	if t == nil {
		return zero, errors.NewBadRequestError("invalid id: " + s)
	}
	switch t.Kind() {
	case reflect.String:
		return any(s).(ID), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v, err := strconv.ParseInt(s, 10, t.Bits())
		if err != nil {
			return zero, errors.NewBadRequestError("invalid id: " + s)
		}
		out := reflect.New(t).Elem()
		out.SetInt(v)
		return out.Interface().(ID), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v, err := strconv.ParseUint(s, 10, t.Bits())
		if err != nil {
			return zero, errors.NewBadRequestError("invalid id: " + s)
		}
		out := reflect.New(t).Elem()
		out.SetUint(v)
		return out.Interface().(ID), nil
	default:
		return zero, errors.NewBadRequestError("invalid id: " + s)
	}
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
