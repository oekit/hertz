package hertz

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/app/server/registry"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	standard "github.com/cloudwego/hertz/pkg/network/standard"
	"github.com/oekit/bootstrap"
	"github.com/oekit/concurrency/async"
	"github.com/oekit/core/lifecycle"
	"github.com/oekit/logx"
)

// TestWithHostPorts_SingleAddress 钉住并登记：hertz v0.10.6 的
// server.WithHostPorts 只存**一个**地址（config.Options.Addr 单字段，
// 无多地址支持）⇒ 探活目标就是这一个地址（不是"全部 HostPort"）。
// bootstrap.go 的相关注释已据此订正。
func TestWithHostPorts_SingleAddress(t *testing.T) {
	h := server.New(
		server.WithHostPorts("127.0.0.1:1111"),
		server.WithHostPorts("127.0.0.1:2222"),
	)
	if got := h.GetOptions().Addr; got != "127.0.0.1:2222" {
		t.Errorf("addr = %q, want the last WithHostPorts value (single address only)", got)
	}
}

func TestRunServer_RegistersShutdownHook(t *testing.T) {
	mgr := lifecycle.New(100 * time.Millisecond)
	app := bootstrap.NewApp(mgr)

	h := server.New(server.WithHostPorts(":0"),
		server.WithExitWaitTime(100*time.Millisecond),
	)
	assert.NotNil(t, h)

	// Should not panic
	RunServer(app, "test-hertz", h)
}

func TestRunServer_MultipleServers(t *testing.T) {
	mgr := lifecycle.New(100 * time.Millisecond)
	app := bootstrap.NewApp(mgr)

	h1 := server.New(server.WithHostPorts(":0"),
		server.WithExitWaitTime(100*time.Millisecond),
	)
	h2 := server.New(server.WithHostPorts(":0"),
		server.WithExitWaitTime(100*time.Millisecond),
	)

	// Should not panic with multiple servers
	RunServer(app, "hertz-1", h1)
	RunServer(app, "hertz-2", h2)
}

func TestRunServer_BootstrapRegistersResource(t *testing.T) {
	// Verify RunServer registers resource without starting a real server.
	// Full integration with h.Run() is tested in examples, not unit tests,
	// because h.Run() blocks until Shutdown is called.
	mgr := lifecycle.New(100 * time.Millisecond)
	app := bootstrap.NewApp(mgr)

	h := server.New(server.WithHostPorts(":0"),
		server.WithExitWaitTime(100*time.Millisecond),
	)
	RunServer(app, "test-hertz", h)

	// Verify resource was registered (no panic, no error)
	assert.NotNil(t, app)
}

func TestRunServer_UserFnErrorPropagates(t *testing.T) {
	// Test that userFn errors propagate without starting a real server.
	// Use a simple scenario: userFn returns error immediately.
	b, _ := bootstrap.New(
		bootstrap.WithServiceName("hertz-err-test"),
		bootstrap.WithShutdownTimeout(500*time.Millisecond),
	)

	err := b.Run(func(app *bootstrap.App) error {
		return errors.New("user fn failed")
	})
	if err == nil {
		t.Fatal("expected error from userFn")
	}
}

// fakeApp 是 lifecycle.App 的最小测试实现，记录 RequestShutdown 调用与注册的资源。
type fakeApp struct {
	lifecycle.App
	ctx            context.Context
	cancel         context.CancelFunc
	requestedCh    chan struct{}
	requestedOnce  sync.Once
	backgroundFunc func(ctx context.Context)
	closers        map[string]func(ctx context.Context) error
}

func newFakeApp() *fakeApp {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeApp{
		ctx:         ctx,
		cancel:      cancel,
		requestedCh: make(chan struct{}),
		closers:     make(map[string]func(ctx context.Context) error),
	}
}

func (f *fakeApp) Context() context.Context { return f.ctx }

func (f *fakeApp) AddResource(name string, closer func(context.Context) error) {
	f.closers[name] = closer
}

func (f *fakeApp) GoBackground(_ string, fn func(ctx context.Context)) {
	f.backgroundFunc = fn
	var wg sync.WaitGroup
	wg.Add(1)
	async.GoSafe(func() {
		defer wg.Done()
		fn(f.ctx)
	})
}

