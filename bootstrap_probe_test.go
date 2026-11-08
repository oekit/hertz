package hertz

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/oekit/logx"
	"github.com/oekit/pkg/errors"
)

// captureHandler 收集 slog 记录，用于断言日志级别与文案。
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// find 返回第一条「级别为 level 且文案含 substr」的记录。
func (h *captureHandler) find(level slog.Level, substr string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, substr) {
			return r, true
		}
	}
	return slog.Record{}, false
}

// count 返回「级别为 level 且文案含 substr」的记录条数。
func (h *captureHandler) count(level slog.Level, substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, substr) {
			n++
		}
	}
	return n
}

// installCapturingLogger 装一个把日志收进 captureHandler 的全局默认 logger，
// 用完自动还原。级别显式钉死（不依赖 LOG_LEVEL 环境变量）保证断言可复现。
func installCapturingLogger(t *testing.T) *captureHandler {
	t.Helper()
	prev := logx.Default()
	cap := &captureHandler{}
	logx.SetDefault(logx.NewSlogLogger("hertz-test",
		logx.WithHandler(cap),
		logx.WithLevel(logx.LevelDebug)))
	t.Cleanup(func() { logx.SetDefault(prev) })
	return cap
}

// TestRunServer_ReadyProbe_ShutdownAbortIsNotFailure 钉住 #48 的核心语义：
// **正常关停打断探活 = 预期路径，不是探活失败**。
//
// 旧实现把关停记成 ERROR「ready probe failed, requesting shutdown」并再调一次
// app.RequestShutdown()（进程本就在关停，此举多余），值班的人会误以为服务
// 启动失败——k8s 滚动更新里「刚起的 Pod 被取消」每个 Pod 都会留一条 ERROR。
//
// 场景：探活目标恒不可建连（字面量 127.0.0.1:0 ⇒ server 绑的是随机端口），
// 探活一直处于等待态，直到关停打断它——这样测的确实是「被关停打断」而不是
// 「探活超时」。
func TestRunServer_ReadyProbe_ShutdownAbortIsNotFailure(t *testing.T) {
	cap := installCapturingLogger(t)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	var readyCalled atomic.Int64
	RunServer(app, "test-svc", h,
		WithOnReady(func(ctx context.Context) { readyCalled.Add(1) }),
		WithReadyProbeTimeout(30*time.Second))

	// 让探活先跑起来（进入等待态），再模拟「收到 SIGTERM」：
	// bootstrap 的 app-lifecycle:cancel job 在关闭序列第一步取消 app 根 ctx。
	time.Sleep(100 * time.Millisecond)
	app.cancel()

	time.Sleep(500 * time.Millisecond)

	// 断言 1：不得再请求关机——进程本就在关停，RequestShutdown 是多余动作。
	if got := app.shutdowns.Load(); got != 0 {
		t.Fatalf("shutdown requests = %d, want 0 (shutdown abort must not request shutdown)", got)
	}
	// 断言 2：不得记 ERROR「ready probe ...」——关停不是探活失败。
	if n := cap.count(slog.LevelError, "ready probe"); n != 0 {
		t.Fatalf("ERROR records mentioning ready probe = %d, want 0", n)
	}
	// 断言 3：应当记一条 INFO 说明探活是被关停打断的。
	if _, ok := cap.find(slog.LevelInfo, "ready probe aborted"); !ok {
		t.Fatal("missing INFO record about ready probe aborted by shutdown")
	}
	// 断言 4：onReady 不得被调用（探活确实没通过）。
	if got := readyCalled.Load(); got != 0 {
		t.Fatalf("onReady called %d times, want 0", got)
	}
}

// TestRunServer_ReadyProbe_ShutdownBeforeProbeStart 钉住 #48 的另一个窗口：
// **关停先于探活开始**（进程刚启动就收到 SIGTERM）。此时探活一进入循环就看到
// ctx 已取消，同样不得报 ERROR、不得请求关机。
func TestRunServer_ReadyProbe_ShutdownBeforeProbeStart(t *testing.T) {
	cap := installCapturingLogger(t)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	// 先取消（模拟关停已启动），再 RunServer。
	app.cancel()

	var readyCalled atomic.Int64
	RunServer(app, "test-svc", h,
		WithOnReady(func(ctx context.Context) { readyCalled.Add(1) }),
		WithReadyProbeTimeout(30*time.Second))

	time.Sleep(500 * time.Millisecond)

	if got := app.shutdowns.Load(); got != 0 {
		t.Fatalf("shutdown requests = %d, want 0", got)
	}
	if n := cap.count(slog.LevelError, "ready probe"); n != 0 {
		t.Fatalf("ERROR records mentioning ready probe = %d, want 0", n)
	}
	if _, ok := cap.find(slog.LevelInfo, "ready probe aborted"); !ok {
		t.Fatal("missing INFO record about ready probe aborted by shutdown")
	}
	if got := readyCalled.Load(); got != 0 {
		t.Fatalf("onReady called %d times, want 0", got)
	}
}

