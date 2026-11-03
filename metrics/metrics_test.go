package metrics

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/oekit/metrics"
)

// TestPromHandlerFor_CachesPerGatherer 钉住：同一 Gatherer 只建一次
// promhttp handler（此前 MetricsHandlerFor 每次调用都新建，无缓存），
// 且不同 Gatherer 的 handler 各取各的指标（缓存不得串味）。
func TestPromHandlerFor_CachesPerGatherer(t *testing.T) {
	regA := prometheus.NewRegistry()
	regB := prometheus.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_a_total", Help: "h"})
	regA.MustRegister(counter)

	hA := promHandlerFor(regA)
	promHandlerFor(regA)

	// 缓存命中：同一 Gatherer 的 handler 已进缓存
	cached, ok := handlerCache.Load(regA)
	if !ok {
		t.Fatal("promhttp handler must be cached per gatherer")
	}
	if reflect.ValueOf(cached.(http.Handler)).Pointer() != reflect.ValueOf(hA).Pointer() {
		t.Error("cached handler must be the one returned to the caller")
	}

	// 行为级：A 的 handler 只暴露 A 的指标
	if body := serveMetrics(t, hA); !strings.Contains(body, "test_a_total") {
		t.Errorf("handler A must expose registry A metrics, got:\n%s", body)
	}
	// B 的 handler 不得暴露 A 的指标（缓存不得串味）
	hB := promHandlerFor(regB)
	if body := serveMetrics(t, hB); strings.Contains(body, "test_a_total") {
		t.Errorf("handler B must not expose registry A metrics, got:\n%s", body)
	}
}

// serveMetrics 用 promhttp handler 渲染一次 /metrics 文本。
func serveMetrics(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

// TestHertzResponseWriter_NotFlusher 钉住：不再实现**假的** http.Flusher
// （promhttp.HandlerFor / HandlerForTransactional 全程不使用 http.Flusher；
// 声明 Flusher 却什么都不做，属假接口）。
func TestHertzResponseWriter_NotFlusher(t *testing.T) {
	c := app.NewContext(0)
	var w http.ResponseWriter = newHertzResponseWriter(c)
	if _, ok := w.(http.Flusher); ok {
		t.Error("hertzResponseWriter must not implement http.Flusher (fake flusher)")
	}
}

func TestMetricsHandler_GzipNegotiation(t *testing.T) {
	metrics.Init("metrics-test") // 注册 Go/Process 采集器，保证 registry 非空

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/metrics", MetricsHandler())

	// 携带 Accept-Encoding: gzip 的客户端应收到压缩响应，
	// 验证合成 http.Request 正确透传了内容协商头。
	resp := ut.PerformRequest(h.Engine, consts.MethodGet, "/metrics", nil,
		ut.Header{Key: "Accept-Encoding", Value: "gzip"},
	).Result()
	if resp.StatusCode() != consts.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode())
	}
	if ce := string(resp.Header.Peek("Content-Encoding")); ce != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip (header not propagated?)", ce)
	}

	// 不携带 Accept-Encoding 时不应压缩。
	resp = ut.PerformRequest(h.Engine, consts.MethodGet, "/metrics", nil).Result()
	if resp.StatusCode() != consts.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode())
	}
	if ce := string(resp.Header.Peek("Content-Encoding")); ce == "gzip" {
		t.Error("response unexpectedly gzipped without Accept-Encoding")
	}
	if len(resp.Body()) == 0 {
		t.Error("metrics body is empty")
	}
}