func (f *fakeApp) RequestShutdown() {
	f.requestedOnce.Do(func() { close(f.requestedCh) })
}

// TestRunServer_ToleratesEngineNotRunning 验证关闭回调在 engine 未运行
// （server 从未启动，或已被 h.Run() 内置信号路径优雅关闭——信号关机时
// bootstrap 与 hertz 双路并发 Shutdown，后到者得到 hertz 私有的
// errStatusNotRunning 错误）时按成功处理，不向 lifecycle.Manager 上报假错误。
// #166：容忍判定按 engine 状态（IsRunning）而非错误文本——文本是 hertz
// 私有 sentinel 的措辞，无兼容承诺，上游改文案即每次信号关机假报错。
func TestRunServer_ToleratesEngineNotRunning(t *testing.T) {
	fa := newFakeApp()

	// server 从未 Run：engine 状态非 running，h.Shutdown 返回 errStatusNotRunning。
	h := server.New(server.WithHostPorts(":0"))
	RunServer(fa, "idle-hertz", h)

	closer, ok := fa.closers["idle-hertz"]
	if !ok {
		t.Fatal("RunServer did not register close resource")
	}
	if err := closer(context.Background()); err != nil {
		t.Fatalf("close resource should tolerate engine-not-running, got: %v", err)
	}
}

// TestShutdownErrTolerable_StateNotText 是 #166 的核心回归：容忍判定按
// **进入关闭回调时采样**的 engine 状态而非 "engine is not running" 文案。
//   - 进回调时已非 running（wasRunning=false）：任何错误（含措辞随机的
//     错误）都容忍——双路关闭竞争的败者；
//   - 进回调时仍 running（wasRunning=true）：任何错误都不容忍——首发关停
//     路自己产生的真失败必须上抛；
//   - err == nil：恒容忍。
//
// #179：判定变量必须是**先于 Shutdown 采样**的状态。hertz v0.10.6 Shutdown
// 先 CAS 翻状态再干活（Deregister 失败等真错误返回时 status 必已离开
// running），事后采样 IsRunning() 恒为 false——旧签名 shutdownErrTolerable
// (h, err) 在 Shutdown 之后取 h.IsRunning()，判据第二项恒真，把所有真失败
// 当成功吞掉。
func TestShutdownErrTolerable_StateNotText(t *testing.T) {
	// 双路竞争败者（进回调时 engine 已停）：任意错误容忍。
	if !shutdownErrTolerable(false, errors.New("engine has stopped dancing")) {
		t.Error("race loser (not running at entry): benign race error must be tolerated regardless of text")
	}

	// 首发关停路（进回调时 engine 仍 running）：真失败必须上抛。
	if shutdownErrTolerable(true, errors.New("nacos deregister failed: connection refused")) {
		t.Error("first shutdown initiator: real failure (deregister/drain) must propagate")
	}

	// 无错误：恒容忍，与状态无关。
	if !shutdownErrTolerable(true, nil) || !shutdownErrTolerable(false, nil) {
		t.Error("nil error must always be tolerated")
	}
}

// failDeregRegistry 是 Deregister 必败的注册中心（#179 回归场景：关停期
// 注销失败=实例泄漏，必须对 shutdown.Manager 可见，而不是被当成功吞掉）。
type failDeregRegistry struct{}

func (f *failDeregRegistry) Register(info *registry.Info) error { return nil }

func (f *failDeregRegistry) Deregister(info *registry.Info) error {
	return errors.New("nacos deregister failed: connection refused")
}