// TestProbeAndReady_AbortedReturnsSentinel 钉住探活侧的哨兵语义：
// ctx 被取消 ⇒ 返回可判别的 errProbeAborted（errors.Is 命中）。
func TestProbeAndReady_AbortedReturnsSentinel(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	app := newFakeLifecycleApp()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消

	err := probeAndReady(ctx, app, "test-svc", h, &runServerConfig{
		onReady:      func(context.Context) {},
		probeTimeout: 30 * time.Second,
	})
	if !errors.Is(err, errProbeAborted) {
		t.Fatalf("probeAndReady err = %v, want errProbeAborted", err)
	}
}

// TestProbeAndReady_TimeoutIsNotAbort 钉住**既有语义不变**：探活超时仍是
// 启动失败（普通 error），不得被误判成「被关停打断」——否则真正的启动失败
// 会被悄悄放过（不再请求关机 ⇒ 服务假活）。
func TestProbeAndReady_TimeoutIsNotAbort(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	app := newFakeLifecycleApp()

	err := probeAndReady(context.Background(), app, "test-svc", h, &runServerConfig{
		onReady:      func(context.Context) {},
		probeTimeout: 100 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("probeAndReady must fail when the probe target is unreachable")
	}
	if errors.Is(err, errProbeAborted) {
		t.Fatalf("probe timeout must not be reported as abort: %v", err)
	}
}

// TestRunServer_ReadyProbeTimeoutStillShutsDown 钉住 #48 修复**没有**削弱
// 「探活超时 ⇒ 按启动失败关机」这条既有语义（与 #43 的行为一致）。
func TestRunServer_ReadyProbeTimeoutStillShutsDown(t *testing.T) {
	cap := installCapturingLogger(t)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	app := newFakeLifecycleApp()
	defer func() { _ = h.Shutdown(context.Background()) }()

	RunServer(app, "test-svc", h,
		WithOnReady(func(context.Context) {}),
		WithReadyProbeTimeout(200*time.Millisecond))

	time.Sleep(900 * time.Millisecond)

	if got := app.shutdowns.Load(); got == 0 {
		t.Fatal("probe timeout must still request shutdown")
	}
	if _, ok := cap.find(slog.LevelError, "ready probe failed"); !ok {
		t.Fatal("probe timeout must still log ERROR ready probe failed")
	}
}

// TestCallOnReady_PanicBecomesError #152 单元面：callOnReady 把 panic 转为
// 可判别 error（探活失败链的入口），正常回调零影响。
func TestCallOnReady_PanicBecomesError(t *testing.T) {
	err := callOnReady("svc-x", func(context.Context) { panic("registration backend exploded") },
		context.Background())
	if err == nil || !strings.Contains(err.Error(), "onReady panicked") ||
		!strings.Contains(err.Error(), "registration backend exploded") {
		t.Fatalf("callOnReady err = %v, want panic converted to error", err)
	}
	if err := callOnReady("svc-x", func(context.Context) {}, context.Background()); err != nil {
		t.Fatalf("normal onReady must not error: %v", err)
	}
}

// TestRunServer_OnReadyPanicShutsDown #152 回归（issue 探针原样）：探活通过后
// onReady panic 不得被 async.GoSafe 静默吞掉——修复前 panic 沿 probeAndReady
// 展开，外层闭包的「探活失败 ⇒ RequestShutdown」处置整段被跳过 ⇒ server 以
// 「探活已过、回调丢失、永不注册」的半启动形态继续 serve。修复后 panic 在
// probeAndReady 内转为 error，走探活失败语义：ERROR + RequestShutdown。
func TestRunServer_OnReadyPanicShutsDown(t *testing.T) {
	cap := installCapturingLogger(t)

	// 取一个空闲端口后立即释放，交由 hertz server 自己绑定（探活能真实通过）。
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
		WithOnReady(func(ctx context.Context) {
			readyCalled.Add(1)
			panic("registration backend exploded")
		}),
		WithReadyProbeTimeout(5*time.Second))

	// 等 RequestShutdown（修复前永不发生——半启动 server 挂着不关）。
	deadline := time.After(5 * time.Second)
	for app.shutdowns.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("panicking onReady must request shutdown (got %d), server would keep serving unregistered",
				app.shutdowns.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := readyCalled.Load(); got != 1 {
		t.Fatalf("onReady called %d times, want 1 (probe passed, callback entered)", got)
	}
	if _, ok := cap.find(slog.LevelError, "ready probe failed"); !ok {
		t.Fatal("onReady panic must take the probe-failure path (ERROR + shutdown)")
	}
}
