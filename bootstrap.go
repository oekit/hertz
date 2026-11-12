package hertz

import (
	"context"
	"fmt"
	"net"
	"runtime/debug"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/oekit/concurrency/async"
	"github.com/oekit/core/lifecycle"
	"github.com/oekit/logx"
	"github.com/oekit/pkg/errors"
)

var bootstrapLog = logx.Delegating("hertz-bootstrap")

// defaultReadyProbeTimeout 是 WithOnReady 启用后的默认探活超时。
const defaultReadyProbeTimeout = 5 * time.Second

// errProbeAborted 是「探活被正常关停打断」的哨兵错误（#48）。
//
// 语义拆分：**探活失败**（启动失败 ⇒ 按失败处理、请求关机）与**探活被关停打断**
// （正常终止 ⇒ 预期路径）是两件事。关停不是失败——调用方用 errors.Is 判定后
// 只记一条 Info，且不再调 app.RequestShutdown()（进程本就在关停，再请求一次
// 是多余动作；旧实现把关停记成 ERROR「ready probe failed」，值班的人会误以为
// 服务启动失败）。
//
// 该错误只由 probeAndReady 在 ctx 被取消时返回；探活超时等真正的启动失败
// 仍返回普通 error。
var errProbeAborted = errors.New("ready probe aborted: app shutting down")

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
					logx.ErrKey, err)
				app.RequestShutdown()
			})
		}
		if err := h.Run(); err != nil {
			bootstrapLog.Error("hertz server stopped with error, requesting shutdown",
				logx.ErrKey, err)
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

// probeAndReady 循环 TCP dial 探活监听地址，可建连后执行 onReady。
//
// 四条守则（#43 / #48 / #131）：
//   - 只在 engine 进入 running 之后才发起探活：端口绑定前探活必然 refused，
//     白白重试到超时；
//   - 探活通过后复核 engine 仍在 running：端口被**他人**占用时，探活连上的
//     是对方的监听器，而本进程的 server 实际已启动失败（netpoll 监听失败
//     直接 panic），此时不得回调 onReady —— 否则会把不属于自己的实例
//     注册到注册中心。
//   - **关停优先于超时判定**（#48）：ctx 被取消（正常关停）时返回哨兵
//     errProbeAborted，而不是「探活超时」——两者在调用方是不同语义（正常终止
//     vs 启动失败）。每次判定超时前先看 ctx.Err()，避免「关停与超时同时发生」
//     时把关停误报成启动失败。
//   - 探活目标解析（#131）：优先 Options.Addr（server.WithHostPorts 只存
//     单个地址；server.WithListener 也会把 Addr 设为 ln.Addr()）。Addr 为空
//     但保留了 Listener 时退回探 listener 地址；两者皆空（server 将在
//     h.Run() 内绑定随机端口，地址事先未知）时无法 dial——至少等到 engine
//     进入 running 再回调，消除「onReady 在 server 尚未 serve 时立即执行」
//     的未监听窗口（窗口内注册中心已有实例但端口未监听，健康检查/首轮流量
//     直接失败）。
//
// probeTimeoutOrDefault 归一探活超时：零值/缺省 = 默认 5s（#129）。
// 负数不会走到这里（probeDisabled 时 RunServer 不启动探活，#146），
// 不再存在「负数悄悄换默认值继续探」的回退。
func (cfg *runServerConfig) probeTimeoutOrDefault() time.Duration {
	if cfg.probeTimeout <= 0 {
		return defaultReadyProbeTimeout
	}
	return cfg.probeTimeout
}

func probeAndReady(ctx context.Context, app lifecycle.App, name string, h *server.Hertz, cfg *runServerConfig) error {
	timeout := cfg.probeTimeoutOrDefault()

	opts := h.GetOptions()
	addr := opts.Addr
	if addr == "" && opts.Listener != nil {
		addr = opts.Listener.Addr().String()
	}
	network := opts.Network
	if network == "" {
		network = "tcp"
	}

	if addr == "" {
		// 无任何可探地址（Addr 与 Listener 皆空）：无法 TCP dial，退化为
		// 「等 engine running 再回调」（#131），仍受同一超时与关停语义约束。
		bootstrapLog.Info("hertz ready probe has no dial target, waiting for engine running before onReady",
			"name", name)
		deadline := time.Now().Add(timeout)
		for !h.IsRunning() {
			if ctx.Err() != nil {
				return errProbeAborted
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("ready probe timeout after %s: server not running and no dial target", timeout)
			}
			select {
			case <-ctx.Done():
				return errProbeAborted
			case <-time.After(10 * time.Millisecond):
			}
		}
		return callOnReady(name, cfg.onReady, app.Context())
	}

	deadline := time.Now().Add(timeout)
	for {
		if !h.IsRunning() {
			// engine 尚未 running（还没走到 listenAndServe）：等它起来再探。
			if ctx.Err() != nil {
				return errProbeAborted
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("ready probe timeout after %s, addr=%s: server not running", timeout, addr)
			}
			select {
			case <-ctx.Done():
				return errProbeAborted
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		d := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := d.DialContext(ctx, network, addr)
		if err == nil {
			_ = conn.Close()
			if !h.IsRunning() {
				return fmt.Errorf("ready probe connected but server is not running, addr=%s", addr)
			}
			bootstrapLog.Info("hertz ready probe passed", "name", name, "addr", addr)
			return callOnReady(name, cfg.onReady, app.Context())
		}
		// 关停优先于超时：ctx 已取消 ⇒ 探活是被正常关停打断，不是启动失败。
		if ctx.Err() != nil {
			return errProbeAborted
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ready probe timeout after %s, addr=%s: %w", timeout, addr, err)
		}
		select {
		case <-ctx.Done():
			return errProbeAborted
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// callOnReady 执行就绪回调并把 panic 转为 error 返回（#152）。
//
// onReady 是就绪/注册动作（典型用途：延迟 Nacos 注册 app.Ready()）——它
// panic 意味着就绪路径已炸。修复前探活路径的两处 onReady 调用裸奔在
// async.GoSafe 里：panic 沿 probeAndReady 展开，外层闭包的 err 处置
// （探活失败 ⇒ Error + RequestShutdown）整段被跳过，GoSafe recover 后
// 仅记一条日志 ⇒ server 以「探活已过、回调丢失、永不注册」的半启动形态
// 继续 serve——恰是本模块在别处按致命处置的「进程假活」形态。
// 探活协程内按探活失败语义收敛：panic → error，走既有失败链
// （Error 日志 + RequestShutdown）。与 probeDisabled 路径同一 panic 政策：
// onReady panic = 启动失败，绝不留半启动 server（禁用路径在 GoBackground
// 之前同步回调，panic 直接传播给 RunServer 调用方，server 尚未启动）。
func callOnReady(name string, fn func(ctx context.Context), ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("hertz %s onReady panicked: %v", name, r)
		}
	}()
	fn(ctx)
	return nil
}