// TestRunServer_ShutdownRealErrorPropagates 是 #179 的核心回归：engine 仍
// running 时首发关停（进回调采样 wasRunning=true），挂 Deregister 必败的
// Registry，h.Shutdown 返回真错误——关闭回调必须把它上抛给 shutdown.Manager。
// #166 修复后（#179 修复前）的实现事后采样 IsRunning()：真错误返回时刻
// status 必已离开 running（hertz Shutdown 先 CAS 翻状态后干活），判据恒真，
// 该真失败被当成功吞掉、closer 返回 nil。
func TestRunServer_ShutdownRealErrorPropagates(t *testing.T) {
	fa := newFakeApp()

	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithExitWaitTime(100*time.Millisecond),
		server.WithRegistry(&failDeregRegistry{}, &registry.Info{ServiceName: "dereg-fail-svc"}),
	)
	RunServer(fa, "dereg-fail-hertz", h)
	closer, ok := fa.closers["dereg-fail-hertz"]
	if !ok {
		t.Fatal("RunServer did not register close resource")
	}

	// RunServer 的 GoBackground 已在后台启动 engine：等它进入 running，
	// 确保 closer 进场时 engine 仍 running（首发关停路）
	deadline := time.Now().Add(5 * time.Second)
	for !h.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("live engine never reached running state")
		}
		time.Sleep(2 * time.Millisecond)
	}

	err := closer(context.Background())
	if err == nil {
		t.Fatal("real shutdown failure (registry deregister failed) must propagate to shutdown.Manager, got nil")
	}
	if !strings.Contains(err.Error(), "deregister failed") {
		t.Fatalf("propagated error should be the registry failure, got: %v", err)
	}
}

// TestRunServer_RaceLoserTolerated 是 #166 良性竞争的**可达模型**回归：
// engine 真跑起来后被先行关停路停掉，bootstrap 关闭回调作为败者进场——
// 进回调时已非 running（wasRunning=false），Shutdown 必得 hertz 私有的
// errStatusNotRunning，必须按成功处理返回 nil。旧测试给 live engine 喂
// 合成错误验证的是不可达状态（running 时 Shutdown 返回错误在 hertz 实现
// 里不存在）；本测试走真实双路时序。
func TestRunServer_RaceLoserTolerated(t *testing.T) {
	fa := newFakeApp()

	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithExitWaitTime(100*time.Millisecond),
	)
	RunServer(fa, "race-loser-hertz", h)
	closer, ok := fa.closers["race-loser-hertz"]
	if !ok {
		t.Fatal("RunServer did not register close resource")
	}

	// RunServer 的 GoBackground 已在后台启动 engine：等它进入 running
	deadline := time.Now().Add(5 * time.Second)
	for !h.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("live engine never reached running state")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 先行关停路：engine 真实优雅关停（状态翻转为非 running）
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("first-path shutdown failed: %v", err)
	}

	// 败者路：进回调时 engine 已非 running，errStatusNotRunning 容忍为 nil
	if err := closer(context.Background()); err != nil {
		t.Fatalf("race loser (engine stopped at entry) must be tolerated, got: %v", err)
	}
}

// TestRunServer_StartupFailureTriggersShutdown 验证 h.Run() 失败（端口占用等
// 启动失败）时调用 app.RequestShutdown()，解除 userFn 阻塞、触发优雅关机，
// 而不是只在后台打一条日志让进程"假活"。
func TestRunServer_StartupFailureTriggersShutdown(t *testing.T) {
	fa := newFakeApp()

	// 端口 1 绑定需要特权，普通用户下必然监听失败，使 h.Run() 同步返回 error。
	h := server.New(server.WithHostPorts("127.0.0.1:1"))
	RunServer(fa, "failing-hertz", h)
	if fa.backgroundFunc == nil {
		t.Fatal("RunServer did not start background run")
	}

	select {
	case <-fa.requestedCh:
		// RequestShutdown 被调用，符合预期
	case <-time.After(5 * time.Second):
		t.Fatal("RunServer did not call RequestShutdown after startup failure")
	}

	fa.cancel() // 清理
}

// TestRunServer_DocAnchorsExitWaitTime 钉住 RunServer doc 包含关键
// 关闭语义关键字:ExitWaitTime (5s 默认 + 推荐显式设置)。
//
// 官方依据:https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/graceful-shutdown/
//
//	"如需修改等待超时时间,可通过 server.WithExitWaitTime() 进行配置"
//
// 实现:用 go/parser 读 hertz/bootstrap.go AST,锚定 RunServer func decl
// 关联的 doc 字符串,hertz 升级或注释误改时立即 fail。
func TestRunServer_DocAnchorsExitWaitTime(t *testing.T) {
	doc := runServerGodoc(t)
	required := []string{
		"ExitWaitTime",            // 排空上限
		"默认 5s",                   // hertz 官方默认
		"server.WithExitWaitTime", // 推荐显式设置
	}
	for _, s := range required {
		if !strings.Contains(doc, s) {
			t.Errorf("RunServer doc missing required anchor %q (doc=\n%s)", s, doc)
		}
	}
}

