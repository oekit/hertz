package hertz

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/oekit/concurrency/async"
	"github.com/oekit/core/lifecycle"
	"github.com/oekit/logx"
	"github.com/oekit/pkg/errors"
)

var bootstrapLog = logx.Delegating("hertz-bootstrap")

// errProbeAborted 是「探活被正常关停打断」的哨兵错误（#48）的 hertz 侧本地
// 别名（#208 消费 core/lifecycle.ReadyProbe 单源）：哨兵实现单源自
// lifecycle.ErrProbeAborted（同一下沉哨兵，文案与 errors.Is 判定逐字一致），
// 本地别名仅为既有调用点/测试的名称可读性保留——探活被关停打断（正常终止 ⇒
// 预期路径）与探活失败（启动失败 ⇒ 请求关机）的语义拆分见 lifecycle 包文档。
var errProbeAborted = lifecycle.ErrProbeAborted

// RunServerOption 配置 RunServer 的就绪回调行为。
type RunServerOption func(*runServerConfig)

type runServerConfig struct {
	onReady      func(ctx context.Context)
	probeTimeout time.Duration
	// probeDisabled 由 WithReadyProbeTimeout(负数) 置 true：禁用探活，
	// RunServer 在 GoBackground 启动前同步回调 onReady。
	// 注意语义方向：**不能用「probeEnabled bool」（bool 零值 false）表示启用**——
	// 未设置 WithReadyProbeTimeout 的缺省语义是「探活 + 默认 5s 超时」，
	// 若以 enabled=false 表示禁用，缺省会被误判成禁用（#129）。
	// 「禁用」是彻底禁用：零探活协程、零 dial（即使探活目标可建连也不碰，
	// 见 TestRunServer_ReadyProbeDisabled_NeverDials）、不吞用户设置的超时
	// 值——该分支根本不探活，timeout 字段无关（#146 复审确认语义已闭环）。
	probeDisabled bool
}

// WithOnReady 注册就绪回调：TCP 探活通过（**server 的监听地址**可建连）后执行，
// 典型用途是触发延迟注册（app.Ready()，配合 bootstrap/nacos v0.3.0）。
// 设置本选项即默认启用 TCP 探活；未设置时行为与 v0.5.x 一致（无回调无探活）。
//
// ⚠️ 探活目标是 **server 的单个监听地址**（hertz v0.10.6 的
// server.WithHostPorts(hp string) 只存一个地址到 Options.Addr，
// config.Options 无多地址字段）——不是「全部 HostPort」。Addr 为空但设置了
// server.WithListener 时退回探 listener 地址；两者皆空时退化为
// 「等 engine running 再回调」（见 probeAndReady，#131）。
func WithOnReady(fn func(ctx context.Context)) RunServerOption {
	return func(cfg *runServerConfig) { cfg.onReady = fn }
}

