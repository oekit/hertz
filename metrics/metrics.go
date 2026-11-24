// Package metrics 提供 Hertz Prometheus 指标端点。
//
// 使用方式:
//
//	metrics.Init("my-service")
//	r.GET("/metrics", hmetrics.MetricsHandler())
//
// ⚠️ 便捷入口默认只暴露 oekit metrics.Registry；注册在
// prometheus.DefaultRegisterer 上的 collector（promauto、第三方库自注册的
// collector）默认不在抓取面上，也不会报错——只是在 /metrics 上静默缺席
// （#298）。需要一并暴露时显式接入外部 Gatherer：
//
//	metrics.InitAndRegister("my-service", r,
//	    metrics.WithExtraGatherers(prometheus.DefaultGatherer))
//	// 或
//	r.GET("/metrics", metrics.MetricsHandler(
//	    metrics.WithExtraGatherers(prometheus.DefaultGatherer)))
package metrics

import (
	"context"
	"net/http"
	"reflect"
	"sync"

	"github.com/oekit/logx"
	"github.com/oekit/metrics"
	"github.com/oekit/pkg/errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// metricsLog 是本包日志门面（懒解析；AGENTS.md：SDK/基础包必须 Delegating）。
var metricsLog = logx.Delegating("hertz-metrics")

// HandlerOption 配置 /metrics 便捷入口（MetricsHandler / InitAndRegister）。
type HandlerOption func(*handlerConfig)

// handlerConfig 是便捷入口的构造期配置。
type handlerConfig struct {
	extraGatherers []prometheus.Gatherer
}

// WithExtraGatherers 让便捷入口在 oekit metrics.Registry 之外额外聚合给定
// Gatherer——典型用法是 prometheus.DefaultGatherer（promauto / 第三方库的
// 默认注册面）：
//
//	metrics.InitAndRegister("svc", h,
//	    metrics.WithExtraGatherers(prometheus.DefaultGatherer))
//
// 聚合语义（#298）：metrics.Registry 优先；额外 Gatherer 中与已出现 metric
// family **同名**的 family 整族跳过，并经 logx 每个名字告警一次。两个
// registry 天然会各带一份 go_* / process_*（client_golang 默认 registry
// 自动注册，metrics.Init 也会注册），同名冲突若原样交给
// prometheus.Gatherers，合并错误会让整个 /metrics 以 500 收场——去重后只剩
// 一份（内容等价），抓取面保持可用。
//
// 需要完全自定义抓取面（不做同名去重）时用 MetricsHandlerFor。
//
// 传入 nil Gatherer 属构造期配置错误：直接 panic（fail-fast，与
// MetricsHandlerFor(nil) 同口径），绝不让 nil 解引用推迟到每次抓取。
func WithExtraGatherers(gatherers ...prometheus.Gatherer) HandlerOption {
	return func(cfg *handlerConfig) {
		for _, g := range gatherers {
			if g == nil {
				panic("hertz/metrics: WithExtraGatherers requires non-nil gatherer")
			}
			cfg.extraGatherers = append(cfg.extraGatherers, g)
		}
	}
}

// MetricsHandler 返回一个 Hertz HandlerFunc，用于暴露 /metrics 端点。
// 它使用全局 metrics.Registry（由 metrics.Init 注册的 Go/Process/BuildInfo 指标），
// 并将 Hertz RequestContext 适配为标准 http.ResponseWriter。
//
// ⚠️ 默认抓取面**只有** metrics.Registry：注册到
// prometheus.DefaultRegisterer 的 collector（promauto 声明的 counter、
// 第三方库自注册的 collector）在 /metrics 上静默不可见（#298）。需要合并
// 时用 WithExtraGatherers：
//
//	MetricsHandler(WithExtraGatherers(prometheus.DefaultGatherer))
func MetricsHandler(opts ...HandlerOption) app.HandlerFunc {
	cfg := handlerConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if len(cfg.extraGatherers) == 0 {
		return MetricsHandlerFor(metrics.Registry)
	}
	return MetricsHandlerFor(&unionGatherer{primary: metrics.Registry, extras: cfg.extraGatherers})
}

// MetricsHandlerFor 返回一个 Hertz HandlerFunc，使用指定的 Gatherer。
// 如果需要暴露自定义的 Registry（非全局的），可以用这个函数。
//
// gatherer 为 nil 属构造期配置错误：直接 panic（fail-fast，与 nns.NewRegistry
// 等本仓口径一致），绝不让 nil 解引用推迟到每个请求上崩。
//
// 客户端的 Accept-Encoding / Accept 头会透传给 promhttp，
// 使 gzip 压缩协商与 OpenMetrics 格式协商正常生效。
func MetricsHandlerFor(gatherer prometheus.Gatherer) app.HandlerFunc {
	if gatherer == nil {
		panic("hertz/metrics: MetricsHandlerFor requires non-nil gatherer")
	}
	httpHandler := promHandlerFor(gatherer)
	return func(ctx context.Context, c *app.RequestContext) {
		writer := newHertzResponseWriter(c)
		req, err := http.NewRequestWithContext(ctx, "GET", "/metrics", nil)
		if err != nil {
			c.AbortWithStatus(500)
			return
		}
		// promhttp 依据请求头做 gzip 与 OpenMetrics 协商；
		// 合成请求不带透传则永远退化为未压缩的旧文本格式。
		for _, key := range []string{"Accept-Encoding", "Accept"} {
			if v := c.Request.Header.Peek(key); len(v) > 0 {
				req.Header.Set(key, string(v))
			}
		}
		httpHandler.ServeHTTP(writer, req)
	}
}

// unionGatherer 合并主/额外 Gatherer 的抓取面（#298）。
//
// 同名 metric family 按「主 Gatherer 优先」整族跳过额外副本，并按 family
// 名各告警一次。典型冲突是 go_* / process_*：client_golang 的默认 registry
// 与 metrics.Init 都会注册它们，family 名与标签完全相同；直接交给
// prometheus.Gatherers 会触发 duplicate-metric 合并错误，让整个 /metrics
// 以 500 收场（不是只丢冲突的那一族）。
type unionGatherer struct {
	primary prometheus.Gatherer
	extras  []prometheus.Gatherer
	// warned 记录已告警过的同名 family（每个名字只告警一次：抓取高频，
	// 不能每次请求刷日志）。
	warned sync.Map // string → struct{}
}

// Gather 实现 prometheus.Gatherer。主/额外 Gatherer 的错误都保留在返回的
// MultiError 里（不吞错）；同名 family 去重本身不产生错误。
func (g *unionGatherer) Gather() ([]*dto.MetricFamily, error) {
	primary, err := g.primary.Gather()
	var errs prometheus.MultiError
	if err != nil {
		errs = append(errs, err)
	}
	seen := make(map[string]struct{}, len(primary))
	for _, mf := range primary {
		seen[mf.GetName()] = struct{}{}
	}
	out := primary
	for i, extra := range g.extras {
		mfs, err := extra.Gather()
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "hertz/metrics: extra gatherer #%d", i+1))
		}
		for _, mf := range mfs {
			if _, dup := seen[mf.GetName()]; dup {
				g.warnDuplicate(mf.GetName())
				continue
			}
			seen[mf.GetName()] = struct{}{}
			out = append(out, mf)
		}
	}
	return out, errs.MaybeUnwrap()
}

