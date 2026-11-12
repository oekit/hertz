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
//	// 3. 启用 hertz client Opentracing span + 上下文注入（client span 让
//	//    出站调用在 trace 里可见；注入的 span 上下文让下游 ServerCtx 接续
//	//    父 span，不再另起根）
//	c, _ := client.NewClient()
//	c.Use(hertztracer.NewClientMiddleware())
//
// 设计选择:
//   - hertztracer 走 Opentracing 协议,跟 P2-2 hertzotel(OTel) 二选一
//   - server 端 span 需「server tracer + 中间件」成对装配（见 NewServerTracing）:
//     只 h.Use 中间件而未装 server tracer 时，中间件自建 span（无 engine
//     tracer 上下文时）
//   - client 端中间件自建 span 并按上游 ServerCtx 的接续协议注入 span 上下文
//     （#177）：上游 ClientCtx 只透传 metainfo 不建 span，本包补齐（与 server
//     侧「上游不给的能力由本包补上」同口径）
//   - 业务如果用 jaeger/zipkin SDK(老生态)→ 用 hertztracer
//   - 业务如果用 OTel SDK(新生态)→ 用 hertzotel(P2-2)
//
// 转换路径: Opentracing → OTel 可用 opentelemetry-go-contrib
// Opentracing Shim;反过来也可。本包只做 hertz 端,业务自行选。
package hertztracer

import (
	"bytes"
	"context"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/app/server"
	serverconfig "github.com/cloudwego/hertz/pkg/common/config"
	hertztracing "github.com/cloudwego/hertz/pkg/common/tracer"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/hertz-contrib/tracer/common"
	hertzcontribtracer "github.com/hertz-contrib/tracer/hertz"
	opentracing "github.com/opentracing/opentracing-go"
	opentracinglog "github.com/opentracing/opentracing-go/log"
)

// TracerOption 是 hertztracer 的配置选项(函数选项模式)。
//
// 命名红线(#247):此前导出裸 Option——AGENTS.md 明文「函数选项模式
// XxxOption(禁止裸 Option,多包 co-import 冲突)」,而 hertztracer 与
// cloudwego/hertz 的公共配置包(pkg/common/config,同样导出 Option)同属一个
// hertz 应用的常见组合,消费方 dot-import 两个包直接编译失败。改名 TracerOption;
// 旧裸名不保留兼容别名——别名本身就是上述冲突的根因(裸名一经导出,名字即占位,
// 与是否被引用无关),保留等于保留缺陷。0.x 阶段 minor 允许 breaking(AGENTS.md);
// 既有 ServerOption 别名保留,别名方向反转指向新名。
//
// 真实字段:tracer(OpenTracing tracer)与 span 名生成函数。
type TracerOption func(*tracerConfig)

// ServerOption 是 TracerOption 的历史别名(保留导出以兼容既有引用;勿删)。
type ServerOption = TracerOption

// tracerConfig 是本包选项的配置载体。
type tracerConfig struct {
	tracer            opentracing.Tracer
	formOperationName func(c *app.RequestContext) string
	// clientOperationName 是 client 侧 span 名生成函数（[WithClientOperationName]；
	// client 侧没有 *app.RequestContext，server 的 formOperationName 无法复用）。
	clientOperationName func(req *protocol.Request) string
}

// WithTracer 指定使用的 OpenTracing tracer。
// nil 时用 opentracing.GlobalTracer()(未 SetGlobalTracer 时是 noop tracer)。
func WithTracer(t opentracing.Tracer) TracerOption {
	return func(cfg *tracerConfig) { cfg.tracer = t }
}

// WithOperationName 指定 span 名生成函数。
// nil 时用缺省 "<METHOD> <fullPath>"。
func WithOperationName(fn func(c *app.RequestContext) string) TracerOption {
	return func(cfg *tracerConfig) { cfg.formOperationName = fn }
}

// WithClientOperationName 指定 client 中间件的 span 名生成函数
// （server 侧的 [WithOperationName] 形参是 *app.RequestContext，client 侧
// 只有 *protocol.Request，二者无法混用）。nil 时用缺省 "<METHOD> <path>"。
func WithClientOperationName(fn func(req *protocol.Request) string) TracerOption {
	return func(cfg *tracerConfig) { cfg.clientOperationName = fn }
}