// WithReadyProbeTimeout 配置探活超时（默认 5s；传 0 或未设置同为默认 5s）。
// 设为负数禁用探活（#129/#146）——RunServer 在 GoBackground 启动前**同步**
// 回调 onReady，且不启动探活协程、不向探活目标发任何 dial（探活失败不会
// 触发 RequestShutdown）。不推荐禁用：
// 回调时端口可能尚未监听，hertz OnRun 钩子先于 listenAndServe 执行，
// 无原生就绪信号可用。
func WithReadyProbeTimeout(d time.Duration) RunServerOption {
	return func(cfg *runServerConfig) {
		cfg.probeTimeout = d
		cfg.probeDisabled = d < 0
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
//     监听地址（默认超时 5s，WithReadyProbeTimeout 可调；**传负数禁用探活**——
//     此时在 GoBackground 启动前同步回调 onReady，不启动探活协程，探活失败
//     关机的路径整个不存在），可建连后执行
//     onReady——hertz 的 OnRun 钩子先于 listenAndServe 执行，无法用作就绪
//     信号，故以探活近似真实就绪（TCP 可建连 ≠ 业务健康，业务级健康门请配合
//     hertz/health 检查后再 Ready）。
//  3. h.Run() 返回 error 或 panic（netpoll 传输层在监听失败如端口占用时
//     直接 panic 而非返回 error）时调用 app.RequestShutdown() 触发优雅关机，
//     避免进程无服务地"假活"；探活超时同样触发关机（按启动失败处理）。
//     **探活被正常关停打断则不触发关机**（#48）：关停是预期路径，只记一条
//     Info，不误报 ERROR「ready probe failed」。
//
// # 关闭语义（对照官方 graceful-shutdown 文档）
//
// 信号路径：SIGTERM/SIGINT（SIGHUP 未被忽略时含之）由 bootstrap 的关停序列
// 触发本函数注册的关闭回调，回调内调 h.Shutdown(ctx)，排空上限为
// server.WithExitWaitTime（**默认 5s** — hertz 官方默认;未配时 netpoll 1s ticker
// 检 active conn / go net 立即关 listen）。若另有先行关停路（调用方自行
// Spin/Shutdown）已把 engine 停掉，本回调的 Shutdown 必得 hertz 私有的
// errStatusNotRunning 错误——这是双路关闭的良性竞争而非故障，按**进入回调
// 时**采样的 engine 状态判定（#166：进回调时已非 running 即视为双路良性
// 败者，按成功处理，不做错误文本匹配——hertz 未导出该 sentinel、文案无
// 兼容承诺；#179：必须在调 Shutdown **之前**采样，hertz Shutdown 先翻状态
// 后干活，事后采样 IsRunning() 恒为 false，会把真实关停失败一并吞掉）。
// 首发关停路自己产生的错误（注册中心注销失败、排空超时等真失败）照常
// 上抛，shutdown.Manager 可见。
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
		// #179：**进入回调先采样** engine 状态。hertz Shutdown 的实现顺序是
		// 「入口判状态非 running 即返回 errStatusNotRunning → CAS 翻状态 →
		// 再干活（Registry.Deregister 失败 / transport.Shutdown 非 ctx.Err
		// 的错误均上抛）」——任何从 Shutdown 返回的真错误产生时 status 必已
		// 离开 running。若事后采样 IsRunning()（#166 的做法），判据第二项
		// 恒真 ⇒ 所有真实关停失败（注册中心注销失败=实例泄漏等）都被当成功
		// 吞掉。进回调时 engine 已非 running 才是双路竞争败者（#166）的
		// 真判定：此时本路 Shutdown 必得 errStatusNotRunning，按成功处理；
		// 首发关停路自己产生的错误一律上抛，shutdown.Manager 可见。
		wasRunning := h.IsRunning()
		err := h.Shutdown(ctx)
		if err != nil && !shutdownErrTolerable(wasRunning, err) {
			return err
		}
		return nil
	})
	// WithReadyProbeTimeout(负数) = 显式禁用探活（#129/#132，#146 复审确认）：
	// 在 GoBackground 启动前**同步**回调 onReady（与 WithReadyProbeTimeout 的
	// godoc 承诺一致），且不启动探活协程。禁用探活的典型动机恰是「addr 不可
	// 拨号」场景（自定义 listener / 不可路由主机名）：若仍照跑探活，必然失败
	// 并触发 RequestShutdown——被禁用的探活反而把服务在启动期杀掉。
	if cfg.onReady != nil && cfg.probeDisabled {
		bootstrapLog.Info("hertz ready probe disabled, invoking onReady synchronously before run", "name", name)
		cfg.onReady(app.Context())
	}
	app.GoBackground(name+"-run", func(_ context.Context) {
		defer func() {
			if r := recover(); r != nil {
				bootstrapLog.Error("hertz server start panicked, requesting shutdown",
					"name", name, "panic", r, "stack", string(debug.Stack()))
				app.RequestShutdown()
			}
		}()
		if cfg.onReady != nil && !cfg.probeDisabled {
			// 探活必须与 h.Run() **并发**（#43）：监听端口是在 h.Run() 内部
			// （engine.Run → listenAndServe）才绑定的，先探活后启动必然
			// connection refused ⇒ 探活必超时 ⇒ server 根本不会启动。
			// 探活协程只负责「探活 + 回调 onReady + 探活失败则按启动失败关机」，
			// h.Run() 仍在本 goroutine 内跑（其 panic 由上面的 recover 兜住）。
			probeCtx, cancelProbe := context.WithCancel(app.Context())
			defer cancelProbe()
			async.GoSafe(func() {
				err := probeAndReady(probeCtx, app, name, h, cfg)
				if err == nil {
					return
				}
				if errors.Is(err, errProbeAborted) {
					// 正常关停打断探活（#48）：**预期路径，不是探活失败**。
					// 只记一条 Info，不请求关机——进程正在关停，RequestShutdown
					// 此刻是多余动作（旧实现此处打 ERROR「ready probe failed，
					// requesting shutdown」，把关停误报成启动失败）。
					bootstrapLog.Info("hertz ready probe aborted by shutdown", "name", name)
					return
				}
				bootstrapLog.Error("hertz ready probe failed, requesting shutdown",
					"name", name, logx.ErrKey, err)
				app.RequestShutdown()
			})
		}
		if err := h.Run(); err != nil {
			bootstrapLog.Error("hertz server stopped with error, requesting shutdown",
				"name", name, logx.ErrKey, err)
			app.RequestShutdown()
		}
	})
}