// warnDuplicate 对每个同名 family 只告警一次。
func (g *unionGatherer) warnDuplicate(name string) {
	if _, loaded := g.warned.LoadOrStore(name, struct{}{}); !loaded {
		warnDuplicateMetricFamily(name)
	}
}

// warnDuplicateMetricFamily 是同名 family 去重的告警出口。抽成变量便于单测
// 断言（生产指向 logx；告警次数由 unionGatherer.warned 控制为每个名字一次）。
var warnDuplicateMetricFamily = func(name string) {
	metricsLog.Warn("hertz/metrics: metric family exists in both metrics.Registry and an extra gatherer - keeping the metrics.Registry copy and skipping the extra one; remove the duplicate registration if the extra copy has unique labels",
		"metric_family", name)
}

// handlerCache 缓存 promhttp handler（按 Gatherer 维度）。
//
// promhttp.HandlerFor 每次调用都新建一个 handler（含内容协商闭包）——
// 每次注册端点/每次调用都重建纯属浪费。缓存后同一 Gatherer 只建一次。
var handlerCache sync.Map // prometheus.Gatherer → http.Handler

// promHandlerOpts 是 promhttp 的统一构造参数（#228）：EnableOpenMetrics 必须
// 开启——promhttp 不开此项时可用格式集合里根本没有 OpenMetrics
// （expfmt.DefaultAcceptedFormats 不含任何 OpenMetrics 格式），客户端带
// Accept: application/openmetrics-text 一律静默回退 text/plain，godoc 承诺的
// 「OpenMetrics 格式协商正常生效」不成立（Prometheus 2.5+/3.x 抓取端默认就带
// 该 Accept，exemplar/转义语义随之丢失）。两处 HandlerFor 共用同一变量，
// 避免分叉漏改。
var promHandlerOpts = promhttp.HandlerOpts{EnableOpenMetrics: true}

