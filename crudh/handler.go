package crudh

import (
	"context"
	"reflect"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/oekit/gormx/crud"
	"github.com/oekit/hertz/validatorh"
	mw "github.com/oekit/middleware"
	"github.com/oekit/pkg/errors"
)

// defaultIDParam 是路径参数的缺省名字（hertz 缺省路由模板用 ":id"）。
const defaultIDParam = "id"

// Op 标识 Handler 的标准 CRUD 操作。
// 响应写出钩子（[WithResponseWriter]）按 Op 区分映射策略：
// Create/Get/Update 的 data 为实体 *T，List 为 *crud.PageResponse[T]，Delete 为 nil。
type Op string

// 标准 CRUD 五操作。
const (
	OpCreate Op = "create"
	OpGet    Op = "get"
	OpList   Op = "list"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
)

// Route 是 [WithRouteTemplate] 的模板条目：Method+Path 定位，Op/Handler 二选一
// 绑定处理函数。子资源/动作端点（Handler）与标准 CRUD（Op）在同一模板内并存，
// 注册顺序即声明顺序，路由布局完全由消费方决定。
type Route struct {
	// Method 是 HTTP 方法（如 consts.MethodPost）。
	Method string
	// Path 是相对 RegisterRoutes prefix 的子路径："" 表示 prefix 本身，
	// 否则必须以 / 开头（如 "/:id"、"/:type/items"、"/cache/refresh"）。
	Path string
	// Op 绑定到本 Handler 的标准 CRUD 操作；与 Handler 二选一。
	Op Op
	// Handler 是自定义端点（子资源/动作）处理函数；与 Op 二选一。
	Handler app.HandlerFunc
}

// validate 校验模板条目，返回非空字符串表示非法原因。
func (r Route) validate() string {
	if r.Method == "" {
		return "empty method"
	}
	if r.Path != "" && !strings.HasPrefix(r.Path, "/") {
		return "path must be empty or start with '/': " + r.Path
	}
	hasOp := r.Op != ""
	hasHandler := r.Handler != nil
	if hasOp == hasHandler {
		return "exactly one of Op/Handler must be set"
	}
	if hasOp {
		switch r.Op {
		case OpCreate, OpGet, OpList, OpUpdate, OpDelete:
		default:
			return "unknown op: " + string(r.Op)
		}
	}
	return ""
}

// DefaultRoutes 返回缺省 5 条 REST 路由模板（未配置 [WithRouteTemplate] 时的
// 注册布局）：POST/GET prefix、GET/PUT/DELETE prefix/:id。返回值是新副本，
// 可增删/修改后传给 WithRouteTemplate（如保留标准 CRUD、追加子资源与动作端点）。
func DefaultRoutes() []Route {
	return []Route{
		{Method: consts.MethodPost, Path: "", Op: OpCreate},
		{Method: consts.MethodGet, Path: "", Op: OpList},
		{Method: consts.MethodGet, Path: "/:id", Op: OpGet},
		{Method: consts.MethodPut, Path: "/:id", Op: OpUpdate},
		{Method: consts.MethodDelete, Path: "/:id", Op: OpDelete},
	}
}