// shutdownErrTolerable 判定双路关闭竞争下 h.Shutdown 的错误是否可容忍
// （#166 / #179）。
//
// 判定变量是**进入关闭回调时采样**的 engine 状态（wasRunning），不是事后
// 状态：hertz v0.10.6 Shutdown 先 CAS 翻状态再干活，任何真错误
// （Registry.Deregister 失败、transport.Shutdown 非 ctx.Err 的错误等）返回
// 时 status 必已离开 running——事后采样 IsRunning() 恒为 false，会把所有
// 真失败判成良性（#179）。
//
//   - err == nil：恒容忍（本来就没失败）；
//   - wasRunning == false：本路是双路关闭竞争的败者——engine 已被先行关停
//     路停掉，本路 Shutdown 必得 hertz 私有的 errStatusNotRunning，属良性
//     竞争而非故障，容忍任意错误（不做错误文本匹配：hertz 未导出该
//     sentinel、文案无兼容承诺，文本匹配会在上游改措辞时把每次信号关机
//     变成假故障）；
//   - wasRunning == true：本路是首发关停路，自己发起的关停产生的错误
//     （注册中心注销失败、排空超时等真失败）一律不容忍，照常上抛。
func shutdownErrTolerable(wasRunning bool, err error) bool {
	return err == nil || !wasRunning
}

// probeTimeoutOrDefault 归一探活超时：零值/缺省 = 默认 5s（#129）。默认值
// 单源自 lifecycle.DefaultProbeTimeout（#208 消费，原本地
// defaultReadyProbeTimeout 常量删除，逐值一致 5s）。负数不会走到这里
// （probeDisabled 时 RunServer 不启动探活，#146），不再存在「负数悄悄换
// 默认值继续探」的回退。
func (cfg *runServerConfig) probeTimeoutOrDefault() time.Duration {
	if cfg.probeTimeout <= 0 {
		return lifecycle.DefaultProbeTimeout
	}
	return cfg.probeTimeout
}

