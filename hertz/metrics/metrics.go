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

	"github.com/yi-nology/ekit-metrics"

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
// 客户端的 Accept-Encoding / Accept 头会透传给 promhttp，
// 使 gzip 压缩协商与 OpenMetrics 格式协商正常生效。
func MetricsHandlerFor(gatherer prometheus.Gatherer) app.HandlerFunc {
	httpHandler := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
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

func (w *hertzResponseWriter) flushHeaders() {
	if w.written {
		return
	}
	for k, vs := range w.header {
		for _, v := range vs {
			w.c.Response.Header.Set(k, v)
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

// Flush 实现 http.Flusher 接口，支持 promhttp 流式响应。
func (w *hertzResponseWriter) Flush() {}
