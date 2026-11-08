// Package crudh 提供基于 Hertz 的通用 CRUD HTTP Handler。
//
// 将 HTTP Handler 从 data/crud 数据层分离，避免数据层反向依赖 HTTP 中间件层。
// Handler 依赖 crud.Service 接口和 middleware 包的响应函数。
//
// 用法:
//
//	// ID 为实体主键类型（int64 自增 / string 文本主键等）
//	repo := gormrepo.NewGormRepository[Project, int64](db)
//	svc := crud.NewBaseService[Project, int64, CreateReq, UpdateReq](repo)
//	h := crudh.NewHandler(svc)
//	h.RegisterRoutes(group, "/projects")
//
// 路径参数经缺省解析（int 系 ParseInt / string 直通）或 WithIDParser
// 注入的解析函数转为主键类型。
//
// 对外形态三件套可用 Option 注入（缺省行为与未配置时逐字一致）：
//
//   - WithResponseWriter：成功响应写出钩子，entity/PageResponse → 消费方信封
//     DTO（如 IDResp{ID} / ListXxxResp{List} / EmptyResp{Success}）；
//   - WithErrMapper：错误映射钩子，crud 错误 → 消费方码表（如 404→40401），
//     渲染仍统一走 mw.Fail；
//   - WithRouteTemplate：路由布局模板，子资源/动作端点与标准 CRUD 并存，
//     可省略个别端点（见 DefaultRoutes）。
//
// v0.9.0 的 List/排序缺省口径（BREAKING，均带保留旧行为的开关）：
//
//   - 分页有界 + 归一：缺省请求也带 LIMIT（page=1、page_size=20、上限 500，
//     与 pkg/pagination 口径一致），响应回显的 page/page_size 恒等于 SQL
//     实际 LIMIT（此前：无分页参数 ⇒ 无 LIMIT ⇒ 全表扫描 + 全量序列化；
//     page_size 超限时 SQL 封顶 500 而响应回显未归一的原值，前端算出的页数
//     与实收数据矛盾）。
//     迁移位：WithPageNormalization(false) 回到 v0.8.14 逐字口径；
//     WithListParams(false) 的既有契约不变（忽略参数、恒全量、回显 0）。
//     可配置缺省/上限：WithDefaultPageSize(n) / WithPageSizeLimit(n)。
//   - 排序列白名单：缺省按实体类型 T 的字段反射生成，候选列名与 DB 真实
//     列名对齐（gorm column tag 或 NamingStrategy 蛇形转换，json tag 名作为
//     别名放行），出 SQL 恒用规范化列名而非用户原文（#165）⇒ 白名单外的列
//     返回 400 "invalid sort parameter"（此前 v0.8.14：任意标识符列都进
//     ORDER BY，列不存在时 DB 报错 ⇒ 5xx；v0.9.0~v0.10.6 的白名单收 Go
//     字段名小写，无 column tag 字段的蛇形真列名与之脱节，按 json 名排序
//     依旧 5xx）。
//     迁移位：WithSortValidation(false) 回到 v0.8.14 的「任意标识符列可排序」。
//
// 路径参数名：缺省 "id" 不变；未显式设置时按路由模板首个 ":param" 推导
// （推导不出回退 "id"），显式设置用 WithIDParam(name)。
//
// Op 语义扩展点（issue #101）：三处写死的对外语义改为可注入，缺省逐字不变：
//
//   - WithQueryBinder / WithListParams：List 查询参数绑定可替换 / 可关
//     （关掉后忽略 page/page_size/sort，恒全量返回，对齐存量 IDL 契约）；
//   - WithIDParseErrorHandler：路径参数解析失败的出口可定制（IDParseErrorPassthrough
//     让解析器自带错误即出口，映射交 WithErrMapper）；
//   - WithDeleteFoundPolicy / WithDeleteCascade：Delete 的 found 口径可配
//     （missing→success / →not-found）+ 级联钩子（跨实体级联删除）。
package crudh
