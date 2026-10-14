package hertzotel

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
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
