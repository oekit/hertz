package crudh

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/gormx/crud"
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

// HandlerOption 是 [NewHandler] 的配置选项。
type HandlerOption[T any, ID comparable, CreateReq, UpdateReq any] func(*Handler[T, ID, CreateReq, UpdateReq])

// WithIDParser 注入路径参数 → ID 的解析函数。
//
// 缺省解析：int 系主键按 strconv.ParseInt（宽度随 ID 类型，与 v0.6 之前
// int64 行为一致）、string 主键直通（含命名 string 类型，#178）；其余
// comparable 类型必须显式注入，缺省解析失败按 400 拒绝（fail-closed，
// 绝不猜测）。
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
