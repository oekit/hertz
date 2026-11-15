package hertztracer

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/cloudwego/hertz/pkg/common/tracer/stats"
	"github.com/cloudwego/hertz/pkg/common/tracer/traceinfo"
	"github.com/cloudwego/hertz/pkg/route"
	opentracing "github.com/opentracing/opentracing-go"
)

// countingTracer 记录 StartSpan 调用（次数 + span 名），用于钉住 TracerOption 生效。
type countingTracer struct {
	mu       sync.Mutex
	spans    int
	lastName string
}

func (t *countingTracer) StartSpan(operationName string, opts ...opentracing.StartSpanOption) opentracing.Span {
	t.mu.Lock()
	t.spans++
	t.lastName = operationName
	t.mu.Unlock()
	return opentracing.NoopTracer{}.StartSpan(operationName, opts...)
}

func (t *countingTracer) Inject(_ opentracing.SpanContext, _ any, _ any) error { return nil }

func (t *countingTracer) Extract(_ any, _ any) (opentracing.SpanContext, error) {
	return nil, opentracing.ErrUnsupportedFormat
}

func (t *countingTracer) snapshot() (int, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spans, t.lastName
}

// TestNewServerMiddleware_ReturnsHandler 验证 NewServerMiddleware 返回非 nil。
func TestNewServerMiddleware_ReturnsHandler(t *testing.T) {
	// 设置一个 noop tracer(避免 opentracing.GlobalTracer() panic)
	opentracing.SetGlobalTracer(opentracing.NoopTracer{})
	defer opentracing.SetGlobalTracer(opentracing.NoopTracer{})

	h := NewServerMiddleware()
	if h == nil {
		t.Fatal("NewServerMiddleware returned nil")
	}
}

// TestNewClientMiddleware_ReturnsMiddleware 验证 client middleware 构造。
func TestNewClientMiddleware_ReturnsMiddleware(t *testing.T) {
	mw := NewClientMiddleware()
	if mw == nil {
		t.Fatal("NewClientMiddleware returned nil")
	}
}

// TestNewServerTracer_ReturnsTracer 验证 NewServerTracer 透传上游。
func TestNewServerTracer_ReturnsTracer(t *testing.T) {
	tracer := NewServerTracer(
		opentracing.NoopTracer{},
		func(c *app.RequestContext) string {
			return "GET " + string(c.Path())
		},
	)
	if tracer == nil {
		t.Fatal("NewServerTracer returned nil")
	}
	// 验证能用作 server.WithTracer
	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
		server.WithTracer(tracer),
	)
	_ = h
}

// TestTracerOption_TypeAlias 验证 TracerOption 类型存在。
func TestOption_TypeAlias(t *testing.T) {
	var _ []TracerOption
	_ = client.NewClient
	_ = app.NewContext(0)
}

// TestTracerOption_IsNotBareStruct 钉住 AGENTS.md「禁止裸 Option」：TracerOption 必须是
// 有真实字段的选项类型（函数选项），不是空壳 struct{}。
func TestOption_IsNotBareStruct(t *testing.T) {
	opt := WithTracer(opentracing.NoopTracer{})
	if opt == nil {
		t.Fatal("WithTracer must return a usable TracerOption")
	}
	cfg := newTracerConfig([]TracerOption{opt})
	if cfg.tracer == nil {
		t.Fatal("option must carry a real tracer")
	}
	if cfg.formOperationName == nil {
		t.Fatal("option must carry a default operation-name formatter")
	}
}

// TestWithTracer_OptionTakesEffect 钉住「参数真正生效」：显式给的 tracer 与
// span 名被中间件采用（此前 NewServerMiddleware 忽略全部参数 ⇒ 空壳）。
func TestWithTracer_OptionTakesEffect(t *testing.T) {
	tr := &countingTracer{}
	mw := NewServerMiddleware(
		WithTracer(tr),
		WithOperationName(func(c *app.RequestContext) string { return "custom-op" }),
	)

	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.SetHandlers(app.HandlersChain{func(ctx context.Context, c *app.RequestContext) {
		c.Next(ctx)
	}})
	mw(context.Background(), c)

	spans, name := tr.snapshot()
	if spans != 1 {
		t.Errorf("configured tracer must be used: spans = %d, want 1", spans)
	}
	if name != "custom-op" {
		t.Errorf("operation name = %q, want custom-op", name)
	}
}

