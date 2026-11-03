package hertztracer

import (
	"context"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/route"
	opentracing "github.com/opentracing/opentracing-go"
)

// serverOptionAsOption 证明 ServerOption 与 Option 是同一类型（别名）。
func serverOptionAsOption(o ServerOption) Option { return o }

// countingTracer 记录 StartSpan 调用（次数 + span 名），用于钉住 Option 生效。
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

// TestOption_TypeAlias 验证 Option 类型存在。
func TestOption_TypeAlias(t *testing.T) {
	var _ []Option
	_ = client.NewClient
	_ = app.NewContext(0)
}

// TestOption_IsNotBareStruct 钉住 AGENTS.md「禁止裸 Option」：Option 必须是
// 有真实字段的选项类型（函数选项），不是空壳 struct{}。
func TestOption_IsNotBareStruct(t *testing.T) {
	opt := WithTracer(opentracing.NoopTracer{})
	if opt == nil {
		t.Fatal("WithTracer must return a usable Option")
	}
	// ServerOption 仍是 Option 的别名（历史导出名保留）——同型可直接互换。
	cfg := newTracerConfig([]Option{serverOptionAsOption(opt)})
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
