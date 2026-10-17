package hertz

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app/server"
)

// AddShutdownHook 注册 Hertz 优雅退出钩子函数,等价于官方教程推荐的
// h.Engine.OnShutdown = append(...) 写法,但放在 ekit hertz 包内提供
// helper,避免用户在 bootstrap.Run userFn 中手拿 engine。
//
// hertz 官方 OnShutdown 签名 func(ctx context.Context) 不接受 error,
// 本 helper 接受 func(ctx) error 并把 fn 返回的 error 通过 bootstrapLog
// 记录为 Warn 日志(hertz 调用栈会忽略 error,日志是唯一能见的位置)。
//
// fn 在 RunServer 调用前后都可以注册(只是 append 切片,无副作用),
// 多个 hook 按 append 顺序执行(官方文档明确)。
//
// ⚠️ 与 RunServer 的关系:
//
//	ekit hertz.RunServer 内部用 lifecycle.App.AddResource 注册 h.Shutdown,
//	OnShutdown hook 在 Shutdown 流程早期被调用(在 deregister 之前),
//	适合做"关停前刷数据/通知外部"类工作。
//	如果 hook 是"释放外部资源"(如关 DB),推荐直接用 lifecycle.App.AddResource
//	+ 显式 closer,语义更清晰、错误能上报 shutdown.Manager。
//
// 官方依据:https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/hooks/
//
//	"h.Engine.OnShutdown = append(h.Engine.OnShutdown, shutDownFunc)"
//
// 用法:
//
//	h := server.New(...)
//	hertz.AddShutdownHook(h, func(ctx context.Context) error {
//	    // 关闭前的最后操作
//	    return flushLastBatchToRemote(ctx)
//	})
//	hertz.RunServer(app, "my-svc", h)
func AddShutdownHook(h *server.Hertz, fn func(ctx context.Context) error) {
	h.OnShutdown = append(h.OnShutdown, func(ctx context.Context) {
		if err := fn(ctx); err != nil {
			bootstrapLog.Warn("shutdown hook returned error",
				"err", err.Error())
		}
	})
}
