package hertzotel

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	serverconfig "github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/tracer/traceinfo"
	"github.com/cloudwego/hertz/pkg/route"
	hertzcontribtracing "github.com/hertz-contrib/obs-opentelemetry/tracing"
)

// TestNewServerMiddleware_ReturnsHandler 验证 NewServerMiddleware 返回
// 非 nil handler(用 tracetest.SpanRecorder 难在单测中跑 hertz full
// server,所以只验证函数构造正确,完整 OTel 集成在 e2e 测试中验证)。
func TestNewServerMiddleware_ReturnsHandler(t *testing.T) {
	h := NewServerMiddleware(
		hertzcontribtracing.WithShouldIgnore(func(_ context.Context, c *app.RequestContext) bool {
			return string(c.Path()) == "/healthz"
		}),
	)
	if h == nil {
		t.Fatal("NewServerMiddleware returned nil")
	}
}

func TestNewServerMiddleware_NoOptions(t *testing.T) {
	// 走 defaultConfig 路径,内部 otel.GetTracerProvider() 提供 nop tracer
	h := NewServerMiddleware()
	if h == nil {
		t.Fatal("NewServerMiddleware with no opts returned nil")
	}
}

func TestNewClientMiddleware_ReturnsMiddleware(t *testing.T) {
	mw := NewClientMiddleware()
	if mw == nil {
		t.Fatal("NewClientMiddleware returned nil")
	}
}

func TestNewServerTracer_ReturnsOptionAndConfig(t *testing.T) {
	serverOpt, cfg := NewServerTracer()
	// serverconfig.Option 是 struct(内含 F func),非 interface,不能直接 == nil
	// 用 reflect.ValueOf().IsNil() 替代:判断 F 字段是否有效
	if serverOpt.F == nil {
		t.Fatal("NewServerTracer returned server option with nil F")
	}
	if cfg == nil {
		t.Fatal("NewServerTracer returned nil config")
	}
}

func TestNewServerMiddleware_WithMultipleOptions(t *testing.T) {
	// 验证多个 HertzOtelOption 不会 panic
	h := NewServerMiddleware(
		hertzcontribtracing.WithRecordSourceOperation(true),
		hertzcontribtracing.WithCustomResponseHandler(func(_ context.Context, c *app.RequestContext) {}),
	)
	if h == nil {
		t.Fatal("NewServerMiddleware with multiple opts returned nil")
	}
}

func TestOption_TypeAlias(t *testing.T) {
	// HertzOtelOption 应该是 hertzcontribtracing.Option 的 type alias,
	// 业务可直接传上游 Option 不需要 cast。
	var opt = hertzcontribtracing.WithRecordSourceOperation(true)
	if opt == nil {
		t.Fatal("type alias assignment failed")
	}
}

func TestNewServerTracing_PairsOptionAndMiddleware(t *testing.T) {
	tracing := NewServerTracing()
	if tracing.Option.F == nil {
		t.Fatal("NewServerTracing returned option with nil F")
	}
	if tracing.Middleware == nil {
		t.Fatal("NewServerTracing returned nil middleware")
	}
}

func TestNewServerTracing_CanPassToServer(t *testing.T) {
	tracing := NewServerTracing()

	h := server.New(
		server.WithHostPorts(":0"),
		tracing.Option,
		server.WithExitWaitTime(100*time.Millisecond),
	)
	h.Use(tracing.Middleware)
	if h == nil {
		t.Fatal("server.New returned nil")
	}
}

// TestNewServerTracing_InstallsServerTracer 钉住成对组件的 option 把 server
// tracer 装进 engine——这是 span 的**唯一来源**（上游的 trace carrier 由
// server tracer 的 Start 放进 ctx，中间件自己装不了）。
//
// 回归：只 h.Use(NewServerMiddleware(...)) 而不装 server tracer 时，上游
// 拿不到 carrier ⇒ 静默不产 span（"配了 tracing 却一条 span 都没有"）。
func TestNewServerTracing_InstallsServerTracer(t *testing.T) {
	tracing := NewServerTracing()
	opts := config.NewOptions(nil)
	tracing.Option.F(opts)
	engine := route.NewEngine(opts)

	if !engine.GetTracer().HasTracer() {
		t.Error("NewServerTracing option must install the server tracer (span source)")
	}
}