// runServerGodoc 读 hertz/bootstrap.go 源码,parse AST,找 RunServer 函数的
// godoc 字符串(// 紧贴 func decl 之前的 comment group)。
func runServerGodoc(t *testing.T) string {
	t.Helper()
	bootstrapPath := "bootstrap.go" // 测试 run 时 cwd 在 ekit repo root
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, bootstrapPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse bootstrap.go: %v", err)
	}
	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "RunServer" {
			continue
		}
		if fn.Doc == nil {
			t.Fatal("RunServer has no godoc; add a comment block immediately above the func decl")
		}
		return fn.Doc.Text()
	}
	t.Fatal("RunServer func decl not found in bootstrap.go")
	return ""
}

func BenchmarkNewLogger(b *testing.B) {
	logger := logx.WithService("bench")
	b.ResetTimer()
	for range b.N {
		NewLogger(logger)
	}
}

func BenchmarkHlogAdapter_Info(b *testing.B) {
	logger := NewLogger(logx.WithService("bench"))
	b.ResetTimer()
	for range b.N {
		logger.Info("benchmark message", "key", "value")
	}
}

func BenchmarkHlogAdapter_CtxInfof(b *testing.B) {
	logger := NewLogger(logx.WithService("bench"))
	ctx := context.Background()
	b.ResetTimer()
	for i := range b.N {
		logger.CtxInfof(ctx, "benchmark message %d", i)
	}
}

var _ = config.Option{}

// fakeLifecycleApp 是 lifecycle.App 的最小测试实现（ready 探活用）。
type fakeLifecycleApp struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closers   []string
	shutdowns atomic.Int64 // RunServer 后台 goroutine 调用，须原子
}

func newFakeLifecycleApp() *fakeLifecycleApp {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeLifecycleApp{ctx: ctx, cancel: cancel}
}

func (f *fakeLifecycleApp) Context() context.Context { return f.ctx }

func (f *fakeLifecycleApp) AddResource(name string, _ func(ctx context.Context) error) {
	f.closers = append(f.closers, name)
}

func (f *fakeLifecycleApp) GoBackground(_ string, fn func(ctx context.Context)) {
	go fn(f.ctx)
}

func (f *fakeLifecycleApp) RequestShutdown() {
	f.shutdowns.Add(1)
	f.cancel()
}

// TestRunServer_ReadyProbePassed 钉住探活通过路径：**server 自己绑定端口**，
// 探活通过后 onReady 被执行，且端口确实在监听。
//
// #43 补强：本用例此前自己 net.Listen 占住端口再构造 hertz server，于是探活
// 连的是那个假监听器，而 hertz 真正启动时会因端口被占而失败——测试没有覆盖
// 「探活目标由 server 自己绑定」这一真实形态（旧实现下该形态必然探活超时、
// server 根本不启动）。
func TestRunServer_ReadyProbePassed(t *testing.T) {
	// 取一个空闲端口后立即释放，交由 hertz server 自己绑定。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	h := server.New(server.WithHostPorts(addr))
	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	var readyCalled atomic.Int64
	RunServer(app, "test-svc", h,
		WithOnReady(func(ctx context.Context) { readyCalled.Add(1) }),
		WithReadyProbeTimeout(5*time.Second))

	deadline := time.After(5 * time.Second)
	for readyCalled.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("onReady not called within deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := readyCalled.Load(); got != 1 {
		t.Fatalf("onReady called %d times, want 1", got)
	}
	// 关键断言：server 真的在监听（旧实现下探活超时 → h.Run() 根本不调用）
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("server must be listening on %s: %v", addr, err)
	}
	_ = conn.Close()
	if len(app.closers) != 1 || app.closers[0] != "test-svc" {
		t.Fatalf("closers = %v", app.closers)
	}
}

