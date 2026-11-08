// Package metrics 提供 Hertz Prometheus 指标端点。
//
// 使用方式:
//
//	metrics.Init("my-service")
//	r.GET("/metrics", hmetrics.MetricsHandler())
package metrics

import (
	"context"
	"net/http"
	"reflect"
	"sync"

	"github.com/oekit/metrics"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsHandler 返回一个 Hertz HandlerFunc，用于暴露 /metrics 端点。
// 它使用全局 metrics.Registry（由 metrics.Init 注册的 Go/Process/BuildInfo 指标），
// 并将 Hertz RequestContext 适配为标准 http.ResponseWriter。
func MetricsHandler() app.HandlerFunc {
	return MetricsHandlerFor(metrics.Registry)
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

// handlerCache 缓存 promhttp handler（按 Gatherer 维度）。
//
// promhttp.HandlerFor 每次调用都新建一个 handler（含内容协商闭包）——
// 每次注册端点/每次调用都重建纯属浪费。缓存后同一 Gatherer 只建一次。
var handlerCache sync.Map // prometheus.Gatherer → http.Handler

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
		return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
	}
	if cached, ok := handlerCache.Load(gatherer); ok {
		return cached.(http.Handler)
	}
	created := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
	actual, _ := handlerCache.LoadOrStore(gatherer, created)
	return actual.(http.Handler)
}

// InitAndRegister 初始化 Prometheus 指标并注册 /metrics 端点。
// 等价于 metrics.Init(serviceName) + h.GET("/metrics", MetricsHandler())。
func InitAndRegister(serviceName string, h *server.Hertz) {
	metrics.Init(serviceName)
	h.GET("/metrics", MetricsHandler())
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