// TestNewServerMiddleware_WithoutServerTracer_Diagnoses 钉住：engine 未装
// server tracer 时必须显式诊断（每进程一次），不再静默哑火；
// 同时链路必须继续（请求仍要有响应）。
func TestNewServerMiddleware_WithoutServerTracer_Diagnoses(t *testing.T) {
	var warned int
	orig := missingServerTracerWarn
	missingServerTracerWarn = func() { warned++ }
	defer func() { missingServerTracerWarn = orig }()

	mw := NewServerMiddleware()
	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	called := false
	c.SetHandlers(app.HandlersChain{func(ctx context.Context, c *app.RequestContext) {
		called = true
		c.Next(ctx)
	}})
	mw(context.Background(), c)

	if warned == 0 {
		t.Error("missing server tracer must be diagnosed (no span would be created)")
	}
	if !called {
		t.Error("middleware must continue the handler chain")
	}
}

// TestNewServerMiddleware_WithServerTracer_NoDiagnostic 装了 server tracer
// （ctx 带 traceInfo）时不得报诊断——否则是误报。
func TestNewServerMiddleware_WithServerTracer_NoDiagnostic(t *testing.T) {
	var warned int
	orig := missingServerTracerWarn
	missingServerTracerWarn = func() { warned++ }
	defer func() { missingServerTracerWarn = orig }()

	mw := NewServerMiddleware()
	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.SetTraceInfo(traceinfo.NewTraceInfo())
	c.SetHandlers(app.HandlersChain{func(ctx context.Context, c *app.RequestContext) {
		c.Next(ctx)
	}})
	mw(context.Background(), c)

	if warned != 0 {
		t.Errorf("must not diagnose when a server tracer is installed (warned %d times)", warned)
	}
}

// TestNewServerMiddleware_MissingTracerMatrix 钉住缺失诊断的判据矩阵（#168）：
// 「engine 未装 otel server tracer」不能用 GetTraceInfo()==nil 判——hertz
// v0.10.6 起只要 engine 挂了**任意** tracer（stats/自研 tracer 实现同一接口）
// 就给 RequestContext 预置 TraceInfo，混合装配（有别的 tracer、缺的恰是 otel
// tracer）下旧判据恒不触发，半截 span 哑火照样发生而警告永不出现。修复后
// 判据为「TraceInfo 缺失（零 tracer）‖ 进程从未创建过 otel server tracer」。
func TestNewServerMiddleware_MissingTracerMatrix(t *testing.T) {
	origWarn := missingServerTracerWarn
	origCreated := serverTracerCreated.Load()
	defer func() {
		missingServerTracerWarn = origWarn
		serverTracerCreated.Store(origCreated)
	}()

	tests := []struct {
		name           string
		traceInfo      bool // engine 挂了任意 tracer ⇒ RequestContext 预置 TraceInfo
		otelCreated    bool // 进程经 NewServerTracer/NewServerTracing 创建过 otel server tracer
		wantDiagnostic bool
	}{
		{
			name:           "zero tracer engine, otel never created",
			traceInfo:      false,
			otelCreated:    false,
			wantDiagnostic: true,
		},
		{
			// #168 回归：混合装配（engine 有非 otel tracer）+ otel tracer
			// 缺失——旧判据 GetTraceInfo()==nil 在此恒 false，哑火无告警
			name:           "mixed assembly: other tracer present, otel never created",
			traceInfo:      true,
			otelCreated:    false,
			wantDiagnostic: true,
		},
		{
			name:           "paired install: otel tracer created",
			traceInfo:      true,
			otelCreated:    true,
			wantDiagnostic: false,
		},
		{
			// 进程别处配过 otel tracer，本 engine 零 tracer：TraceInfo
			// 缺失这半边判据独立兜住（进程级状态覆盖不到的场景）
			name:           "zero tracer engine, otel created elsewhere",
			traceInfo:      false,
			otelCreated:    true,
			wantDiagnostic: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var warned int
			missingServerTracerWarn = func() { warned++ }
			serverTracerCreated.Store(tt.otelCreated)

			mw := NewServerMiddleware()
			c := app.NewContext(0)
			c.Request.SetMethod("GET")
			if tt.traceInfo {
				c.SetTraceInfo(traceinfo.NewTraceInfo())
			}
			chainCalled := false
			c.SetHandlers(app.HandlersChain{func(ctx context.Context, c *app.RequestContext) {
				chainCalled = true
				c.Next(ctx)
			}})
			mw(context.Background(), c)

			if tt.wantDiagnostic && warned == 0 {
				t.Error("expected missing-server-tracer diagnostic, got none")
			}
			if !tt.wantDiagnostic && warned != 0 {
				t.Errorf("unexpected diagnostic (warned %d times)", warned)
			}
			if !chainCalled {
				t.Error("middleware must continue the handler chain")
			}
		})
	}
}

