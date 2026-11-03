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
package crudh