// probeAndReady 探活监听地址，可建连并通过退出复核后执行 onReady。
//
// #208 消费后 dial 循环、超时预算（总超时/单次 dial 500ms/重试 100ms）、
// 关停哨兵与 onReady panic 收编（#152）单源自 core/lifecycle.ReadyProbe，
// hertz 侧只保留领域皮——探活目标解析（probeTarget，#131）、IsRunning
// 就绪门来源（watchEngineRunning）与日志。既有守则不变：
//   - 只在 engine 进入 running 之后才发起探活（#43）：Started 门由
//     watchEngineRunning 把 IsRunning 轮询转成 close；
//   - 探活通过后复核 engine 仍在 running（#120/#131）：Exited 门——端口被
//     **他人**占用时，探活连上的是对方的监听器，而本进程的 server 实际已
//     启动失败（netpoll 监听失败直接 panic），此时不得回调 onReady，否则会
//     把不属于自己的实例注册到注册中心；
//   - **关停优先于超时判定**（#48）：ctx 被取消（正常关停）时 ReadyProbe.Run
//     返回哨兵 errProbeAborted（= lifecycle.ErrProbeAborted），调用方按
//     正常终止处置，不误报启动失败；
//   - 探活目标解析（#131）：优先 Options.Addr（server.WithHostPorts 只存
//     单个地址；server.WithListener 也会把 Addr 设为 ln.Addr()）。Addr 为空
//     但保留了 Listener 时退回探 listener 地址；两者皆空（server 将在
//     h.Run() 内绑定随机端口，地址事先未知）时返回空 addr——ReadyProbe 视为
//     「无拨号目标」，退化为「等 engine running 再回调」，消除「onReady 在
//     server 尚未 serve 时立即执行」的未监听窗口（窗口内注册中心已有实例
//     但端口未监听，健康检查/首轮流量直接失败）。
//
// onReady 以 app 根 ctx 交付（与下沉前 callOnReady(name, cfg.onReady,
// app.Context()) 同口径）：典型用途是延迟注册 app.Ready()，回调的生命周期
// 与 app 一致而非探活协程；其 panic 由 ReadyProbe.Run 转为 error 返回
// （#152 单源），走既有失败链（Error 日志 + RequestShutdown）。
func probeAndReady(ctx context.Context, app lifecycle.App, name string, h *server.Hertz, cfg *runServerConfig) error {
	network, addr := probeTarget(h)
	if addr == "" {
		// 无任何可探地址（Addr 与 Listener 皆空）：无法 TCP dial，退化为
		// 「等 engine running 再回调」（#131），仍受同一超时与关停语义约束。
		bootstrapLog.Info("hertz ready probe has no dial target, waiting for engine running before onReady",
			"name", name)
	}

	started, exited := make(chan struct{}), make(chan struct{})
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	async.GoSafe(func() {
		watchEngineRunning(ctx, stopWatch, h, started, exited)
	})

	probe := lifecycle.NewReadyProbe(lifecycle.WithProbeTimeout(cfg.probeTimeoutOrDefault()))
	err := probe.Run(ctx, lifecycle.ProbeRequest{
		Target:  func(context.Context) (string, string) { return network, addr },
		Started: started,
		Exited:  exited,
		OnReady: func(context.Context) { cfg.onReady(app.Context()) },
	})
	if err == nil {
		bootstrapLog.Info("hertz ready probe passed", "name", name, "addr", addr)
	}
	return err
}

// probeTarget 解析探活目标（#131，hertz 侧保留的领域皮）：优先 Options.Addr
// （server.WithHostPorts 只存单个地址；server.WithListener 也会把 Addr 设为
// ln.Addr()）。Addr 为空但保留了 Listener 时退回探 listener 地址；两者皆空
// 时返回空 addr（ReadyProbe 视为无拨号目标）。Network 为空按 "tcp" 处理
// （与 ReadyProbe.Run 的兜底一致）。
func probeTarget(h *server.Hertz) (network, addr string) {
	opts := h.GetOptions()
	addr = opts.Addr
	if addr == "" && opts.Listener != nil {
		addr = opts.Listener.Addr().String()
	}
	network = opts.Network
	if network == "" {
		network = "tcp"
	}
	return network, addr
}

// watchEngineRunning 把 h.IsRunning() 轮询转成 ReadyProbe 的 Started/Exited
// 就绪门信号（#208 下沉后 hertz 侧保留的「IsRunning 就绪门来源」）：
//   - engine 首次进入 running：close(started)——端口绑定发生在 h.Run() 内部，
//     ReadyProbe 在此之前不发 dial（先探活后启动必然 connection refused，#43）；
//   - started 关闭后 engine 首次离开 running：close(exited)——ReadyProbe 建连
//     成功后复核该信号，端口被他人抢占、本进程 server 已启动失败时不得回调
//     onReady（#120/#131 TOCTOU）。
//
// engine 未被观察到进入 running 就退出时两门都不关：Started 等门超时按启动
// 失败收敛（与下沉前 dial 循环「等 running 到超时」同语义）。轮询间隔与
// 下沉前等 running 的 sleep 一致（10ms）。ctx 取消或 stop 关闭即返回。
func watchEngineRunning(ctx context.Context, stop <-chan struct{}, h *server.Hertz, started, exited chan struct{}) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	runningSeen := false
	for {
		running := h.IsRunning()
		if running && !runningSeen {
			runningSeen = true
			close(started)
		} else if runningSeen && !running {
			close(exited)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}