// TestRunServer_ReadyProbe_ForeignListenerNotRegistered 钉住「端口被他人占用」
// 时不得回调 onReady：探活会连上**对方**的监听器，但本进程的 server 实际
// 启动失败（netpoll 监听失败直接 panic）——此时回调 onReady 会把不属于自己的
// 实例注册到注册中心（app.Ready() → NNS 延迟注册）。
func TestRunServer_ReadyProbe_ForeignListenerNotRegistered(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	h := server.New(server.WithHostPorts(ln.Addr().String()))
	app := newFakeLifecycleApp()

	var readyCalled atomic.Int64
	RunServer(app, "test-svc", h,
		WithOnReady(func(ctx context.Context) { readyCalled.Add(1) }),
		WithReadyProbeTimeout(2*time.Second))

	time.Sleep(1500 * time.Millisecond)
	if readyCalled.Load() != 0 {
		t.Fatal("onReady must not fire when our server failed to bind (probe hit a foreign listener)")
	}
}

// TestRunServer_ReadyProbeTimeout 钉住探活超时路径：探活目标不可建连时
// 按启动失败处理（RequestShutdown）。
//
// 用 HostPorts("127.0.0.1:0")：server 绑定随机端口正常启动，而探活目标
// （字面量 127.0.0.1:0）永远不可建连 ⇒ 探活必然超时。
func TestRunServer_ReadyProbeTimeout(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	var readyCalled atomic.Int64
	RunServer(app, "test-svc", h,
		WithOnReady(func(ctx context.Context) { readyCalled.Add(1) }),
		WithReadyProbeTimeout(300*time.Millisecond))

	time.Sleep(900 * time.Millisecond)
	if readyCalled.Load() != 0 {
		t.Fatal("onReady should not be called on probe timeout")
	}
	if app.shutdowns.Load() == 0 {
		t.Fatal("probe timeout should request shutdown")
	}
}

// TestRunServer_NoOnReady_DefaultShape 钉住默认路径：未设置 OnReady 时
// 不探活、行为与 v0.5.x 一致。
func TestRunServer_NoOnReady_DefaultShape(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	app := newFakeLifecycleApp()

	RunServer(app, "test-svc", h)
	time.Sleep(100 * time.Millisecond)

	if len(app.closers) != 1 {
		t.Fatalf("closers = %v", app.closers)
	}
}

// orderingFakeApp 记录「onReady 同步回调」与「GoBackground 启动」的先后顺序
// （#129/#132：负数禁用探活时，onReady 必须在 GoBackground 启动前同步执行）。
type orderingFakeApp struct {
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	order      []string
	shutdowns  atomic.Int64
	background func(ctx context.Context)
}

func newOrderingFakeApp() *orderingFakeApp {
	ctx, cancel := context.WithCancel(context.Background())
	return &orderingFakeApp{ctx: ctx, cancel: cancel}
}

func (f *orderingFakeApp) log(ev string) {
	f.mu.Lock()
	f.order = append(f.order, ev)
	f.mu.Unlock()
}

func (f *orderingFakeApp) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *orderingFakeApp) Context() context.Context { return f.ctx }

func (f *orderingFakeApp) AddResource(string, func(context.Context) error) {}

func (f *orderingFakeApp) GoBackground(_ string, fn func(ctx context.Context)) {
	f.log("goBackground")
	// 刻意不立刻执行 fn：本用例只验证同步语义（onReady 先于 GoBackground
	// 注册发生），不真正启动 server。
	f.background = fn
}

func (f *orderingFakeApp) RequestShutdown() {
	f.shutdowns.Add(1)
}

