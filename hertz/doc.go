// Package hertz 为 Hertz HTTP 框架提供 ekit 生态集成。
//
// 覆盖以下能力（对称 kitex 包，治理配置热更新不适用 Hertz）：
//   - 日志桥接：将 Hertz 框架内部日志通过 hlog → logx 归一，自动携带 trace_id 和结构化字段
//   - 生命周期管理：RunServer 将 Hertz server 注册到 bootstrap，一行完成 shutdown + 后台 Run；
//     server 启动失败（如端口占用）时自动触发 RequestShutdown 优雅关机，避免进程"假活"
//
// 子包（推荐新代码直接使用子包）：
//   - hertz/health: 健康检查端点（/ping、/health、/buildVersion）+ Redis/DB checker
//   - hertz/metrics: Prometheus /metrics 端点（gzip/OpenMetrics 协商透传）
//   - hertz/sse: Server-Sent Events 流式推送（官方 protocol/sse 的并发安全封装）
//   - OTel tracing 见 observability/tracing/instrumentation/hertzotel（server/client 全量能力）
//   - trace_id 中间件见 middleware/traceid/hmw（FromRequestContext + Middleware）
//
// # 完整生命周期示例
//
//	hlog.SetSilentMode(true) // 静默客户端不规范行为导致的读头错误日志（推荐生产开启）
//	hlog.SetLogger(hertz.NewLogger(logx.NewSlogLogger("my-http")))
//
//	bootstrap.Run(cfg, func(app *bootstrap.App) error {
//	    tracing := hertzotel.NewServerTracing()
//	    h := server.New(
//	        server.WithHostPorts(":8080"),
//	        server.WithExitWaitTime(10*time.Second), // 排空等待上限，默认 5s
//	        tracing.Option,
//	    )
//	    h.Use(tracing.Middleware)
//	    h.Use(mw.DefaultMiddleware(mw.StackConfig{ServiceName: "my-http"})...)
//
//	    // 健康检查 + 指标（在 RunServer 之前注册，确保端点随 server 一起就绪）
//	    health.RegisterHealthRoutes(h, health.WithCheckers(
//	        health.NamedRedisChecker("redis", rdb),
//	        health.NamedDBChecker("mysql", sqlDB),
//	    ))
//	    hmetrics.InitAndRegister("my-http", h)
//
//	    hertz.RunServer(app, "hertz", h)
//	    <-app.Context().Done()
//	    return nil
//	})
//
// # 信号行为（对照官方 graceful-shutdown 文档）
//
// Hertz 内置信号处理（h.Run() 内的 waitSignal）：
//   - v0.9.6+：SIGTERM/SIGINT/SIGHUP 均触发优雅退出（h.Shutdown）
//   - < v0.9.6：SIGTERM 立即退出；SIGINT/SIGHUP 触发优雅退出
//
// ekit bootstrap 同时监听 SIGINT/SIGTERM，与 Hertz 内置处理并发触发。
// RunServer 内部已容忍双路 Shutdown 竞态（hertz 返回 "engine is not running" 视为成功），
// 用户无需额外处理。
//
// 优雅退出流程（Hertz 内部，对照官方文档）：
//  1. engine 状态设为 closed
//  2. 并发触发 OnShutdown 钩子（h.Engine.OnShutdown = append(h.Engine.OnShutdown, fn)）
//  3. 从注册中心注销
//  4. 关闭网络库信号监听
//  5. 进行中请求响应附带 Connection:Close 头
//  6. netpoll：每秒检查直到 active conn = 0；go net：关闭监听
//  7. 等待 ExitWaitTime（默认 5s）到期后退出
//
// # 退出时间控制
//
// server.WithExitWaitTime(d) — 排空等待上限（默认 5s），是官方文档的核心旋钮。
// 有长耗时请求时调大；与 bootstrap 的 ShutdownTimeout（单资源关闭预算）配合：
// bootstrap 关闭回调传入的 ctx 有 ShutdownTimeout 截断，Hertz 内部再以 ExitWaitTime 截断，
// 实际排空上限 = min(ShutdownTimeout, ExitWaitTime)。
// 保持 ExitWaitTime <= ShutdownTimeout 以确保排空不被 bootstrap 兜底截断。
//
// # 与 kitex 包的差异
//
// Hertz 使用中间件链（h.Use）而非 Suite 模式，因此不提供 ClientSuite/ServerSuite。
// 治理配置热更新（retry/rpc_timeout/circuit_breaker/limiter）是 Kitex RPC 框架级概念，
// Hertz 的对应能力由 mw.RateLimitMiddleware 等静态中间件覆盖，无需动态治理。
package hertz
