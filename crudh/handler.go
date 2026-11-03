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
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/oekit/gormx/crud"
	"github.com/oekit/hertz/validatorh"
	mw "github.com/oekit/middleware"
	"github.com/oekit/pkg/errors"
)

// sortColumnRe 严格匹配 SQL 标识符（字母/下划线开头，仅字母数字下划线）。
// 排序列名必须通过此校验，杜绝 ORDER BY 注入。
var sortColumnRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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

// ResponseWriter 是成功响应写出钩子：将 CRUD 数据（见 [Op]）映射为消费方
// 信封 DTO 并写入响应。缺省为 mw.SuccessResp——统一信封 {code,data,message,trace_id}，
// 与未配置时逐字一致；消费方可在此换成自有 DTO（如 IDResp{ID} /
// ListXxxResp{List} / EmptyResp{Success}）与自有信封。
type ResponseWriter func(ctx context.Context, c *app.RequestContext, op Op, data any)

// ErrMapper 是错误映射钩子：将 crudh/服务层错误映射为消费方码表的 BizError
// （如 404→40401），再经统一出口 mw.Fail 渲染（HTTP 状态由消费方 StatusMapper
// 推导）。返回 nil 表示保留原错误。缺省不映射，与未配置时逐字一致。
type ErrMapper func(err error) error

// QueryBinder 是 List 查询参数绑定钩子：把请求查询参数绑定到 crud.Query。
// 返回 error 表示绑定失败（handler 统一错误出口 mw.Fail 渲染）；返回 nil 继续查询。
// 缺省绑定（page/page_size/sort，非法值 400）见 [Handler.BindDefaultQuery]。
type QueryBinder func(ctx context.Context, c *app.RequestContext, q *crud.Query) error

// IDParseErrorHandler 是路径参数解析失败的出口钩子：把解析错误转换为对外错误。
// 缺省出口为 BizError(400, "invalid id")（历史对外契约，不透出解析细节）。
type IDParseErrorHandler func(ctx context.Context, c *app.RequestContext, err error) error

// IDParseErrorPassthrough 是「解析器自带错误即出口」的现成实现：原样返回解析错误，
// 由 [WithErrMapper] 映射进消费方码表（如自有绑定错误码 40001）。
func IDParseErrorPassthrough(_ context.Context, _ *app.RequestContext, err error) error {
	return err
}

// DeleteFoundPolicy 是 Delete 的 found 口径策略（记录不存在时的对外形态）。
type DeleteFoundPolicy int

const (
	// DeleteFoundNotFound：记录不存在 → not-found 错误（缺省，与历史一致）。
	DeleteFoundNotFound DeleteFoundPolicy = iota
	// DeleteFoundSuccess：记录不存在 → 视为删除成功（幂等口径，如存量
	// DeleteDictItem「删不存在 = 200」契约）。
	DeleteFoundSuccess
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
	// sortable 是排序白名单（小写列名）；nil 表示仅做标识符格式校验
	sortable map[string]struct{}
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

// WithResponseWriter 注入成功响应写出钩子（entity/PageResponse → 信封 DTO）。
// 钩子负责映射并写出完整响应；未注入时用 mw.SuccessResp，与历史行为逐字一致。
func WithResponseWriter[T any, ID comparable, CreateReq, UpdateReq any](fn ResponseWriter) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.responseWriter = fn }
}

// WithErrMapper 注入错误映射钩子（crud 错误 → 消费方码表）。
// 钩子作用于 handler 全部错误出口（服务层错误、绑定校验、路径参数/分页/排序解析），
// 返回 nil 保留原错误；未注入时不映射，与历史行为逐字一致。
// 错误渲染仍统一走 mw.Fail（handler 统一错误出口不变）。
func WithErrMapper[T any, ID comparable, CreateReq, UpdateReq any](fn ErrMapper) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.errMapper = fn }
}

// WithRouteTemplate 以完整模板替换缺省路由布局（缺省见 [DefaultRoutes]）。
// 模板逐条注册：Op 绑定标准 CRUD 操作，Handler 挂自定义子资源/动作端点，二者并存；
// 省略某条即不注册该端点（如「无 GET :id」）。传空模板 = 不注册任何路由
// （自行注册时抑制缺省布局用）。
// 模板非法（Method 为空、Path 非 "" 且不以 / 开头、Op/Handler 同缺或同给、
// Op 非标准五操作）时 panic——属构造期配置错误，启动即暴露。
func WithRouteTemplate[T any, ID comparable, CreateReq, UpdateReq any](routes ...Route) HandlerOption[T, ID, CreateReq, UpdateReq] {
	tpl := make([]Route, len(routes))
	for i, r := range routes {
		if reason := r.validate(); reason != "" {
			panic("crudh: invalid route template: " + reason)
		}
		tpl[i] = r
	}
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.routes = tpl }
}