// TestRunServer_ReadyProbeDisabled_SyncOnReadyBeforeRun 钉住负数路径的
// 文档语义（#129/#132）：WithReadyProbeTimeout(负数) = 禁用探活——
// onReady 在 RunServer 返回前**同步**执行（早于 GoBackground），
// 且不启动探活协程（探活目标不可拨号也不会触发 RequestShutdown——
// 旧实现照跑探活，5s 窗口后被禁用的探活把服务在启动期杀掉）。
func TestRunServer_ReadyProbeDisabled_SyncOnReadyBeforeRun(t *testing.T) {
	fa := newOrderingFakeApp()
	// 探活目标不可拨号（端口 0 永远拒绝建连）：若负数没真正禁用探活，
	// 旧实现的探活协程会在超时后 RequestShutdown。
	h := server.New(server.WithHostPorts("127.0.0.1:0"))

	var readyCalled atomic.Int64
	RunServer(fa, "disabled-probe-svc", h,
		WithOnReady(func(ctx context.Context) {
			fa.log("onReady")
			readyCalled.Add(1)
		}),
		WithReadyProbeTimeout(-1*time.Second),
	)

	// 同步语义：RunServer 返回时 onReady 已执行且恰好一次，先于 GoBackground。
	if got := readyCalled.Load(); got != 1 {
		t.Fatalf("onReady called %d times synchronously, want 1", got)
	}
	if order := fa.snapshot(); len(order) != 2 || order[0] != "onReady" || order[1] != "goBackground" {
		t.Fatalf("order = %v, want [onReady goBackground]", order)
	}

	// 禁用的探活不得杀服务：静置一段窗口（远小于旧实现的 5s 探活超时），
	// 不应出现任何 RequestShutdown。
	time.Sleep(600 * time.Millisecond)
	if got := fa.shutdowns.Load(); got != 0 {
		t.Fatalf("disabled probe must not request shutdown, got %d", got)
	}
	fa.cancel()
}

// TestRunServer_ReadyProbeDisabled_NeverDials 钉住 #146 的「不发 dial」半边：
// 负数禁用探活后，即使 Options.Addr 指向**可建连**的真实监听器，也不得对它
// 发起任何 TCP 探活。计数 listener 记录每一条建连——「探活目标可建连」排除了
// 「没 dial 是因为连不上」的假阴性，任何路径的探活 dial 都会被计数器捕获
// （旧实现禁用形同虚设，照常 dial 后按结果回调/关机）。
//
// 与 TestRunServer_ReadyProbeDisabled_SyncOnReadyBeforeRun 互补：本用例钉
// 「零 dial」，后者钉「同步回调先于 GoBackground」——探活若因回归被复活，
// 两条用例至少一条转红（dial 逃不过计数器；回调顺序逃不过 ordering 断言）。
func TestRunServer_ReadyProbeDisabled_NeverDials(t *testing.T) {
	// 「他人」的监听器：可建连、可计数。GoBackground 刻意不执行（ordering
	// fake 同款），h.Run() 不会与它抢端口；若负数没真正禁用探活，任何位置的
	// 探活 dial 都会落到计数器上。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int64
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			dials.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-acceptDone
	})

	fa := newOrderingFakeApp()
	h := server.New(server.WithHostPorts(ln.Addr().String()))

	var readyCalled atomic.Int64
	RunServer(fa, "no-dial-svc", h,
		WithOnReady(func(ctx context.Context) {
			fa.log("onReady")
			readyCalled.Add(1)
		}),
		WithReadyProbeTimeout(-1*time.Second),
	)

	// 同步语义（与 SyncOnReadyBeforeRun 同款断言）：onReady 恰好一次、先于
	// GoBackground——回归复活的探活协程只能晚于 GoBackground 存在，回调
	// 顺序必被 ordering 抓住。
	if got := readyCalled.Load(); got != 1 {
		t.Fatalf("onReady called %d times synchronously, want 1", got)
	}
	if order := fa.snapshot(); len(order) != 2 || order[0] != "onReady" || order[1] != "goBackground" {
		t.Fatalf("order = %v, want [onReady goBackground]", order)
	}

	// 静置窗口（旧实现下探活协程早已启动并进入重试循环的时间尺度）：
	// 对可建连目标零建连、零关机请求。
	time.Sleep(600 * time.Millisecond)
	if got := dials.Load(); got != 0 {
		t.Fatalf("disabled probe must not dial the target, got %d dials", got)
	}
	if got := fa.shutdowns.Load(); got != 0 {
		t.Fatalf("disabled probe must not request shutdown, got %d", got)
	}
	fa.cancel()
}