// Handler 是通用 CRUD HTTP Handler。
// ID 为实体主键类型（int64 自增 / string 文本主键等，见 crud.Service），
// 路径参数经 IDParser 解析为 ID。
type Handler[T any, ID comparable, CreateReq, UpdateReq any] struct {
	svc crud.Service[T, ID, CreateReq, UpdateReq]
	// idParser 是路径参数 → ID 的解析函数（WithIDParser 注入）；
	// nil 时用缺省解析（见 defaultParseID）。
	idParser func(string) (ID, error)
	// sortable 是排序白名单：键为小写别名，值为出 SQL 用的**规范化列名**。
	// nil 表示按实体类型 T 的字段反射生成（缺省口径，见 [WithSortableColumns] /
	// [WithSortValidation]）。#165：别名（json 名等）与真列名（gorm column tag
	// 或蛇形转换）在此解耦——校验查别名，出 SQL 恒用规范化名，用户原文不进
	// ORDER BY。
	sortable map[string]string
	// sortValidation 为 true 时排序列必须命中白名单；false 时只做标识符格式
	// 校验（全放开，v0.8.14 缺省口径，见 [WithSortValidation]）。
	sortValidation bool
	// pageNormalization 为 true 时分页参数归一（v0.9.0 缺省口径：回显的
	// page/page_size 恒等于 SQL 实际 LIMIT）；false 时保留 v0.8.14 逐字口径
	// （不归一、缺省不分页，见 [WithPageNormalization]）。
	pageNormalization bool
	// defaultPageSize 是 page_size 缺省/非正时生效的每页条数；<=0 用包缺省 20。
	defaultPageSize int
	// pageSizeLimit 是每页条数上限；<=0 用包缺省 500。
	pageSizeLimit int
	// idParam 是路径参数名（[WithIDParam] 注入或按路由模板推导）；缺省 "id"。
	idParam string
	// idParamExplicit 标记 idParam 是否被显式指定（[WithIDParam]）；
	// 显式指定优先于按路由模板推导。
	idParamExplicit bool
	// responseWriter 是成功响应写出钩子（WithResponseWriter 注入）；
	// nil 时用 mw.SuccessResp（与历史行为一致）。
	responseWriter ResponseWriter
	// errMapper 是错误映射钩子（WithErrMapper 注入）；nil 时不映射。
	errMapper ErrMapper
	// routes 是路由模板（WithRouteTemplate 注入）：非 nil 即生效（空=不注册）；
	// nil 时按 DefaultRoutes() 布局注册。
	routes []Route
	// queryBinder 是 List 查询参数绑定钩子（WithQueryBinder 注入）；
	// nil 时按 listParamsDisabled 与缺省绑定处理。
	queryBinder QueryBinder
	// listParamsDisabled 为 true 时 List 不绑定任何查询参数
	// （WithListParams(false)；存量 IDL 契约：忽略分页/排序参数、恒全量返回）。
	listParamsDisabled bool
	// idParseErrHandler 是路径参数解析失败的出口钩子（WithIDParseErrorHandler 注入）；
	// nil 时用缺省出口 badRequest("invalid id")。
	idParseErrHandler IDParseErrorHandler
	// deleteFoundPolicy 是 Delete 的 found 口径（WithDeleteFoundPolicy 注入）；
	// 零值 = DeleteFoundNotFound（缺省）。
	deleteFoundPolicy DeleteFoundPolicy
	// deleteCascade 是删除级联钩子（WithDeleteCascade 注入）；nil 时无级联。
	deleteCascade func(ctx context.Context, c *app.RequestContext, id ID, del func(context.Context) error) error
}

// NewHandler 创建 CRUD Handler。
func NewHandler[T any, ID comparable, CreateReq, UpdateReq any](svc crud.Service[T, ID, CreateReq, UpdateReq], opts ...HandlerOption[T, ID, CreateReq, UpdateReq]) *Handler[T, ID, CreateReq, UpdateReq] {
	h := &Handler[T, ID, CreateReq, UpdateReq]{
		svc:               svc,
		sortValidation:    true,
		pageNormalization: true,
		idParam:           defaultIDParam,
	}
	for _, opt := range opts {
		opt(h)
	}
	// 排序白名单缺省按实体类型 T 的字段反射生成（消费方拿不到实体字段 ⇒
	// 空集 ⇒ 白名单外的列一律 400）。显式设置过（WithSortableColumns，
	// 含显式空调用）则保持调用方口径。
	if h.sortValidation && h.sortable == nil {
		h.sortable = entityColumns[T]()
	}
	// 路径参数名：未显式指定时按路由模板推导（推导不出回退 "id"）。
	if !h.idParamExplicit {
		h.idParam = deriveIDParam(h.routes)
	}
	return h
}

// deriveIDParam 按路由模板推导路径参数名：取标准 CRUD 的 Get/Update/Delete
// 三条路由路径里的首个 ":param"；多条路由参数名一致才采用，否则回退 "id"。
// 自定义端点（Route.Handler）与 List 路由不参与推导。
func deriveIDParam(routes []Route) string {
	if routes == nil {
		routes = DefaultRoutes()
	}
	name := ""
	for _, r := range routes {
		switch r.Op {
		case OpGet, OpUpdate, OpDelete:
		default:
			continue
		}
		p := firstPathParam(r.Path)
		if p == "" {
			continue
		}
		if name == "" {
			name = p
			continue
		}
		if name != p {
			return defaultIDParam
		}
	}
	if name == "" {
		return defaultIDParam
	}
	return name
}

// firstPathParam 返回路径里首个 ":param" 段的参数名（无则空串）。
func firstPathParam(path string) string {
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, ":") && len(seg) > 1 {
			return seg[1:]
		}
	}
	return ""
}

// write 成功响应出口：注入 ResponseWriter 时走钩子，否则 mw.SuccessResp（行为不变）。
func (h *Handler[T, ID, C, U]) write(ctx context.Context, c *app.RequestContext, op Op, data any) {
	if h.responseWriter != nil {
		h.responseWriter(ctx, c, op, data)
		return
	}
	mw.SuccessResp(c, data)
}