// promHandlerFor 返回（并缓存）指定 Gatherer 对应的 promhttp handler。
//
// nil 防御分支保留（本函数是包内公共落点），但 MetricsHandlerFor 已在构造期
// 对 nil fail-fast（#176），正常不会走到。
//
// Gatherer 不可比较时（值类型含 slice/map 等）不能作 map key ⇒ 不缓存，
// 退化为每次新建（行为与缓存前一致）。
func promHandlerFor(gatherer prometheus.Gatherer) http.Handler {
	if gatherer == nil {
		return nil
	}
	if t := reflect.TypeOf(gatherer); t != nil && !t.Comparable() {
		return promhttp.HandlerFor(gatherer, promHandlerOpts)
	}
	if cached, ok := handlerCache.Load(gatherer); ok {
		return cached.(http.Handler)
	}
	created := promhttp.HandlerFor(gatherer, promHandlerOpts)
	actual, _ := handlerCache.LoadOrStore(gatherer, created)
	return actual.(http.Handler)
}

// InitAndRegister 初始化 Prometheus 指标并注册 /metrics 端点。
// 等价于 metrics.Init(serviceName) + h.GET("/metrics", MetricsHandler(opts...))。
//
// 默认抓取面只含 metrics.Registry；promauto / 第三方 collector 注册在
// prometheus.DefaultRegisterer 上，需显式接线（#298）：
//
//	InitAndRegister("svc", h, WithExtraGatherers(prometheus.DefaultGatherer))
func InitAndRegister(serviceName string, h *server.Hertz, opts ...HandlerOption) {
	metrics.Init(serviceName)
	h.GET("/metrics", MetricsHandler(opts...))
}

// hertzResponseWriter 将 Hertz RequestContext 适配为标准 http.ResponseWriter，
// 使 promhttp.Handler 可以直接写入 Hertz 的响应。
type hertzResponseWriter struct {
	c       *app.RequestContext
	header  http.Header
	written bool
}

func newHertzResponseWriter(c *app.RequestContext) *hertzResponseWriter {
	return &hertzResponseWriter{c: c, header: make(http.Header)}
}

func (w *hertzResponseWriter) Header() http.Header {
	return w.header
}

// flushHeaders 把 Header() 里暂存的响应头一次性倒进 hertz 响应头。
//
// 同名多值头（http.Header 契约允许 Add 追加）必须逐值保留（#176）：首个值用
// Set（覆盖掉 hertz 侧可能已有的同名头，语义明确），其余用 Add 追加——此前
// 内层循环逐值 Set，多值头被静默压成一个（与 #99 / transportx replyHeader.Add
// 同族缺陷，此处第三份）。
func (w *hertzResponseWriter) flushHeaders() {
	if w.written {
		return
	}
	for k, vs := range w.header {
		if len(vs) == 0 {
			continue
		}
		w.c.Response.Header.Set(k, vs[0])
		for _, v := range vs[1:] {
			w.c.Response.Header.Add(k, v)
		}
	}
	w.written = true
}

func (w *hertzResponseWriter) Write(data []byte) (int, error) {
	w.flushHeaders()
	w.c.Response.AppendBody(data)
	return len(data), nil
}

func (w *hertzResponseWriter) WriteHeader(statusCode int) {
	w.flushHeaders()
	w.c.Response.SetStatusCode(statusCode)
}

// ⚠️ 本类型**刻意不实现** http.Flusher：promhttp 的 HandlerFor/HandlerForTransactional
// 全程不使用 http.Flusher（promhttp/instrument_server 的 delegator 只在底层
// writer 自身实现 Flusher 时才暴露 Flush），故此前的空 Flush 实现是假接口
// （声称可流式刷新、实际什么都不做）。去掉后行为不变（响应照常写出）。
