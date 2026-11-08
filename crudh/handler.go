// Package crudh 提供基于 Hertz 的通用 CRUD HTTP Handler。
//
// 将 HTTP 层（Handler）与数据层（crud.Repository/crud.Service）分离，
// 避免 data/crud 包反向依赖 middleware 包。
package crudh

import (
	"context"
	"database/sql/driver"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/oekit/gormx/crud"
	"github.com/oekit/hertz/validatorh"
	mw "github.com/oekit/middleware"
	"github.com/oekit/pkg/errors"
	"github.com/oekit/pkg/pagination"
)

// sortColumnRe 严格匹配 SQL 标识符（字母/下划线开头，仅字母数字下划线）。
// 排序列名必须通过此校验，杜绝 ORDER BY 注入。
var sortColumnRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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

// WithSortableColumns 设置 List 接口排序参数的白名单列名（列名小写匹配）。
//
// v0.9.0 起**未设置时白名单按实体类型 T 的字段反射生成**——候选列名与 DB
// 真实列名对齐（gorm `column:` tag 或 [gorm NamingStrategy] 蛇形转换，#165），
// json tag 名作为别名放行但出 SQL 时一律改用规范化列名——白名单外的列返回
// 400 "invalid sort parameter"（不再让不存在的列进 ORDER BY 触发 DB 报错 ⇒ 5xx）。
//
// 显式设置时以本白名单为准（不再叠加反射结果），白名单值按调用方书写形式
// 原样进 ORDER BY（显式白名单即调用方声明的真实列名）。显式空调用
// （WithSortableColumns()）= 空白名单 = **全部拒绝**（与「未设置」语义相反，
// 两者不再混同）。需要 v0.8.14 的「任意标识符列均可排序」用
// [WithSortValidation](false)。
//
// [gorm NamingStrategy]: https://github.com/go-gorm/gorm/blob/master/schema/naming.go
func WithSortableColumns[T any, ID comparable, CreateReq, UpdateReq any](cols ...string) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) {
		h.sortable = make(map[string]string, len(cols))
		for _, col := range cols {
			h.sortable[strings.ToLower(col)] = col
		}
	}
}

// WithSortValidation 控制排序参数是否做白名单校验（v0.9.0 缺省 = true）。
//
// true（缺省）：排序列必须命中白名单——白名单缺省按实体类型 T 的字段反射
// 生成（见 [WithSortableColumns]），白名单外的列返回 400 "invalid sort parameter"。
//
// false：只做标识符格式校验（**全放开**）——v0.8.14 缺省口径（未设
// [WithSortableColumns] 时任意标识符列都进 ORDER BY，列不存在时 DB 报
// no such column ⇒ 客户端拿到 5xx 而非 400）。
//
// 显式开关取代「不传 = 全放开」的隐式语义；与 [WithSortableColumns] 同时
// 设置时本开关优先（全放开，白名单不参与校验）。
func WithSortValidation[T any, ID comparable, CreateReq, UpdateReq any](enabled bool) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.sortValidation = enabled }
}

// WithDefaultPageSize 设置 List 的缺省每页条数（page_size 缺省/非正时生效）。
//
// n > 0 → 使用 n；n <= 0 → 用包缺省 [pagination.DefaultPageSize]（20）。
// 生效值恒不超过有效上限（[WithPageSizeLimit]）与 [pagination.MaxPageSize]
// （500）——超过上限的配置按上限生效，保证响应回显与 SQL 实际 LIMIT 恒等。
//
// 仅在归一口径下生效（缺省）；[WithPageNormalization](false) 时不生效。
func WithDefaultPageSize[T any, ID comparable, CreateReq, UpdateReq any](n int) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.defaultPageSize = n }
}

// WithPageSizeLimit 设置 List 的每页条数上限（page_size 超过即按上限截断）。
//
// n > 0 → 上限为 min(n, [pagination.MaxPageSize])；n <= 0 → 用包缺省上限 500。
//
// ⚠️ 上限无法突破 500：下游 pkg/pagination 的 PageRequest.Limit() 硬性封顶
// 500（本仓不升级该依赖），放宽到 500 以上不会被下游兑现——按 500 生效，
// 保证响应回显与 SQL 实际 LIMIT 恒等。
//
// 仅在归一口径下生效（缺省）；[WithPageNormalization](false) 时不生效。
func WithPageSizeLimit[T any, ID comparable, CreateReq, UpdateReq any](n int) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.pageSizeLimit = n }
}