// fail 错误统一出口：先经 ErrMapper（若有）映射为消费方码表，再走 mw.Fail。
func (h *Handler[T, ID, C, U]) fail(ctx context.Context, c *app.RequestContext, err error) {
	if h.errMapper != nil {
		if mapped := h.errMapper(err); mapped != nil {
			err = mapped
		}
	}
	mw.Fail(ctx, c, err)
}

// badRequest 构造本地参数类错误（id/分页/排序解析失败）。
// 用 NewBizError(400, ...) 而非 NewBadRequestError：mw.Fail 的 BizError 渲染
// （HTTPCode=0 经 StatusMapper 推导）与历史 mw.ErrorResp(c, 400, msg) 输出逐字一致。
func badRequest(msg string) error {
	return errors.NewBizError(400, msg)
}

// idParseError 路径参数解析失败的出口：注入 [IDParseErrorHandler] 时用钩子给出的
// 错误（钩子返回 nil 则回退缺省，不吞错误），否则用缺省 badRequest("invalid id")
// ——固定文案为历史对外契约（不透出解析细节），保持逐字不变。
func (h *Handler[T, ID, C, U]) idParseError(ctx context.Context, c *app.RequestContext, err error) error {
	if h.idParseErrHandler != nil {
		if out := h.idParseErrHandler(ctx, c, err); out != nil {
			return out
		}
	}
	return badRequest("invalid id")
}

// isNotFoundErr 判断错误是否为「记录不存在」形态：crud.ErrNotFound 哨兵
// （errors.Is 链判定，含 crud.BaseService 的 not-found 包装）或 404 业务码
// （BizCode / HTTPCode 任一为 404）。
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, crud.ErrNotFound) {
		return true
	}
	if be, ok := errors.AsBizError(err); ok && be != nil {
		return be.BizCode == 404 || be.HTTPCode == 404
	}
	return false
}

// Create 处理 POST 创建请求。
func (h *Handler[T, ID, C, U]) Create(ctx context.Context, c *app.RequestContext) {
	var req C
	if err := validatorh.BindAndValidate(c, &req); err != nil {
		h.fail(ctx, c, err)
		return
	}

	entity, err := h.svc.Create(ctx, &req)
	if err != nil {
		h.fail(ctx, c, err)
		return
	}

	h.write(ctx, c, OpCreate, entity)
}

// Get 处理 GET 单个获取请求。
func (h *Handler[T, ID, C, U]) Get(ctx context.Context, c *app.RequestContext) {
	id, err := h.parseID(c)
	if err != nil {
		h.fail(ctx, c, h.idParseError(ctx, c, err))
		return
	}

	entity, err := h.svc.Get(ctx, id)
	if err != nil {
		h.fail(ctx, c, err)
		return
	}

	h.write(ctx, c, OpGet, entity)
}

// List 处理 GET 列表查询请求（带分页）。
func (h *Handler[T, ID, C, U]) List(ctx context.Context, c *app.RequestContext) {
	q := crud.NewQuery()

	// 绑定查询参数：自定义 QueryBinder 优先，其次缺省绑定开关，最后缺省绑定。
	if bindErr := h.bindQuery(ctx, c, q); bindErr != nil {
		h.fail(ctx, c, bindErr)
		return
	}

	resp, err := h.svc.List(ctx, q)
	if err != nil {
		h.fail(ctx, c, err)
		return
	}

	// v0.8.14 逐字回显（WithPageNormalization(false) 契约）：信封回显**请求
	// 原值**，不跟随下游入口归一。gormx v0.11.12（#155）起 crud.Query.Page 会
	// 在入口把 (1,0) 归一成 (1,20)、超上限钳 500——信封若取 GetPage() 字段即
	// 回显归一值，兼容档消费者的响应格式会漂。SQL 行为两侧一致（PageRequest
	// 的 Limit()/Offset() 读时归一历来如此），此处只回填信封回显面：
	// page/page_size 回显原值、pages 按原值口径（未分页/0 页宽 = 0）。
	// 仅缺省绑定路径生效——自定义 QueryBinder / WithListParams(false) 的
	// 语义归各自开关管辖。
	if !h.pageNormalization && h.queryBinder == nil && !h.listParamsDisabled {
		page, _ := strconv.Atoi(c.Query("page"))
		pageSize, _ := strconv.Atoi(c.Query("page_size"))
		if resp != nil {
			resp.Page, resp.PageSize = page, pageSize
			if pageSize > 0 {
				resp.Pages = int((resp.Total + int64(pageSize) - 1) / int64(pageSize))
			} else {
				resp.Pages = 0
			}
		}
	}

	h.write(ctx, c, OpList, resp)
}