// TestRunServer_ReadyProbeDisabled_RealRunStillServes 钉住负数路径与真实
// h.Run() 的组合（#132 复现形态）：server 正常启动并监听（Addr 绑随机端口），
// onReady 恰好一次，且无探活失败关机。
func TestRunServer_ReadyProbeDisabled_RealRunStillServes(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"),
		server.WithExitWaitTime(100*time.Millisecond),
	)
	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	var readyCalled atomic.Int64
	RunServer(app, "disabled-probe-run", h,
		WithOnReady(func(ctx context.Context) { readyCalled.Add(1) }),
		WithReadyProbeTimeout(-1),
	)

	// #132 复现断言：旧实现 onReady 调用数 = 0（探活在跑），这里必须立刻为 1。
	if got := readyCalled.Load(); got != 1 {
		t.Fatalf("onReady called %d times immediately after RunServer, want 1 (sync path)", got)
	}

	time.Sleep(600 * time.Millisecond)
	if got := readyCalled.Load(); got != 1 {
		t.Fatalf("onReady called %d times, want exactly 1", got)
	}
	if got := app.shutdowns.Load(); got != 0 {
		t.Fatalf("disabled probe must not request shutdown, got %d", got)
	}
}

// TestRunServer_ReadyProbeTimeoutZero_StillProbes 钉住零值路径（#129/#132）：
// WithReadyProbeTimeout(0) 不是「禁用」——不取同步回调捷径，探活照常进行
// （超时归一为默认 5s）。探活目标 127.0.0.1:0 不可建连 ⇒ 零值下 onReady
// 不应被调用（探活窗口内既没通过也没失败到关机）。
func TestRunServer_ReadyProbeTimeoutZero_StillProbes(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"),
		server.WithExitWaitTime(100*time.Millisecond),
	)
	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	var readyCalled atomic.Int64
	RunServer(app, "zero-probe-svc", h,
		WithOnReady(func(ctx context.Context) { readyCalled.Add(1) }),
		WithReadyProbeTimeout(0),
	)

	// 同步回调不存在：RunServer 返回时 onReady 必须尚未执行。
	if got := readyCalled.Load(); got != 0 {
		t.Fatalf("zero timeout must not take the sync-callback path, onReady called %d", got)
	}

	// 600ms < 默认 5s 探活窗口：探活仍在进行，未通过也未到失败关机。
	time.Sleep(600 * time.Millisecond)
	if got := readyCalled.Load(); got != 0 {
		t.Fatalf("onReady must not fire while probing an undialable addr, got %d", got)
	}
	if got := app.shutdowns.Load(); got != 0 {
		t.Fatalf("probe must still be within default 5s window, shutdowns = %d", got)
	}
}

// TestWithReadyProbeTimeout_Semantics 单元钉住选项语义（#129）：
// 负数=禁用探活；零/缺省=探活 + 默认 5s；正数=按值。
func TestWithReadyProbeTimeout_Semantics(t *testing.T) {
	cfg := &runServerConfig{}
	if cfg.probeDisabled {
		t.Fatal("default config must have probe enabled (缺省=探活+默认 5s)")
	}
	if got := cfg.probeTimeoutOrDefault(); got != 5*time.Second {
		t.Fatalf("default probe timeout = %v, want 5s", got)
	}

	WithReadyProbeTimeout(0)(cfg)
	if cfg.probeDisabled {
		t.Fatal("zero timeout must not disable probe")
	}
	if got := cfg.probeTimeoutOrDefault(); got != 5*time.Second {
		t.Fatalf("zero probe timeout = %v, want default 5s", got)
	}

	WithReadyProbeTimeout(3 * time.Second)(cfg)
	if cfg.probeDisabled {
		t.Fatal("positive timeout must not disable probe")
	}
	if got := cfg.probeTimeoutOrDefault(); got != 3*time.Second {
		t.Fatalf("probe timeout = %v, want 3s", got)
	}

	WithReadyProbeTimeout(-1)(cfg)
	if !cfg.probeDisabled {
		t.Fatal("negative timeout must disable probe")
	}
}