// TestNewServerMiddleware_WithoutServerTracer_ContinuesChain 钉住：只 h.Use
// 中间件（engine 未装 tracer）时链路必须继续——上游 ServerCtx 在拿不到
// trace container 时会记录 "get tracer container failed" 并**中断链路**
// （不 c.Next ⇒ 请求拿不到响应）。
func TestNewServerMiddleware_WithoutServerTracer_ContinuesChain(t *testing.T) {
	opentracing.SetGlobalTracer(opentracing.NoopTracer{})
	mw := NewServerMiddleware()

	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	called := false
	c.SetHandlers(app.HandlersChain{func(ctx context.Context, c *app.RequestContext) {
		called = true
		c.Next(ctx)
	}})
	mw(context.Background(), c)

	if !called {
		t.Error("middleware must continue the chain when no server tracer is installed")
	}
}

// TestNewServerTracing_InstallsServerTracer 钉住成对组件把 server tracer
// 装进 engine（span 的来源）。
func TestNewServerTracing_InstallsServerTracer(t *testing.T) {
	tracing := NewServerTracing()
	opts := config.NewOptions(nil)
	tracing.Option.F(opts)
	engine := route.NewEngine(opts)

	if !engine.GetTracer().HasTracer() {
		t.Error("NewServerTracing option must install the server tracer (span source)")
	}
}

// captureHlog 把 hertz hlog 输出重定向到内存 buffer(断言上游 ServerCtx 是否
// 被调用过——它拿不到 traceContainer 时必打 "get tracer container failed")。
func captureHlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	hlog.SetOutput(&buf)
	return &buf, func() { hlog.SetOutput(os.Stderr) }
}

// TestNewServerMiddleware_MixedAssembly_ContinuesChain 钉住 #288:engine 装了
// **非 contrib** tracer(hertz 给 RequestContext 预置了 TraceInfo、但没有
// contrib 的 traceContainer)时,中间件不得把控制权交给上游 ServerCtx——上游
// 查不到 container 会记录 "get tracer container failed" 并**不 c.Next**,
// 整条链路被截断成空响应。判不真必须走自建 span 分支(链路不中断)。
func TestNewServerMiddleware_MixedAssembly_ContinuesChain(t *testing.T) {
	opentracing.SetGlobalTracer(opentracing.NoopTracer{})
	prev := serverTracerCreated.Load()
	serverTracerCreated.Store(false) // 本进程未配对安装(只 h.Use 中间件的形态)
	defer serverTracerCreated.Store(prev)

	selfTracer := &countingTracer{}
	mw := NewServerMiddleware(WithTracer(selfTracer))

	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	// 模拟 engine 挂了任意非 contrib tracer:initTrace 发现有 tracer 即预置 TraceInfo
	c.SetTraceInfo(traceinfo.NewTraceInfo())

	called := false
	c.SetHandlers(app.HandlersChain{func(ctx context.Context, c *app.RequestContext) {
		called = true
		c.Next(ctx)
	}})
	logBuf, restoreLog := captureHlog(t)
	mw(context.Background(), c)
	restoreLog()

	if !called {
		t.Fatal("mixed assembly must not interrupt the chain (terminal handler must run, #288)")
	}
	if spans, _ := selfTracer.snapshot(); spans != 1 {
		t.Errorf("self-built span = %d, want 1 (fallback branch must create its own span)", spans)
	}
	// 判据为假时上游 ServerCtx 根本不该被调用——它拿不到 container 必打这条
	// ERROR(#288 的症状之一是每请求一条),出现即说明判据退回了 GetTraceInfo。
	if strings.Contains(logBuf.String(), "get tracer container failed") {
		t.Errorf("upstream ServerCtx must not be invoked in mixed assembly, got log: %s", logBuf.String())
	}
}

