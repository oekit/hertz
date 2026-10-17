// Package crudh 提供基于 Hertz 的通用 CRUD HTTP Handler。
//
// 将 HTTP Handler 从 data/crud 数据层分离，避免数据层反向依赖 HTTP 中间件层。
// Handler 依赖 crud.Service 接口和 middleware 包的响应函数。
//
// 用法:
//
//	repo := gormrepo.NewGormRepository[Project](db)
//	svc := crud.NewBaseService[Project, CreateReq, UpdateReq](repo)
//	h := crudh.NewHandler(svc)
//	h.RegisterRoutes(group, "/projects")
package crudh
