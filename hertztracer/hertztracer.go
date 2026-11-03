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
//	    hertztracer "github.com/oekit/hertz/hertztracer"
//	)
//
//	// 1. 初始化 opentracing 全局 Tracer
//	cfg, _ := jaegercfg.FromEnv()
//	tracer, _ := cfg.InitGlobalTracer("my-service")
//	defer tracer.Close()
//
//	// 2. 启用 hertz server Opentracing span（中间件 + server tracer 同源）
//	tracing := hertztracer.NewServerTracing()
//	h := server.New(server.WithHostPorts(":8080"), tracing.Option)
//	h.Use(tracing.Middleware)
//
//	// 3. 启用 hertz client Opentracing propagation
//	c, _ := client.NewClient()
//	c.Use(hertztracer.NewClientMiddleware())
//
// 设计选择:
//   - hertztracer 走 Opentracing 协议,跟 P2-2 hertzotel(OTel) 二选一
//   - server 端 span 需「server tracer + 中间件」成对装配（见 NewServerTracing）:
//     只 h.Use 中间件而未装 server tracer 时，中间件自建 span（无 engine
//     tracer 上下文时）
//   - 业务如果用 jaeger/zipkin SDK(老生态)→ 用 hertztracer
//   - 业务如果用 OTel SDK(新生态)→ 用 hertzotel(P2-2)
//
// 转换路径: Opentracing → OTel 可用 opentelemetry-go-contrib
// Opentracing Shim;反过来也可。本包只做 hertz 端,业务自行选。
package hertztracer

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/app/server"
	serverconfig "github.com/cloudwego/hertz/pkg/common/config"
	hertztracing "github.com/cloudwego/hertz/pkg/common/tracer"
	hertzcontribtracer "github.com/hertz-contrib/tracer/hertz"
	opentracing "github.com/opentracing/opentracing-go"
)

// Option 是 hertztracer 的配置选项(函数选项模式;AGENTS.md「禁止裸 Option」)。
//
// 真实字段:tracer(OpenTracing tracer)与 span 名生成函数。
type Option func(*tracerConfig)

// ServerOption 是 Option 的历史别名(AGENTS.md「Option 类型命名 XxxOption」)。
// 保留导出以兼容既有引用;勿删。
type ServerOption = Option

// tracerConfig 是本包选项的配置载体。
type tracerConfig struct {
	tracer            opentracing.Tracer
	formOperationName func(c *app.RequestContext) string
}

// WithTracer 指定使用的 OpenTracing tracer。
// nil 时用 opentracing.GlobalTracer()(未 SetGlobalTracer 时是 noop tracer)。
func WithTracer(t opentracing.Tracer) Option {
	return func(cfg *tracerConfig) { cfg.tracer = t }
}

// WithOperationName 指定 span 名生成函数。
// nil 时用缺省 "<METHOD> <fullPath>"。
func WithOperationName(fn func(c *app.RequestContext) string) Option {
	return func(cfg *tracerConfig) { cfg.formOperationName = fn }
}

// newTracerConfig 聚合选项并填缺省值。
func newTracerConfig(opts []Option) *tracerConfig {
	cfg := &tracerConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if cfg.tracer == nil {
		cfg.tracer = opentracing.GlobalTracer()
	}
	if cfg.formOperationName == nil {
		cfg.formOperationName = defaultOperationName
	}
	return cfg
}

// defaultOperationName 是缺省 span 名:"<METHOD> <fullPath>"。
func defaultOperationName(c *app.RequestContext) string {
	return string(c.Method()) + " " + c.FullPath()
}

// NewServerMiddleware 构造 hertz server Opentracing 中间件(h.Use 注册)。
// 业务需先 opentracing.SetGlobalTracer(...) 初始化全局 tracer。
//
// opts **真正生效**:tracer 与 span 名取自 opts([WithTracer] /
// [WithOperationName])。两种装配形态都支持:
//
//   - 仅中间件(不装 server tracer):中间件自建 span(opts 生效),链路不中断;
//   - 中间件 + server tracer([NewServerTracing] / [NewServerTracer]):
//     走上游 ServerCtx(span 由 engine 的 server tracer 驱动,与上游契约一致)。
//
// ⚠️ 上游 ServerCtx 在 engine 未装 tracer 时会记录
// "get tracer container failed" 并**中断链路**(不 c.Next)——本实现改为自建
// span,链路不中断;engine 装了 tracer 时仍走上游(engine 的 Finish 依赖上游
// trace container 里的 span,必须由上游设置)。
func NewServerMiddleware(opts ...Option) app.HandlerFunc {
	cfg := newTracerConfig(opts)
	upstream := hertzcontribtracer.ServerCtx()
	return func(ctx context.Context, c *app.RequestContext) {
		if c.GetTraceInfo() != nil {
			upstream(ctx, c)
			return
		}
		span, spanCtx := opentracing.StartSpanFromContextWithTracer(ctx, cfg.tracer, cfg.formOperationName(c))
		defer span.Finish()
		c.Next(spanCtx)
	}
}

// ServerTracing 是一次成对创建的 Opentracing 组件:
// Option 传给 server.New()(安装 server tracer),Middleware 通过 h.Use 注册。
// 二者共享同一次构造(tracer 与 span 名同源),符合官方文档的标准用法。
type ServerTracing struct {
	// Option 设置 server 级别 tracer,需在 server.New() 时传入。
	Option serverconfig.Option
	// Middleware 为每个入站请求创建 span,需通过 h.Use 注册。
	Middleware app.HandlerFunc
}

// NewServerTracing 创建成对的 server tracing 组件。
//
// 用法:
//
//	tracing := hertztracer.NewServerTracing()
//	h := server.New(server.WithHostPorts(":8080"), tracing.Option)
//	h.Use(tracing.Middleware)
//
// 前提:业务已 opentracing.SetGlobalTracer(...) 初始化全局 tracer
// (或用 [WithTracer] 显式指定)。
func NewServerTracing(opts ...Option) ServerTracing {
	cfg := newTracerConfig(opts)
	tr := hertzcontribtracer.NewTracer(cfg.tracer, cfg.formOperationName)
	return ServerTracing{
		Option:     server.WithTracer(tr),
		Middleware: NewServerMiddleware(opts...),
	}
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