// TestNewServerMiddleware_PairedServerTracer_UsesUpstream 钉住配对形态仍走
// 上游 ServerCtx:span 由 contrib server tracer 创建(上游 tracer 计数为 1),
// 本包不再自建 span(避免双 span),链路照常推进。
func TestNewServerMiddleware_PairedServerTracer_UsesUpstream(t *testing.T) {
	opentracing.SetGlobalTracer(opentracing.NoopTracer{})
	prev := serverTracerCreated.Load()
	serverTracerCreated.Store(false)
	defer serverTracerCreated.Store(prev)

	upstreamTracer := &countingTracer{}
	ct := NewServerTracer(upstreamTracer, nil)
	if !serverTracerCreated.Load() {
		t.Fatal("NewServerTracer must mark serverTracerCreated (#288 判据)")
	}

	selfTracer := &countingTracer{}
	mw := NewServerMiddleware(WithTracer(selfTracer))

	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	// engine 装了 contrib tracer 的形态:TraceInfo 预置 + HTTPStart 事件已记录
	// (hertz engine DoStart 的 Record,这里手工补齐等价物)
	ti := traceinfo.NewTraceInfo()
	ti.Stats().SetLevel(stats.LevelDetailed)
	ti.Stats().Record(stats.HTTPStart, stats.StatusInfo, "")
	c.SetTraceInfo(ti)

	called := false
	c.SetHandlers(app.HandlersChain{func(ctx context.Context, c *app.RequestContext) {
		called = true
		c.Next(ctx)
	}})
	// engine 的 contrib tracer 在 DoStart 时把 traceContainer 注入 ctx
	mw(ct.Start(context.Background(), c), c)

	if !called {
		t.Fatal("paired assembly must keep the chain running")
	}
	if spans, _ := upstreamTracer.snapshot(); spans != 1 {
		t.Errorf("upstream (contrib) span = %d, want 1 (paired path must drive upstream ServerCtx)", spans)
	}
	if spans, _ := selfTracer.snapshot(); spans != 0 {
		t.Errorf("self-built span = %d, want 0 (paired path must not create a second span)", spans)
	}
}

// TestNewServerMiddleware_UpstreamEarlyReturn_FallsBack 钉住「链路不中断」的
// 兜底:判据为真但上游 ServerCtx 早退(进程粒度判据的多 engine / Option 未装
// 上本 engine 形态,拿不到 traceContainer 不 c.Next)时,必须落回自建 span
// 分支把链路接完。上游此时会打一条 "get tracer container failed" ERROR 日志,
// 属预期(该形态无法在请求期逐 engine 验证,见 serverTracerCreated 注释)。
func TestNewServerMiddleware_UpstreamEarlyReturn_FallsBack(t *testing.T) {
	opentracing.SetGlobalTracer(opentracing.NoopTracer{})
	prev := serverTracerCreated.Load()
	serverTracerCreated.Store(true) // 判据②为真(别处已配对),本 engine 却没有 container
	defer serverTracerCreated.Store(prev)

	selfTracer := &countingTracer{}
	mw := NewServerMiddleware(WithTracer(selfTracer))

	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.SetTraceInfo(traceinfo.NewTraceInfo())

	called := false
	c.SetHandlers(app.HandlersChain{func(ctx context.Context, c *app.RequestContext) {
		called = true
		c.Next(ctx)
	}})
	logBuf, restoreLog := captureHlog(t)
	mw(context.Background(), c) // ctx 里没有 contrib traceContainer
	restoreLog()

	if !called {
		t.Fatal("upstream early-return must fall back to self-built span (chain must not break)")
	}
	if spans, _ := selfTracer.snapshot(); spans != 1 {
		t.Errorf("self-built span = %d, want 1 (fallback must create the span)", spans)
	}
	// 钉住本形态确实是「上游被调用过但早退」:上游拿不到 container 必打这条
	// ERROR(也是 [captureHlog] 断言在 MixedAssembly 里有效的正向对照)。
	if !strings.Contains(logBuf.String(), "get tracer container failed") {
		t.Errorf("upstream ServerCtx should have been invoked and early-returned, log = %q", logBuf.String())
	}
}

// TestNewServerTracing_MarksServerTracerCreated 钉住配对入口置位判据②。
func TestNewServerTracing_MarksServerTracerCreated(t *testing.T) {
	prev := serverTracerCreated.Load()
	serverTracerCreated.Store(false)
	defer serverTracerCreated.Store(prev)

	_ = NewServerTracing()
	if !serverTracerCreated.Load() {
		t.Fatal("NewServerTracing must mark serverTracerCreated (#288 判据)")
	}
}
