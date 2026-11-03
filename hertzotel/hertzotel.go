// Package hertzotel 提供 Hertz HTTP client/server 的 OpenTelemetry tracing
// 集成 helper,桥接 hertz-contrib/obs-opentelemetry/tracing。
//
// 跟 P2-2 kitexotel 对称(那一个桥接 kitex RPC,本包桥接 hertz HTTP)。
// 设计:
//   - NewServerMiddleware(opts ...HertzOtelOption) 返回 hertz server 中间件,
//     为每个请求启 root span,记录 method/path/status_code 等属性。
//     ⚠️ span 依赖 server tracer(装在 server 上),只 h.Use 中间件不产 span。
//   - NewServerTracing(opts ...) 成对返回 server.Option(装 server tracer)
//   - 中间件,二者同源——server 端启 span 的正确入口。
//   - NewClientMiddleware(opts ...HertzOtelOption) 返回 hertz client 中间件,
//     把当前 ctx 的 SpanContext 透传到下游请求 header,Otel propagator
//     用的是 otel.GetTextMapPropagator()(默认 W3C TraceContext + Baggage)。
//   - NewServerTracer(opts ...) 透传上游的 NewServerTracer,业务可拿
//     server.Option 用于 server.New(...) 或拿 *HertzOtelConfig 用于 ServerMiddleware。
//
// 集成方式:
//
//	import "github.com/oekit/tracing"
//	import hertzotel "github.com/oekit/hertz/hertzotel"
//
//	func main() {
//	    // 1. 初始化全局 OTel tracer + propagator
//	    tracing.Init(tracing.Config{ServiceName: "my-svc", Endpoint: "otel:4317"})
//
//	    // 2. 启用 hertz server 端 OTel span(方式 A: server option + middleware 同源)
//	    //    ⚠️ 只 h.Use(NewServerMiddleware(...)) 不会产生 span——server tracer
//	    //    必须同时装在 server 上(见 NewServerTracing)。
//	    tracing := hertzotel.NewServerTracing(
//	        hertzotel.WithShouldIgnore(func(ctx context.Context, c *app.RequestContext) bool {
//	            return string(c.Path()) == "/healthz"
//	        }),
//	    )
//	    h := server.New(server.WithHostPorts(":8080"), tracing.Option)
//	    h.Use(tracing.Middleware)
//
//	    // 3. 启用 hertz client 端 OTel propagation
//	    c, _ := client.NewClient()
//	    c.Use(hertzotel.NewClientMiddleware())
//	}
//
// 官方依据:
//   - https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/observability/
//   - https://github.com/hertz-contrib/obs-opentelemetry
//   - https://github.com/cloudwego/hertz#extensions
package hertzotel

import (
	"context"
	"sync"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	serverconfig "github.com/cloudwego/hertz/pkg/common/config"
	hertzcontribtracing "github.com/hertz-contrib/obs-opentelemetry/tracing"
	"github.com/oekit/logx"
)

// otelLog 是本包日志门面（懒解析；AGENTS.md：SDK/基础包必须 Delegating）。
var otelLog = logx.Delegating("hertzotel")

// missingServerTracerOnce 保证诊断每进程只记一次（避免刷屏）。
var missingServerTracerOnce sync.Once

// missingServerTracerWarn 是「engine 未装 server tracer ⇒ 不会生成 span」的
// 诊断出口。抽成变量便于单测断言（生产指向 logx，且每进程只记一次）。
var missingServerTracerWarn = func() {
	missingServerTracerOnce.Do(func() {
		otelLog.Error("hertzotel: server tracer not installed - NO span will be created; " +
			"use NewServerTracing() (server option + middleware, same source) " +
			"instead of h.Use(NewServerMiddleware()) alone")
	})
}

// HertzOtelOption 是 hertz-contrib/obs-opentelemetry/tracing 的 Option 别名,直
// 接透传,ekit 不再包装一层(避免类型不兼容导致业务无法混合传
// 入上下游两个库的 Option)。
type HertzOtelOption = hertzcontribtracing.Option

// HertzOtelConfig 是 hertz-contrib/obs-opentelemetry/tracing 的 Config 别名,供
// NewServerTracer 第二个返回值。
type HertzOtelConfig = hertzcontribtracing.Config