// WithQueryBinder 注入 List 查询参数绑定钩子（替换缺省绑定）。
//
// 钩子拿到 crud.Query 与请求上下文，自行绑定分页/排序/过滤条件；注入后缺省绑定
// （page/page_size/sort，非法值 400）不再执行——自定义钩子全权接管。
// 需要保留缺省口径（分页 + 排序白名单）再加自定义过滤时，在钩子内调用
// [Handler.BindDefaultQuery]。
// 钩子返回 error 表示绑定失败，经 handler 统一错误出口 mw.Fail 渲染。
// 未注入时按 [WithListParams] 与缺省绑定处理。
func WithQueryBinder[T any, ID comparable, CreateReq, UpdateReq any](fn QueryBinder) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.queryBinder = fn }
}

// WithListParams 控制 List 是否绑定缺省查询参数（page/page_size/sort）。
//
// 传 false → 完全忽略查询参数：不分页、不排序，非法值（如 page=x）也不再 400，
// 恒全量返回——对齐存量 IDL BindAndValidate 契约（忽略这些参数、恒全量返回）。
// 缺省（未设置）= 绑定，行为与历史逐字一致。
// 与 [WithQueryBinder] 同时设置时以自定义钩子为准（钩子全权接管绑定）。
func WithListParams[T any, ID comparable, CreateReq, UpdateReq any](enabled bool) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.listParamsDisabled = !enabled }
}

// WithIDParseErrorHandler 注入路径参数解析失败的出口钩子。
//
// 钩子拿到解析错误（WithIDParser 注入的解析函数返回的错误，或缺省解析的错误），
// 返回对外错误；返回 nil → 回退缺省出口 BizError(400, "invalid id")（不吞错误）。
// 出口错误仍经 [WithErrMapper] 映射进消费方码表。
// 未注入时为缺省出口，与历史输出逐字一致。
func WithIDParseErrorHandler[T any, ID comparable, CreateReq, UpdateReq any](fn IDParseErrorHandler) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.idParseErrHandler = fn }
}

// WithDeleteFoundPolicy 配置 Delete 的 found 口径（记录不存在时的对外形态）。
//
// 缺省 [DeleteFoundNotFound]：记录不存在 → not-found 错误（与历史一致）。
// [DeleteFoundSuccess]：记录不存在 → 视为删除成功（幂等删除，200）。
// 两种口径都只作用于「记录不存在」形态的错误，其他错误（如 500）照常失败。
func WithDeleteFoundPolicy[T any, ID comparable, CreateReq, UpdateReq any](policy DeleteFoundPolicy) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.deleteFoundPolicy = policy }
}

// WithDeleteCascade 注入删除级联钩子（跨实体级联删除）。
//
// 钩子签名 fn(ctx, c, id, del)：
//   - del 是主记录删除执行器（内部走 svc.Delete），钩子可调用它决定删除时机；
//   - 钩子返回 error → 删除中止（主记录不删），错误经统一出口 mw.Fail 渲染；
//   - 钩子返回 nil 且未调用 del → handler 随后执行主记录删除（前置级联语义）。
//
// 事务语义：crudh 层不持有数据源，跨实体级联的原子性由消费方保证——需要
// 「从属记录 + 主记录同事务」时，在钩子内开事务并把事务句柄经 ctx 传给 del
// （消费方自定 ctx 约定），del 即在该事务内执行。
func WithDeleteCascade[T any, ID comparable, CreateReq, UpdateReq any](fn func(ctx context.Context, c *app.RequestContext, id ID, del func(context.Context) error) error) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.deleteCascade = fn }
}

// NewHandler 创建 CRUD Handler。
func NewHandler[T any, ID comparable, CreateReq, UpdateReq any](svc crud.Service[T, ID, CreateReq, UpdateReq], opts ...HandlerOption[T, ID, CreateReq, UpdateReq]) *Handler[T, ID, CreateReq, UpdateReq] {
	h := &Handler[T, ID, CreateReq, UpdateReq]{svc: svc}
	for _, opt := range opts {
		opt(h)
	}
	return h
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
				return badRequest("invalid sort parameter")
			}
			q.Order(order)
		}
	}
	return nil
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
