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
	"sync/atomic"

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

// missingServerTracerWarn 是「server tracer 缺失 ⇒ 不会生成 span」的诊断出口。
// 抽成变量便于单测断言（生产指向 logx，且每进程只记一次）。
var missingServerTracerWarn = func() {
	missingServerTracerOnce.Do(func() {
		otelLog.Error("hertzotel: otel server tracer not installed - NO span will be created; " +
			"use NewServerTracing() (server option + middleware, same source) " +
			"instead of h.Use(NewServerMiddleware()) alone")
	})
}

// serverTracerCreated 是包级「已配对安装」状态：本进程内是否经
// NewServerTracer / NewServerTracing 创建过 otel server tracer。
//
// 为什么不用 c.GetTraceInfo() == nil 判「engine 未装 tracer」（#168）：hertz
// v0.10.6 起 engine 构造时 enableTrace=true，ctxPool.New 给每个
// RequestContext 预置 TraceInfo，只有 initTrace 发现**完全没有任何**
// tracer.Tracer 时才置 false——只要 engine 上还挂着任意非 otel 的 tracer
// （stats/自研 tracer 实现同一接口），GetTraceInfo() 就非 nil，该判据在
// 「混合装配」（engine 有 tracer 但缺的恰是 otel server tracer）下恒不触发，
// 而 otel carrier（由 otel serverTracer 的 Start 注入）确实缺失，正是要防的
// 「半截 span 哑火」形态。
//
// 局限（进程粒度而非 engine 粒度）：多 engine 进程里只要任意 engine 配过
// 本包 tracer，另一台裸 engine 上的哑火探测不到——carrier 类型在上游
// internal 包内，请求期无法逐 engine 验证。进程内「从未创建过 otel
// server tracer」时判据是精确的（此时任何 engine 都不可能有 otel carrier）。
var serverTracerCreated atomic.Bool

// markServerTracerCreated 置位「已配对安装」状态。只由 NewServerTracer /
// NewServerTracing 调用——NewServerMiddleware 内部也调上游 NewServerTracer
// 建中间件 cfg，但那份 Option 被丢弃（从未装上 engine），不得置位。
func markServerTracerCreated() { serverTracerCreated.Store(true) }

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
//
// 诊断判据(#168)——请求期两条任一命中即告警(每进程一次):
//   - c.GetTraceInfo() == nil:engine **零 tracer**(hertz v0.10.6 只有
//     initTrace 发现完全没有任何 tracer 时才不给 RequestContext 预置
//     TraceInfo),otel carrier 必然缺失;
//   - 进程内从未经 NewServerTracer/NewServerTracing 创建过 otel server
//     tracer(见 [serverTracerCreated]):carrier 同样必然缺失,覆盖「engine
//     挂了别的 tracer 但缺的恰是 otel tracer」的混合装配哑火——旧判据在此
//     场景恒不触发。
//
// 两条件都不满足时按「已配对安装」处理,请求期不再逐 engine 验证(见
// serverTracerCreated 的局限说明)。
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
		if c.GetTraceInfo() == nil || !serverTracerCreated.Load() {
			// server tracer 缺失 ⇒ 上游 Start 不会放入 trace carrier
			// ⇒ 中间件拿不到 carrier ⇒ 静默不产 span。显式诊断,避免哑火。
			missingServerTracerWarn()
		}
		mw(ctx, c)
	}
}

// NewServerTracer 透传上游 NewServerTracer,业务可拿到 server.Option 用
// 于 server.New(..., serverOption) 或者拿到 *HertzOtelConfig 用 ServerMiddleware。
//
// ⚠️ 返回的 Option 必须传进 server.New(装 server tracer),只 h.Use 中间件
// 不产 span。本函数同时置位包级「已配对安装」状态(serverTracerCreated),
// 供 NewServerMiddleware 的缺失诊断判据使用(#168)。
//
// 官方依据:https://github.com/hertz-contrib/obs-opentelemetry/tracing
// (tracer_server.go NewServerTracer)。
func NewServerTracer(opts ...HertzOtelOption) (serverconfig.Option, *HertzOtelConfig) {
	option, cfg := hertzcontribtracing.NewServerTracer(opts...)
	markServerTracerCreated()
	return option, cfg
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
// ⚠️ 返回值的两个字段必须**成对用满**(MustUse):Option 传给 server.New(),
// Middleware 通过 h.Use 注册——只注册 Middleware 不装 Option 不会产 span
// (span 依赖装在 server 上的 tracer 注入 carrier)。缺一形态的哑火由
// NewServerMiddleware 的缺失诊断兜底(#168,判据含进程级「已配对安装」
// 状态 serverTracerCreated,由本函数置位)。
//
// 使用方式:
//
//	tracing := hertzotel.NewServerTracing()
//	h := server.New(server.WithHostPorts(":8080"), tracing.Option)
//	h.Use(tracing.Middleware)
func NewServerTracing(opts ...HertzOtelOption) ServerTracing {
	option, cfg := hertzcontribtracing.NewServerTracer(opts...)
	markServerTracerCreated()
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
