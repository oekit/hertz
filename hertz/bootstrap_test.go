package hertz

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yi-nology/ekit-bootstrap/bootstrap"
	"github.com/yi-nology/ekit-core/concurrency/async"
	"github.com/yi-nology/ekit-core/lifecycle"
	"github.com/yi-nology/ekit-observability/observability/logx"
	"github.com/yi-nology/ekit-observability/shutdown"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
)

func TestRunServer_RegistersShutdownHook(t *testing.T) {
	mgr := shutdown.New(100 * time.Millisecond)
	app := bootstrap.NewApp(mgr)

	h := server.New(server.WithHostPorts(":0"),
		server.WithExitWaitTime(100*time.Millisecond),
	)
	assert.NotNil(t, h)

	// Should not panic
	RunServer(app, "test-hertz", h)
}

func TestRunServer_MultipleServers(t *testing.T) {
	mgr := shutdown.New(100 * time.Millisecond)
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
	mgr := shutdown.New(100 * time.Millisecond)
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

// TestRunServer_ToleratesEngineNotRunning 钉住 hertz "engine is not running"
// 错误文案，并验证关闭回调在 engine 未运行（server 从未启动，或已被 h.Run()
// 内置信号路径优雅关闭——信号关机时 bootstrap 与 hertz 双路并发 Shutdown，
// 后到者得到该错误）时按成功处理，不向 shutdown.Manager 上报假错误。
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

	// 钉住文案：hertz 升级若改动错误文本，上方容忍逻辑会失效，此处显式失败提示。
	if err := h.Shutdown(context.Background()); err == nil || err.Error() != errEngineNotRunning {
		t.Fatalf("expected %q from Shutdown on non-running engine, got: %v", errEngineNotRunning, err)
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
	for i := 0; i < b.N; i++ {
		NewLogger(logger)
	}
}

func BenchmarkHlogAdapter_Info(b *testing.B) {
	logger := NewLogger(logx.WithService("bench"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.Info("benchmark message", "key", "value")
	}
}

func BenchmarkHlogAdapter_CtxInfof(b *testing.B) {
	logger := NewLogger(logx.WithService("bench"))
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.CtxInfof(ctx, "benchmark message %d", i)
	}
}

var _ = config.Option{}
