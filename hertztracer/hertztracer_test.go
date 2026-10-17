package hertztracer

import (
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/app/server"
	opentracing "github.com/opentracing/opentracing-go"
)

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