// newTracerConfig 聚合选项并填缺省值。
func newTracerConfig(opts []TracerOption) *tracerConfig {
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
	if cfg.clientOperationName == nil {
		cfg.clientOperationName = defaultClientOperationName
	}
	return cfg
}

// defaultOperationName 是缺省 span 名:"<METHOD> <fullPath>"。
func defaultOperationName(c *app.RequestContext) string {
	return string(c.Method()) + " " + c.FullPath()
}

// defaultClientOperationName 是 client 中间件的缺省 span 名:"<METHOD> <path>"。
func defaultClientOperationName(req *protocol.Request) string {
	return string(req.Method()) + " " + string(req.Path())
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
func NewServerMiddleware(opts ...TracerOption) app.HandlerFunc {
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
func NewServerTracing(opts ...TracerOption) ServerTracing {
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
//
// opts **真正生效**：tracer 与 span 名取自 opts（[WithTracer] /
// [WithClientOperationName]）。
//
// 本实现与 [NewServerMiddleware]「自建 span」的口径一致（#177）——上游
// hertz-contrib/tracer 的 ClientCtx 只做 metainfo → HTTP 头透传，不建 span、
// 不注入 trace 上下文（出站调用在 trace 里不可见、下游 ServerCtx 因查不到
// span 上下文另起根 span）；上游 README 的完整客户端装配是
// ClientTraceMW + ClientCtx 两件套，本中间件把两件事合成一件：
//
//  1. 用配置的 tracer 创建 **client span**（出站调用在 trace 里可见，
//     server span 与下游 span 之间的空档补上），调用失败打 error tag + log；
//  2. 把 span 上下文按上游 ServerCtx 的接续协议注入：Inject(opentracing.Binary)
//     进 metainfo 键 common.SpanContextLabel（"JAEGERSPANCONTEXT"），再经
//     metainfo.ToHTTPHeader 写进请求头——下游 hertztracer/hertz-contrib 的
//     ServerCtx 从该键 Extract 出父 span（ChildOf），不再另起根 span；
//  3. 业务 metainfo 透传（上游 ClientCtx 的既有行为）原样保留。
//
// ⚠️ 注入值经 HTTP 头传播，metainfo.ToHTTPHeader 会丢弃非合法头字段值的内容
// （含控制字节的 Binary 载荷，如 jaeger-client-go 的 protobuf 载体）——这是
// 上游 ClientTraceMW + ClientCtx 装配固有的约束，非本包引入；jaeger/zipkin
// SDK 的 Binary 载体可配置文本安全形态，或用 [NewServerTracer] 侧同源的
// tracer 时优先验证一次端到端链路。
func NewClientMiddleware(opts ...TracerOption) client.Middleware {
	cfg := newTracerConfig(opts)
	return func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) (err error) {
			if ctx == nil {
				ctx = context.Background()
			}
			span, spanCtx := opentracing.StartSpanFromContextWithTracer(ctx, cfg.tracer, cfg.clientOperationName(req))
			defer span.Finish()

			// 注入（下游 ServerCtx 读的就是这个 metainfo 键）；Inject 失败
			// 只跳过注入，不中断调用（span 照常产生、Finish 照常执行）。
			// ⚠️ 必须走 transient（metainfo.WithValue，与上游 ClientTraceMW
			// 一致）：GetValue 只检索 transient/stale，persistent 值它读不到。
			var buf bytes.Buffer
			if injectErr := cfg.tracer.Inject(span.Context(), opentracing.Binary, &buf); injectErr == nil {
				spanCtx = metainfo.WithValue(spanCtx, common.SpanContextLabel, buf.String())
			}
			// metainfo → 请求头：带上注入的 span 上下文与业务 metainfo
			// （上游 ClientCtx 行为保留）。
			metainfo.ToHTTPHeader(spanCtx, &req.Header)

			if err = next(spanCtx, req, resp); err != nil {
				span.SetTag("error", true)
				span.LogFields(opentracinglog.Error(err))
			}
			return err
		}
	}
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