// TestNewServerTracer_MarksPairedInstallState 钉住：「已配对安装」状态只由
// NewServerTracer / NewServerTracing 置位——NewServerMiddleware 内部为建
// 中间件 cfg 也调上游 NewServerTracer，但那份 Option 被丢弃（从未装上
// engine），不得置位，否则缺失诊断自身哑火。
func TestNewServerTracer_MarksPairedInstallState(t *testing.T) {
	origCreated := serverTracerCreated.Load()
	defer serverTracerCreated.Store(origCreated)

	serverTracerCreated.Store(false)
	NewServerTracer()
	if !serverTracerCreated.Load() {
		t.Error("NewServerTracer must mark the paired-install state (#168)")
	}

	serverTracerCreated.Store(false)
	NewServerTracing()
	if !serverTracerCreated.Load() {
		t.Error("NewServerTracing must mark the paired-install state (#168)")
	}
}

// TestNewServerTracing_HalfInstalled_Diagnoses 回归 #230：NewServerTracing 只
// h.Use(Middleware)、不把 Option 传给 server.New（半装配）⇒ 本包缺失诊断必须
// 触发。此前 Middleware 是上游裸中间件（无诊断包装），判据 serverTracerCreated
// 又被本函数自己置位——诊断在主推入口上结构性失效（#168 修法残留）。
func TestNewServerTracing_HalfInstalled_Diagnoses(t *testing.T) {
	var warned int
	orig := missingServerTracerWarn
	missingServerTracerWarn = func() { warned++ }
	defer func() { missingServerTracerWarn = orig }()

	tracing := NewServerTracing()
	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	// engine 零 tracer 场景（RequestContext 无 TraceInfo）⇒ 判据第一支命中。
	tracing.Middleware(context.Background(), c)

	if warned == 0 {
		t.Error("half-installed ServerTracing (Middleware used without Option) must be diagnosed (#230)")
	}
}

// TestNewServerTracing_OptionNotApplied_Diagnoses 钉住判据第二支：engine 预置了
// TraceInfo（挂了任意 tracer）但**本对组件的 Option 从未被应用**（没传给
// server.New）⇒ 诊断仍必须触发——判据必须是「本对组件的 Option 是否真的落地」，
// 不是进程级「创建过 tracer」（后者被本函数一调用就置位，恒为真）。
func TestNewServerTracing_OptionNotApplied_Diagnoses(t *testing.T) {
	var warned int
	orig := missingServerTracerWarn
	missingServerTracerWarn = func() { warned++ }
	defer func() { missingServerTracerWarn = orig }()

	tracing := NewServerTracing()
	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.SetTraceInfo(traceinfo.NewTraceInfo())
	tracing.Middleware(context.Background(), c)

	if warned == 0 {
		t.Error("ServerTracing whose Option was never applied must be diagnosed (#230)")
	}
}

// TestNewServerTracing_PairedInstall_NoDiagnostic 成对安装（Option 实际应用到
// server config）后不得误报。
func TestNewServerTracing_PairedInstall_NoDiagnostic(t *testing.T) {
	var warned int
	orig := missingServerTracerWarn
	missingServerTracerWarn = func() { warned++ }
	defer func() { missingServerTracerWarn = orig }()

	tracing := NewServerTracing()
	// Option 落地：应用到 serverconfig.Options（模拟 server.New 的装配动作）。
	var opts serverconfig.Options
	tracing.Option.F(&opts)

	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.SetTraceInfo(traceinfo.NewTraceInfo())
	tracing.Middleware(context.Background(), c)

	if warned != 0 {
		t.Errorf("paired install must not diagnose (warned %d times)", warned)
	}
}