// Update 处理 PUT 更新请求。
func (h *Handler[T, ID, C, U]) Update(ctx context.Context, c *app.RequestContext) {
	id, err := h.parseID(c)
	if err != nil {
		h.fail(ctx, c, h.idParseError(ctx, c, err))
		return
	}

	var req U
	if err = validatorh.BindAndValidate(c, &req); err != nil {
		h.fail(ctx, c, err)
		return
	}

	entity, err := h.svc.Update(ctx, id, &req)
	if err != nil {
		h.fail(ctx, c, err)
		return
	}

	h.write(ctx, c, OpUpdate, entity)
}

// Delete 处理 DELETE 删除请求。
func (h *Handler[T, ID, C, U]) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := h.parseID(c)
	if err != nil {
		h.fail(ctx, c, h.idParseError(ctx, c, err))
		return
	}

	// del 是主记录删除执行器：级联钩子可调用它决定删除时机（可在消费方事务内调用）。
	delCalled := false
	del := func(dctx context.Context) error {
		delCalled = true
		return h.svc.Delete(dctx, id)
	}

	var deleteErr error
	if h.deleteCascade != nil {
		deleteErr = h.deleteCascade(ctx, c, id, del)
	}
	if deleteErr == nil && !delCalled {
		deleteErr = del(ctx)
	}

	if deleteErr != nil {
		// 幂等口径：记录不存在按成功处理（存量 DeleteDictItem「删不存在 = 200」）。
		if h.deleteFoundPolicy == DeleteFoundSuccess && isNotFoundErr(deleteErr) {
			h.write(ctx, c, OpDelete, nil)
			return
		}
		h.fail(ctx, c, deleteErr)
		return
	}

	h.write(ctx, c, OpDelete, nil)
}

// RegisterRoutes 注册 CRUD 路由到 Hertz Group。
// 未配置 WithRouteTemplate 时按 [DefaultRoutes] 布局注册 5 条 REST：
// POST/GET prefix、GET/PUT/DELETE prefix/:id；配置后按模板逐条注册——
// 子资源/动作端点与标准 CRUD 并存，省略某条即不注册该端点。
func (h *Handler[T, ID, C, U]) RegisterRoutes(rg *route.RouterGroup, prefix string) {
	routes := h.routes
	if routes == nil {
		routes = DefaultRoutes()
	}
	for _, r := range routes {
		if r.Handler != nil {
			rg.Handle(r.Method, prefix+r.Path, r.Handler)
			continue
		}
		rg.Handle(r.Method, prefix+r.Path, h.opHandler(r.Op))
	}
}

// opHandler 返回 Op 对应的标准 CRUD 处理函数；未知 Op panic
// （模板在 WithRouteTemplate 已校验，此处为防御性兜底）。
func (h *Handler[T, ID, C, U]) opHandler(op Op) app.HandlerFunc {
	switch op {
	case OpCreate:
		return h.Create
	case OpGet:
		return h.Get
	case OpList:
		return h.List
	case OpUpdate:
		return h.Update
	case OpDelete:
		return h.Delete
	}
	panic("crudh: unknown op: " + string(op))
}

// parseID 从 URL 参数中解析 ID（WithIDParser 或缺省解析）。
// 读取的参数名由 [WithIDParam] 指定（缺省 "id"；未显式设置时按路由模板推导）。
func (h *Handler[T, ID, C, U]) parseID(c *app.RequestContext) (ID, error) {
	idStr := c.Param(h.idParam)
	if h.idParser != nil {
		return h.idParser(idStr)
	}
	return defaultParseID[ID](idStr)
}

// defaultParseID 缺省路径参数解析：int 系主键 strconv.ParseInt（宽度随
// ID 类型，int64 与 v0.6 之前行为一致）、string 主键直通（含命名 string
// 类型，如 type OrderID string，#178）；其余 comparable 类型不支持缺省
// 解析（须 WithIDParser 注入），一律按 400 拒绝。
func defaultParseID[ID comparable](s string) (ID, error) {
	var zero ID
	t := reflect.TypeOf(zero)
	if t == nil {
		return zero, errors.NewBadRequestError("invalid id: " + s)
	}
	switch t.Kind() {
	case reflect.String:
		// 命名 string 类型不能 any(s).(ID) 硬断言：装箱的动态类型是底层
		// string，断言到命名类型必 panic——端点「注册成功但每个 GET 都是
		// 500」（#178）。与 int/uint 分支同构，反射构造命名类型值。
		out := reflect.New(t).Elem()
		out.SetString(s)
		return out.Interface().(ID), nil
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