// NewServerMiddleware 构造 hertz server OTel 中间件(handler 形式,直接
// h.Use 即可)。
//
// 内部走 hertz-contrib/obs-opentelemetry 的 NewServerTracer + ServerMiddleware,
// 因为上游 server API 拆成两步:NewServerTracer 负责建 cfg,ServerMiddleware
// 负责把 cfg 装成 app.HandlerFunc。
//
// ⚠️ **只 h.Use 本中间件不会生成 span**:上游的 span 依赖 server tracer 的
// Start 把 OTel trace carrier 放进请求 ctx,中间件从 ctx 取 carrier 造 span;
// server tracer 是装在 **server** 上的(server.WithTracer),不是中间件里。
// carrier 类型在上游 internal 包内,本包无法自造 ⇒ 缺 server tracer 时只能
// 显式诊断,不能自建 span(半截 span 比没有更容易误导看板)。
// 本函数检测到 engine 未装 server tracer 时记一条诊断日志(每进程一次)。
//
// 正确用法(option 与 middleware 必须同源,见 [NewServerTracing]):
//
//	tracing := hertzotel.NewServerTracing()
//	h := server.New(server.WithHostPorts(":8080"), tracing.Option)
//	h.Use(tracing.Middleware)
func NewServerMiddleware(opts ...HertzOtelOption) app.HandlerFunc {
	_, cfg := hertzcontribtracing.NewServerTracer(opts...)
	mw := hertzcontribtracing.ServerMiddleware(cfg)
	return func(ctx context.Context, c *app.RequestContext) {
		if c.GetTraceInfo() == nil {
			// engine 未装任何 tracer ⇒ 上游 Start 不会放入 trace carrier
			// ⇒ 中间件拿不到 carrier ⇒ 静默不产 span。显式诊断,避免哑火。
			missingServerTracerWarn()
		}
		mw(ctx, c)
	}
}

// NewServerTracer 透传上游 NewServerTracer,业务可拿到 server.Option 用
// 于 server.New(..., serverOption) 或者拿到 *HertzOtelConfig 用 ServerMiddleware。
//
// 官方依据:https://github.com/hertz-contrib/obs-opentelemetry/tracing
// (tracer_server.go NewServerTracer)。
func NewServerTracer(opts ...HertzOtelOption) (serverconfig.Option, *HertzOtelConfig) {
	return hertzcontribtracing.NewServerTracer(opts...)
}

// NewClientMiddleware 构造 hertz client OTel 中间件(直接透传)。
func NewClientMiddleware(opts ...HertzOtelOption) client.Middleware {
	return hertzcontribtracing.ClientMiddleware(opts...)
}

// ServerTracing 是一次成对创建的 Hertz OTel tracing 组件:
// Option 传给 server.New(),Middleware 通过 h.Use 注册。
// 二者共享同一次 NewServerTracer() 创建的 tracer/Config,符合官方
// 文档的标准用法(option 与 middleware 必须同源)。
type ServerTracing struct {
	// Option 设置 server 级别 TracerProvider,需在 server.New() 时传入。
	Option serverconfig.Option
	// Middleware 为每个入站请求创建 span,需通过 h.Use 注册。
	Middleware app.HandlerFunc
}

// NewServerTracing 创建成对的 server tracing 组件。
//
// 前提:ekit [tracing.Init] 已在 bootstrap 阶段调用,OTel 全局
// TracerProvider/Propagator 已设置。
//
// 使用方式:
//
//	tracing := hertzotel.NewServerTracing()
//	h := server.New(server.WithHostPorts(":8080"), tracing.Option)
//	h.Use(tracing.Middleware)
func NewServerTracing(opts ...HertzOtelOption) ServerTracing {
	option, cfg := hertzcontribtracing.NewServerTracer(opts...)
	return ServerTracing{
		Option:     option,
		Middleware: hertzcontribtracing.ServerMiddleware(cfg),
	}
}

// 透传常用 HertzOtelOption:
//
//	import hertzcontribtracing "github.com/hertz-contrib/obs-opentelemetry/tracing"
//
//	h.Use(hertzotel.NewServerMiddleware(
//	    hertzcontribtracing.WithShouldIgnore(func(ctx context.Context, c *app.RequestContext) bool {
//	        return string(c.Path()) == "/healthz"
//	    }),
//	))
//
// HertzOtelOption 类型用 type alias 透传,业务可直接 import 上游 HertzOtelOption 也可通
// 过 type alias 引用,ekit 不重复包装(避免类型不兼容)。
