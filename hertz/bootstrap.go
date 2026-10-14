package hertz

import (
	"context"
	"runtime/debug"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/yi-nology/ekit-core/lifecycle"
	"github.com/yi-nology/ekit-observability/observability/logx"
)

var bootstrapLog = logx.Delegating("hertz-bootstrap")

// errEngineNotRunning 是 Hertz [server.Hertz.Shutdown] 在 engine 状态非 running
// （已优雅退出或从未启动）时返回的错误文本（hertz route/engine.go 的
// errStatusNotRunning = errs.NewPrivate("engine is not running")，hertz 未导出
// 该 sentinel，只能按文本识别；bootstrap_test.go 有用例钉住该文案）。
const errEngineNotRunning = "engine is not running"

// RunServer 将 Hertz Server 注册到 ekit 生命周期管理（bootstrap.App 满足
// lifecycle.App 接口；transport 适配层只依赖 lifecycle 契约，不依赖 bootstrap）。
//
// 该函数会：
//  1. 通过 app.AddResource 注册 h.Shutdown 作为关闭回调（优雅关闭时自动调用）。
//  2. 通过 app.GoBackground 在后台启动 h.Run()（阻塞直到 server 退出）。
//  3. h.Run() 返回 error 或 panic（netpoll 传输层在监听失败如端口占用时
//     直接 panic 而非返回 error）时调用 app.RequestShutdown() 触发优雅关机，
//     避免进程无服务地"假活"。
//
// # 关闭语义（对照官方 graceful-shutdown 文档）
//
// 信号路径：SIGTERM/SIGINT（SIGHUP 未被忽略时含之）由 h.Run() 内置 waitSignal
// 处理，直接触发 h.Shutdown(context.Background())，排空上限为
// server.WithExitWaitTime（**默认 5s** — hertz 官方默认;未配时 netpoll 1s ticker
// 检 active conn / go net 立即关 listen）。同一信号也会让 bootstrap 走本函数注册的
// 关闭回调，第二次 Shutdown 因 engine 状态已非 running 返回
// "engine is not running"——这是双路关闭的良性竞争而非故障，此处按成功处理，
// 避免每次信号关机都产生假错误日志。
//
// 程序化路径（userFn 返回 / RequestShutdown）：走本函数注册的关闭回调，
// ctx 为 bootstrap 单资源预算（shutdown.New 的 defaultTimeout）；hertz 内部
// 再以 ExitWaitTime 截断，实际排空上限 = min(两者)。需要更长排空时间时调大
// server.WithExitWaitTime，并保持其小于 bootstrap 单资源预算。
//
// ⚠️ **推荐显式设置 server.WithExitWaitTime**(尤其长耗时 RPC):保持其小于
// bootstrap 单资源关闭预算(shutdown.New 的 defaultTimeout),避免排空被
// bootstrap 兜底截断。hertz Shutdown 接受传入 ctx 但内部还会以 ExitWaitTime
// 截断,二者取 min。
//
// 使用方式（在 bootstrap.Run 的 userFn 内，对称 kitex.RunServer）：
//
//	bootstrap.Run(cfg, func(app *bootstrap.App) error {
//	    h := server.New(server.WithHostPorts(":8080"))
//	    h.Use(mw.DefaultMiddleware(mw.StackConfig{ServiceName: "my-svc"})...)
//	    hertz.RunServer(app, "hertz", h)
//	    <-app.Context().Done() // 阻塞直到收到 SIGINT/SIGTERM
//	    return nil
//	})
//
// ⚠️ userFn 返回 = 主动关机。若 RunServer 后直接 return nil，
// bootstrap.Run 会立即执行 shutdown 序列，服务器启动即退出。
func RunServer(app lifecycle.App, name string, h *server.Hertz) {
	app.AddResource(name, func(ctx context.Context) error {
		err := h.Shutdown(ctx)
		if err != nil && err.Error() != errEngineNotRunning {
			return err
		}
		return nil
	})
	app.GoBackground(name+"-run", func(_ context.Context) {
		defer func() {
			if r := recover(); r != nil {
				bootstrapLog.Error("hertz server start panicked, requesting shutdown",
					"name", name, "panic", r, "stack", string(debug.Stack()))
				app.RequestShutdown()
			}
		}()
		if err := h.Run(); err != nil {
			bootstrapLog.Error("hertz server stopped with error, requesting shutdown",
				"name", name, "err", err)
			app.RequestShutdown()
		}
	})
}