// TestRunServer_ReadyProbe_CustomListener 钉住自定义 listener 形态（#131）：
// server.WithListener(ln) 下探活目标为 ln 的地址（hertz v0.10.6 的
// WithListener 会把 Options.Addr 设为 ln.Addr().String()），探活通过后
// onReady 执行。自定义 listener 按官方建议显式用标准库 transporter。
func TestRunServer_ReadyProbe_CustomListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	h := server.New(
		server.WithListener(ln),
		server.WithTransport(standard.NewTransporter),
		server.WithExitWaitTime(100*time.Millisecond),
	)
	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	var readyCalled atomic.Int64
	RunServer(app, "listener-svc", h,
		WithOnReady(func(ctx context.Context) { readyCalled.Add(1) }),
		WithReadyProbeTimeout(5*time.Second),
	)

	deadline := time.After(5 * time.Second)
	for readyCalled.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("onReady not called within deadline for custom-listener server")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := readyCalled.Load(); got != 1 {
		t.Fatalf("onReady called %d times, want 1", got)
	}
	// server 确实在 listener 上服务
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("server must serve on listener addr %s: %v", ln.Addr(), err)
	}
	_ = conn.Close()
}

// TestRunServer_ReadyProbe_AddrEmptyWithListener_ProbesListenerAddr 钉住
// Addr 被显式清空但保留 Listener 的形态（#131）：退回探 listener 地址，
// 而不是零探活立即回调（旧实现存在「回调时 server 尚未 serve」的窗口）。
func TestRunServer_ReadyProbe_AddrEmptyWithListener_ProbesListenerAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	h := server.New(
		server.WithListener(ln),
		server.WithTransport(standard.NewTransporter),
		server.WithHostPorts(""), // 显式清空 Addr：触发 listener 退回路径
		server.WithExitWaitTime(100*time.Millisecond),
	)
	if got := h.GetOptions().Addr; got != "" {
		t.Fatalf("precondition: Options.Addr = %q, want empty", got)
	}
	if h.GetOptions().Listener == nil {
		t.Fatal("precondition: Options.Listener must be set")
	}

	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	var readyCalled atomic.Int64
	var ranWhenReady atomic.Bool
	RunServer(app, "empty-addr-listener-svc", h,
		WithOnReady(func(ctx context.Context) {
			ranWhenReady.Store(h.IsRunning())
			readyCalled.Add(1)
		}),
		WithReadyProbeTimeout(5*time.Second),
	)

	deadline := time.After(5 * time.Second)
	for readyCalled.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("onReady not called within deadline (listener addr must be probed)")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !ranWhenReady.Load() {
		t.Fatal("onReady must fire only after engine running (no pre-listen window)")
	}
}

// TestRunServer_ReadyProbe_AddrEmptyNoListener_WaitsForRunning 钉住
// Addr 与 Listener 皆空的形态（#131）：server 将在 h.Run() 内绑定随机端口，
// 地址事先未知、无法 dial——onReady 至少要等到 engine 进入 running 才执行，
// 消除旧实现「零探活立即回调」的未监听窗口。
func TestRunServer_ReadyProbe_AddrEmptyNoListener_WaitsForRunning(t *testing.T) {
	h := server.New(
		server.WithHostPorts(""), // Addr 为空：h.Run() 绑随机端口
		server.WithExitWaitTime(100*time.Millisecond),
	)
	if got := h.GetOptions().Addr; got != "" {
		t.Fatalf("precondition: Options.Addr = %q, want empty", got)
	}

	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	var readyCalled atomic.Int64
	var runningWhenReady atomic.Bool
	RunServer(app, "empty-addr-svc", h,
		WithOnReady(func(ctx context.Context) {
			runningWhenReady.Store(h.IsRunning())
			readyCalled.Add(1)
		}),
		WithReadyProbeTimeout(5*time.Second),
	)

	deadline := time.After(5 * time.Second)
	for readyCalled.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("onReady not called within deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// 核心回归断言：旧实现立即回调（IsRunning=false）；新实现等 running。
	if !runningWhenReady.Load() {
		t.Fatal("onReady fired before engine running (unlistened-registration window)")
	}
}
