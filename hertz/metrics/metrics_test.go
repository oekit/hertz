package metrics

import (
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/yi-nology/ekit-metrics"
)

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
