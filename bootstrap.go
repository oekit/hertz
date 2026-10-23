package hertz

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/oekit/concurrency/async"
	"github.com/oekit/core/lifecycle"
	"github.com/oekit/logx"
)

var bootstrapLog = logx.Delegating("hertz-bootstrap")

// errEngineNotRunning 是 Hertz [server.Hertz.Shutdown] 在 engine 状态非 running
// （已优雅退出或从未启动）时返回的错误文本（hertz route/engine.go 的
// errStatusNotRunning = errs.NewPrivate("engine is not running")，hertz 未导出
// 该 sentinel，只能按文本识别；bootstrap_test.go 有用例钉住该文案）。
const errEngineNotRunning = "engine is not running"

// defaultReadyProbeTimeout 是 WithOnReady 启用后的默认探活超时。
const defaultReadyProbeTimeout = 5 * time.Second

// RunServerOption 配置 RunServer 的就绪回调行为。
type RunServerOption func(*runServerConfig)

type runServerConfig struct {
	onReady      func(ctx context.Context)
	probeTimeout time.Duration
	probeEnabled bool
}

// WithOnReady 注册就绪回调：TCP 探活通过（全部 HostPort 可建连）后执行，
// 典型用途是触发延迟注册（app.Ready()，配合 bootstrap/nacos v0.3.0）。
// 设置本选项即默认启用 TCP 探活；未设置时行为与 v0.5.x 一致（无回调无探活）。
func WithOnReady(fn func(ctx context.Context)) RunServerOption {
	return func(cfg *runServerConfig) { cfg.onReady = fn }
}

// WithReadyProbeTimeout 配置探活超时（默认 5s）。设为负数禁用探活——
// 直接在 GoBackground 启动前同步回调（不推荐：回调时端口可能尚未监听，
// hertz OnRun 钩子先于 listenAndServe 执行，无原生就绪信号可用）。
func WithReadyProbeTimeout(d time.Duration) RunServerOption {
	return func(cfg *runServerConfig) {
		cfg.probeTimeout = d
		cfg.probeEnabled = d >= 0
	}
}

// RunServer 将 Hertz Server 注册到 ekit 生命周期管理（bootstrap.App 满足
// lifecycle.App 接口；transport 适配层只依赖 lifecycle 契约，不依赖 bootstrap）。
//
// 该函数会：
//  1. 通过 app.AddResource 注册 h.Shutdown 作为关闭回调（优雅关闭时自动调用）。
//  2. 通过 app.GoBackground 在后台启动 h.Run()（阻塞直到 server 退出）。
//     设置 WithOnReady 时，**探活与 h.Run() 并发**（#43）：监听端口是在
//     h.Run() 内部（engine.Run → listenAndServe）才绑定的，串行先探活必然
//     connection refused、探活必超时、server 根本不会启动。探活循环 TCP dial
//     监听地址（默认超时 5s，WithReadyProbeTimeout 可调），可建连后执行
//     onReady——hertz 的 OnRun 钩子先于 listenAndServe 执行，无法用作就绪
//     信号，故以探活近似真实就绪（TCP 可建连 ≠ 业务健康，业务级健康门请配合
//     hertz/health 检查后再 Ready）。
//  3. h.Run() 返回 error 或 panic（netpoll 传输层在监听失败如端口占用时
//     直接 panic 而非返回 error）时调用 app.RequestShutdown() 触发优雅关机，
//     避免进程无服务地"假活"；探活超时同样触发关机（按启动失败处理）。
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
//	b, _ := bootstrap.New(bootstrap.WithServiceName("svc")) // + nacos/obs WithXxx 按需注入
//	b.Run(func(app *bootstrap.App) error {
//	    h := server.New(server.WithHostPorts(":8080"))
//	    h.Use(mw.DefaultMiddleware(mw.StackConfig{})...)
//	    hertz.RunServer(app, "hertz", h,
//	        hertz.WithOnReady(func(ctx context.Context) { app.Ready() }))
//	    <-app.Context().Done() // 阻塞直到收到 SIGINT/SIGTERM
//	    return nil
//	})
//
// ⚠️ userFn 返回 = 主动关机。若 RunServer 后直接 return nil，
// bootstrap.Run 会立即执行 shutdown 序列，服务器启动即退出。
func RunServer(app lifecycle.App, name string, h *server.Hertz, opts ...RunServerOption) {
	cfg := &runServerConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

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
		if cfg.onReady != nil {
			// 探活必须与 h.Run() **并发**（#43）：监听端口是在 h.Run() 内部
			// （engine.Run → listenAndServe）才绑定的，先探活后启动必然
			// connection refused ⇒ 探活必超时 ⇒ server 根本不会启动。
			// 探活协程只负责「探活 + 回调 onReady + 探活失败则按启动失败关机」，
			// h.Run() 仍在本 goroutine 内跑（其 panic 由上面的 recover 兜住）。
			probeCtx, cancelProbe := context.WithCancel(app.Context())
			defer cancelProbe()
			async.GoSafe(func() {
				if err := probeAndReady(probeCtx, app, name, h, cfg); err != nil {
					bootstrapLog.Error("hertz ready probe failed, requesting shutdown",
						"name", name, "err", err)
					app.RequestShutdown()
				}
			})
		}
		if err := h.Run(); err != nil {
			bootstrapLog.Error("hertz server stopped with error, requesting shutdown",
				"name", name, "err", err)
			app.RequestShutdown()
		}
	})
}

// probeAndReady 循环 TCP dial 探活监听地址，可建连后执行 onReady。
//
// 两条守则（#43）：
//   - 只在 engine 进入 running 之后才发起探活：端口绑定前探活必然 refused，
//     白白重试到超时；
//   - 探活通过后复核 engine 仍在 running：端口被**他人**占用时，探活连上的
//     是对方的监听器，而本进程的 server 实际已启动失败（netpoll 监听失败
//     直接 panic），此时不得回调 onReady —— 否则会把不属于自己的实例
//     注册到注册中心。
func probeAndReady(ctx context.Context, app lifecycle.App, name string, h *server.Hertz, cfg *runServerConfig) error {
	timeout := cfg.probeTimeout
	if !cfg.probeEnabled {
		timeout = defaultReadyProbeTimeout
	}
	if timeout == 0 {
		timeout = defaultReadyProbeTimeout
	}

	addr := h.GetOptions().Addr
	if addr == "" {
		// 无监听地址可探（测试或自定义 listener）：直接回调
		cfg.onReady(app.Context())
		return nil
	}

	deadline := time.Now().Add(timeout)
	for {
		if !h.IsRunning() {
			// engine 尚未 running（还没走到 listenAndServe）：等它起来再探。
			if time.Now().After(deadline) {
				return fmt.Errorf("ready probe timeout after %s, addr=%s: server not running", timeout, addr)
			}
			select {
			case <-ctx.Done():
				return errors.New("ready probe aborted: app shutting down")
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		d := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			if !h.IsRunning() {
				return fmt.Errorf("ready probe connected but server is not running, addr=%s", addr)
			}
			bootstrapLog.Info("hertz ready probe passed", "name", name, "addr", addr)
			cfg.onReady(app.Context())
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ready probe timeout after %s, addr=%s: %w", timeout, addr, err)
		}
		select {
		case <-ctx.Done():
			return errors.New("ready probe aborted: app shutting down")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