// WithPageNormalization 控制分页参数是否归一（v0.9.0 缺省 = true）。
//
// true（缺省）：分页参数归一——page 缺省/非正 → 1，page_size 缺省/非正 →
// 20（或 [WithDefaultPageSize]），上限 500（或 [WithPageSizeLimit]）；
// **缺省请求也分页**（SQL 带 LIMIT，不再全表扫描），响应回显的
// page/page_size 恒等于 SQL 实际 LIMIT。
//
// false：保留 v0.8.14 逐字口径——不归一（回显原值），且 page/page_size 均
// 缺省或非正时不分页（SQL 无 LIMIT，恒全量返回）。
//
// 注意：与 [WithListParams](false) 不同——后者忽略全部查询参数（连排序一起），
// 本开关只回退分页归一，排序口径仍由 [WithSortValidation] 决定。
func WithPageNormalization[T any, ID comparable, CreateReq, UpdateReq any](enabled bool) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) { h.pageNormalization = enabled }
}

// WithIDParam 设置路径参数名（Get/Update/Delete 读取的 hertz 路由参数名）。
//
// 缺省 "id"（与 v0.8.14 逐字一致）；未显式设置时按 [WithRouteTemplate] 的
// 路径首个 ":param" 推导（推导不出或多条路由参数名不一致时回退 "id"）。
// 显式设置优先于推导。
//
// 用途：路由模板用非 "id" 的参数名（如 "/projects/:code"）时，
// 缺省实现只读 c.Param("id") ⇒ 取不到值（string 主键拿到空串 ⇒ 404，
// int 主键解析失败 ⇒ 400），端点「注册成功但永远不可用」。
func WithIDParam[T any, ID comparable, CreateReq, UpdateReq any](name string) HandlerOption[T, ID, CreateReq, UpdateReq] {
	return func(h *Handler[T, ID, CreateReq, UpdateReq]) {
		h.idParamExplicit = true
		if name == "" {
			h.idParam = defaultIDParam
			return
		}
		h.idParam = name
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

// entityColumns 按实体类型 T 的字段反射生成排序列白名单（#165、#175）。
//
// 返回「别名 → 规范化列名」映射：规范化列名与 gorm 的 DB 真实列名对齐
// （gorm `column:` tag 优先；无 tag 字段套 gorm NamingStrategy 的蛇形转换，
// 见 [snakeColumn]）——修复前白名单收的是 Go 字段名小写（如 DisplayName →
// "displayname"），与 NamingStrategy 产出的真列名 "display_name" 脱节，
// 按别名校验通过后 "displayname" 原文进 ORDER BY ⇒ 列不存在 ⇒ DB 报错 5xx。
// 修复后别名只决定「能不能排」，出 SQL 恒用规范化列名。
//
// 别名集合 = 规范化列名本身 + json tag 名（小写）。Go 字段名小写**不再是**
// 别名（与真列名脱节的正是它；无 tag 字段的字段名小写 ≠ 蛇形真列名时旧行为
// 本就产生 5xx，有此需要的用 gorm column tag 或 json tag 显式声明）。
//
// 嵌入结构体（#175）：gorm 对嵌入类型只**提升内层字段**为列、不为嵌入类型
// 本身建列——白名单同样只收内层字段（gorm.Model 的 ID/CreatedAt 等真实列），
// 嵌入类型名（"model"）绝不作为列入表。嵌入的判定与 gorm 一致：匿名结构体
// （time.Time / 实现 driver.Valuer 的类型除外，gorm 对它们建单列）或显式
// `gorm:"embedded"`；`embeddedPrefix` 前缀拼进内层列名（嵌套嵌入累加前缀）。
//
// 实体不是结构体 / 无可用字段 ⇒ 空集（白名单外的列一律 400，fail-closed）。
func entityColumns[T any]() map[string]string {
	cols := make(map[string]string)
	collectEntityColumnsWithPrefix(reflect.TypeOf((*T)(nil)).Elem(), cols, map[reflect.Type]struct{}{}, "")
	return cols
}

// valuerType 是 driver.Valuer 的反射类型：匿名嵌入实现了 Valuer 的结构体时，
// gorm 不展开内层字段，而是把该字段当**单个**列（经 Value() 取值）——白名单
// 同样只收字段名对应的单列。
var valuerType = reflect.TypeOf((*driver.Valuer)(nil)).Elem()

// collectEntityColumnsWithPrefix 递归收集结构体字段的「别名 → 规范化列名」，
// prefix 是外层嵌入链累积的 embeddedPrefix（gorm 语义：内层列名 = 前缀 +
// 内层列名，column tag 也被加前缀）。
//
// 嵌入判定对齐 gorm schema.ParseField（v1.31.2）：
//   - 显式 `gorm:"embedded"`（具名字段/匿名皆可）⇒ 嵌入，展开内层字段；
//   - 匿名结构体且非 time.Time、未实现 driver.Valuer ⇒ 嵌入，展开内层字段；
//   - 匿名 time.Time / Valuer / 非结构体 ⇒ 不嵌入，gorm 按字段名建**单列**
//     （如匿名 time.Time → 列 "time"），白名单同样只收该单列；
//   - 显式 embedded 的 time.Time（内层全非导出，展开后无列）/ 非结构体
//     （gorm 报 invalid embedded struct）⇒ 无列，跳过。
//
// seen 按类型防环；同一类型以多个不同前缀重复嵌入时只收首个前缀下的列
// （gorm 侧同类型多前缀嵌入会产生同名/重复列，本身不是合法 schema）。
func collectEntityColumnsWithPrefix(t reflect.Type, cols map[string]string, seen map[reflect.Type]struct{}, prefix string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	if _, ok := seen[t]; ok {
		return
	}
	seen[t] = struct{}{}
	timeType := reflect.TypeOf(time.Time{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			// 非导出字段：不是 DB 列，不进白名单。
			continue
		}
		gormTag := f.Tag.Get("gorm")
		if gormSkipField(gormTag) {
			// gorm:"-" / gorm:"-:all"：字段整体忽略（不可读写），不是 DB 列。
			continue
		}
		embeddedByTag := gormHasOption(gormTag, "embedded")
		if f.Anonymous || embeddedByTag {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft != timeType &&
				(embeddedByTag || !reflect.PointerTo(ft).Implements(valuerType)) {
				// 嵌入结构体：gorm 只提升内层字段，类型本身不是列（#175）——
				// 递归收内层并 continue，嵌入类型名不再入表。
				collectEntityColumnsWithPrefix(ft, cols, seen, prefix+gormOptionValue(gormTag, "embeddedPrefix"))
				continue
			}
			if embeddedByTag {
				// 显式 embedded 的 time.Time / 非结构体：gorm 侧无可用列。
				continue
			}
			// 匿名且未声明 embedded 的 time.Time / Valuer / 非结构体：
			// gorm 按字段名建单列 → 落到下方通用单列收集。
		}
		// 规范化列名：gorm `column:` tag 优先；无 tag 套 NamingStrategy 蛇形
		// 转换（与 gorm 建表时的真列名对齐，#165）。
		col := prefix + snakeColumn(f.Name)
		if c := gormColumnTag(gormTag); c != "" {
			col = prefix + c
		}
		addColumn(cols, col, col)
		if tag, ok := f.Tag.Lookup("json"); ok {
			name := strings.Split(tag, ",")[0]
			if name != "" && name != "-" {
				// json 名是别名：放行校验，出 SQL 用规范化列名。
				addColumn(cols, name, col)
			}
		}
	}
}

// addColumn 把「别名 → 规范化列名」按小写口径加入白名单。
// 别名为空或规范化列名为空时不收（fail-closed，不产出空列名的 ORDER BY）。
func addColumn(cols map[string]string, alias, col string) {
	alias = strings.ToLower(alias)
	if alias == "" || col == "" {
		return
	}
	cols[alias] = col
}

// commonInitialisms 是 gorm schema.NamingStrategy 的缩写词表（逐字对齐，
// 顺序也一致——strings.NewReplacer 在同一位置命中多个 pattern 时按参数顺序
// 取第一个）。
var commonInitialisms = []string{"API", "ASCII", "CPU", "CSS", "DNS", "EOF", "GUID", "HTML", "HTTP", "HTTPS", "ID", "IP", "JSON", "LHS", "QPS", "RAM", "RHS", "RPC", "SLA", "SMTP", "SSH", "TLS", "TTL", "UID", "UI", "UUID", "URI", "URL", "UTF8", "VM", "XML", "XSRF", "XSS"}

// initialismsReplacer 把缩写词改写成「首字母大写 + 其余小写」的驼峰形态
// （gorm 用 golang.org/x/text 的 cases.Title，本仓零依赖口径用等价的手工
// 首字母大写——缩写词表全为 ASCII 大写，两者逐词等价）。
var initialismsReplacer = func() *strings.Replacer {
	pairs := make([]string, 0, len(commonInitialisms)*2)
	for _, in := range commonInitialisms {
		pairs = append(pairs, in, strings.ToUpper(in[:1])+strings.ToLower(in[1:]))
	}
	return strings.NewReplacer(pairs...)
}()

// snakeColumn 把 Go 字段名转成 gorm NamingStrategy 的蛇形列名（#165）。
//
// 算法逐字对齐 gorm schema.NamingStrategy.toDBName（v1.31.2，已用真实
// NamingStrategy 对缩写词表全量回归：ID→id、UserID→user_id、DisplayName→
// display_name、HTTPServer→http_server、OAuth2Token→o_auth2_token 等，
// 见 snake_column_test.go）——白名单产出的列名必须与 gorm 建表列名一致，
// 否则「校验通过的列」依然是 DB 里不存在的列（本修复要消灭的正是这个）。
func snakeColumn(name string) string {
	if name == "" {
		return ""
	}
	value := initialismsReplacer.Replace(name)

	var buf strings.Builder
	var lastCase, nextCase, nextNumber bool // upper case == true
	curCase := value[0] >= 'A' && value[0] <= 'Z'

	for i, v := range value[:len(value)-1] {
		nextCase = value[i+1] >= 'A' && value[i+1] <= 'Z'
		nextNumber = value[i+1] >= '0' && value[i+1] <= '9'

		if curCase {
			if lastCase && (nextCase || nextNumber) {
				buf.WriteRune(v + 32)
			} else {
				if i > 0 && value[i-1] != '_' && value[i+1] != '_' {
					buf.WriteByte('_')
				}
				buf.WriteRune(v + 32)
			}
		} else {
			buf.WriteRune(v)
		}

		lastCase = curCase
		curCase = nextCase
	}

	if curCase {
		if !lastCase && len(value) > 1 {
			buf.WriteByte('_')
		}
		buf.WriteByte(value[len(value)-1] + 32)
	} else {
		buf.WriteByte(value[len(value)-1])
	}
	return buf.String()
}

// gormColumnTag 从 gorm tag 中取 column 名（无则空串）。
// 形如 `gorm:"column:created_at;index"` → "created_at"。
func gormColumnTag(tag string) string {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, "column:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// gormHasOption 判断 gorm tag 是否声明了某选项（形如 `gorm:"embedded;embeddedPrefix:au_"`
// 中的 "embedded"）。与 gorm 的 TagSettings 口径一致：分号分隔，选项名不区分大小写，
// 带值/不带值都算声明（gorm 对 EMBEDDED 无论是否带值都视为声明）。
func gormHasOption(tag, option string) bool {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if key, _, found := strings.Cut(part, ":"); found {
			if strings.EqualFold(strings.TrimSpace(key), option) {
				return true
			}
			continue
		}
		if strings.EqualFold(part, option) {
			return true
		}
	}
	return false
}

// gormOptionValue 取 gorm tag 中某选项的值（无则空串）。
// 形如 `gorm:"embedded;embeddedPrefix:author_"` 中 embeddedPrefix → "author_"。
// 值内的 ":" 按 gorm ParseTagSetting 的口径保留（Join(values[1:], ":")）。
func gormOptionValue(tag, option string) string {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		key, rest, found := strings.Cut(part, ":")
		if found && strings.EqualFold(strings.TrimSpace(key), option) {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// gormSkipField 判断 gorm tag 是否整体忽略字段：`gorm:"-"` / `gorm:"-:all"`
// 使字段不可读写（DataType 置空），gorm 不为其建列——白名单同样跳过；
// `gorm:"-:migration"` 只是不参与建表迁移，字段仍是可读写的真实列，不跳过。
// 值口径与 gorm 一致（小写 + 去空白后等于 "-" 或 "all"；裸 "-" 的值即 "-"，
// 见 ParseTagSetting 对无值选项 settings[k]=k 的处理）。
func gormSkipField(tag string) bool {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		key, val, found := strings.Cut(part, ":")
		if !strings.EqualFold(strings.TrimSpace(key), "-") {
			continue
		}
		if !found {
			val = "-"
		}
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "-", "all":
			return true
		}
		return false
	}
	return false
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

// sortAllowlist 返回当前生效的排序列白名单（别名 → 规范化列名）；nil 表示
// 不做白名单校验（全放开，仅标识符格式校验）。见 [WithSortValidation]。
func (h *Handler[T, ID, C, U]) sortAllowlist() map[string]string {
	if !h.sortValidation {
		return nil
	}
	return h.sortable
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
// 读取的参数名由 [WithIDParam] 指定（缺省 "id"；未显式设置时按路由模板推导）。
func (h *Handler[T, ID, C, U]) parseID(c *app.RequestContext) (ID, error) {
	idStr := c.Param(h.idParam)
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
