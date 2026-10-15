// Package hertztracer 提供 Hertz HTTP client/server 的 OpenTracing
// tracing 集成 helper,桥接 hertz-contrib/tracer。跟 P2-2 kitexotel(走
// OTel SDK)+ P8-2 hertzotel(也走 OTel SDK)对称,但 hertztracer 走
// Opentracing 旧生态(jaeger/zipkin 等 SDK)。
//
// 官方依据:https://github.com/hertz-contrib/tracer + hertz 官方
// observability/tracing 教程。
//
// 业务用法(以 jaeger 为例):
//
//	import (
//	    opentracing "github.com/opentracing/opentracing-go"
//	    jaegercfg "github.com/uber/jaeger-client-go/config"
//	    hertztracer "github.com/oekit/hertz/observability/tracing/instrumentation/hertztracer"
//	)
//
//	// 1. 初始化 opentracing 全局 Tracer
//	cfg, _ := jaegercfg.FromEnv()
//	tracer, _ := cfg.InitGlobalTracer("my-service")
//	defer tracer.Close()
//
//	// 2. 启用 hertz server Opentracing span
//	h := server.Default()
//	h.Use(hertztracer.NewServerMiddleware())
//
//	// 3. 启用 hertz client Opentracing propagation
//	c, _ := client.NewClient()
//	c.Use(hertztracer.NewClientMiddleware())
//
// 设计选择:
//   - hertztracer 走 Opentracing 协议,跟 P2-2 hertzotel(OTel) 二选一
//   - 业务如果用 jaeger/zipkin SDK(老生态)→ 用 hertztracer
//   - 业务如果用 OTel SDK(新生态)→ 用 hertzotel(P2-2)
//
// 转换路径: Opentracing → OTel 可用 opentelemetry-go-contrib
// Opentracing Shim;反过来也可。本包只做 hertz 端,业务自行选。
package hertztracer

import (
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	hertztracing "github.com/cloudwego/hertz/pkg/common/tracer"
	hertzcontribtracer "github.com/hertz-contrib/tracer/hertz"
	opentracing "github.com/opentracing/opentracing-go"
)

// Option 是 hertz-contrib/tracer 透传类型占位(ekit 不引入额外 Option,
// 业务直接用 opentracing.SetGlobalTracer(...) 设全局 tracer)。
type Option struct{}

// ServerOption 是 Option 的合规命名 alias(AGENTS.md "Option 类型命名
// XxxOption");占位类型,当前 NewServerMiddleware 不消费任何选项。
type ServerOption = Option

// NewServerMiddleware 构造 hertz server Opentracing 中间件。
// 业务需先 opentracing.SetGlobalTracer(...) 初始化全局 tracer。
func NewServerMiddleware(_ ...Option) app.HandlerFunc {
	return hertzcontribtracer.ServerCtx()
}

// NewClientMiddleware 构造 hertz client Opentracing 中间件。
// 业务用法:
//
//	c, _ := client.NewClient()
//	c.Use(hertztracer.NewClientMiddleware())
func NewClientMiddleware(_ ...Option) client.Middleware {
	return hertzcontribtracer.ClientCtx
}

// NewServerTracer 透传 hertz-contrib/tracer.NewTracer,给业务直接拿
// 上游 tracer.Tracer(给 hertz 框架 `server.WithTracer(...)` 用)。
//
// 业务用法(高级):
//
//	tracer := hertztracer.NewServerTracer(
//	    opentracing.NoopTracer{},
//	    func(c *app.RequestContext) string {
//	        return "GET " + string(c.Path())
//	    },
//	)
//	h := server.New(server.WithTracer(tracer))
//	h.Use(hertztracer.NewServerMiddleware())
func NewServerTracer(t opentracing.Tracer, formOpName func(c *app.RequestContext) string) hertztracing.Tracer {
	return hertzcontribtracer.NewTracer(t, formOpName)
}
