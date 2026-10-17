package health

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/oekit/hertz/versionh"
	"github.com/oekit/tracing/traceid"
)

// envelope 用于断言统一响应外壳 {code, data, message, trace_id}。
type envelope struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	TraceID string          `json:"trace_id"`
}

func mustUnmarshalEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("response body is not a unified envelope: %v (body=%s)", err, body)
	}
	return e
}

func TestPingHandler(t *testing.T) {
	ctx := context.Background()
	c := app.NewContext(0)

	PingHandler(ctx, c)

	if c.Response.StatusCode() != consts.StatusOK {
		t.Errorf("expected status 200, got %d", c.Response.StatusCode())
	}
}

// TestPingHandler_EnvelopeShape 验证 /ping 返回统一响应外壳
// {"code":0,"data":null,"message":"pong","trace_id":"..."}。
func TestPingHandler_EnvelopeShape(t *testing.T) {
	ctx := context.Background()
	c := app.NewContext(0)
	c.Set(traceid.Key, "abc123def456")

	PingHandler(ctx, c)

	e := mustUnmarshalEnvelope(t, c.Response.Body())
	if e.Code != 0 {
		t.Errorf("code = %d, want 0", e.Code)
	}
	if e.Message != "pong" {
		t.Errorf("message = %q, want %q", e.Message, "pong")
	}
	if e.TraceID != "abc123def456" {
		t.Errorf("trace_id = %q, want %q", e.TraceID, "abc123def456")
	}
	if string(e.Data) != "null" {
		t.Errorf("data = %s, want null", e.Data)
	}
}

func TestNewHealthHandler_AllCheckersPass(t *testing.T) {
	checker1 := NamedChecker("check1", func(ctx context.Context) error {
		return nil
	})
	checker2 := NamedChecker("check2", func(ctx context.Context) error {
		return nil
	})

	handler := NewHealthHandler(WithCheckers(checker1, checker2))

	ctx := context.Background()
	c := app.NewContext(0)

	handler(ctx, c)

	if c.Response.StatusCode() != consts.StatusOK {
		t.Errorf("expected status 200, got %d", c.Response.StatusCode())
	}
}

// TestNewHealthHandler_SomeCheckerFails verifies the 503 path keeps the
// unified envelope shape (code=503 in body).
func TestNewHealthHandler_SomeCheckerFails(t *testing.T) {
	checker1 := NamedChecker("check1", func(ctx context.Context) error {
		return nil
	})
	checker2 := NamedChecker("check2", func(ctx context.Context) error {
		return errors.New("connection refused")
	})

	handler := NewHealthHandler(WithCheckers(checker1, checker2))

	ctx := context.Background()
	c := app.NewContext(0)

	handler(ctx, c)

	if c.Response.StatusCode() != consts.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", c.Response.StatusCode())
	}

	e := mustUnmarshalEnvelope(t, c.Response.Body())
	if e.Code != 503 {
		t.Errorf("body code = %d, want 503", e.Code)
	}
	if e.Message != "service unhealthy" {
		t.Errorf("message = %q, want %q", e.Message, "service unhealthy")
	}

	var data struct {
		Status string `json:"status"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Status != "error" {
		t.Errorf("data.status = %q, want %q", data.Status, "error")
	}
	if len(data.Checks) != 2 || data.Checks[1].Status != "error" {
		t.Errorf("unexpected checks: %+v", data.Checks)
	}
}

// TestBuildVersionHandler_EnvelopeShape 验证 /buildVersion 的统一响应外壳
// 与版本字段（经 versionh.Handler 注册，响应结构一致）。
func TestBuildVersionHandler_EnvelopeShape(t *testing.T) {
	ctx := context.Background()
	c := app.NewContext(0)

	versionh.Handler(ctx, c)

	if c.Response.StatusCode() != consts.StatusOK {
		t.Fatalf("expected status 200, got %d", c.Response.StatusCode())
	}

	e := mustUnmarshalEnvelope(t, c.Response.Body())
	if e.Code != 0 {
		t.Errorf("code = %d, want 0", e.Code)
	}
	if e.Message != "ok" {
		t.Errorf("message = %q, want %q", e.Message, "ok")
	}

	var data struct {
		Version     string `json:"version"`
		BuildCommit string `json:"buildCommit"`
		BuildTime   string `json:"buildTime"`
		BuildBranch string `json:"buildBranch"`
		StartTime   string `json:"startTime"`
		Uptime      string `json:"uptime"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v (data=%s)", err, e.Data)
	}
	if data.Version == "" {
		t.Error("expected non-empty version in data")
	}
	if _, err := time.Parse(time.RFC3339, data.StartTime); err != nil {
		t.Errorf("startTime not RFC3339: %v", err)
	}
}

// TestRegisterHealthRoutes_Routes 端到端验证默认路由注册与统一响应外壳：
// GET /ping、/health、/buildVersion 均返回 {code, data, message, trace_id}。
func TestRegisterHealthRoutes_Routes(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	RegisterHealthRoutes(h,
		WithCheckers(NamedChecker("always-ok", func(ctx context.Context) error { return nil })),
	)

	// /ping
	resp := ut.PerformRequest(h.Engine, consts.MethodGet, "/ping", nil).Result()
	if resp.StatusCode() != consts.StatusOK {
		t.Errorf("GET /ping status = %d, want 200", resp.StatusCode())
	}
	if e := mustUnmarshalEnvelope(t, resp.Body()); e.Code != 0 || e.Message != "pong" {
		t.Errorf("GET /ping envelope = %+v", e)
	}

	// /health（全部通过）
	resp = ut.PerformRequest(h.Engine, consts.MethodGet, "/health", nil).Result()
	if resp.StatusCode() != consts.StatusOK {
		t.Errorf("GET /health status = %d, want 200", resp.StatusCode())
	}
	if e := mustUnmarshalEnvelope(t, resp.Body()); e.Code != 0 || e.Message != "success" {
		t.Errorf("GET /health envelope = %+v", e)
	}

	// /buildVersion
	resp = ut.PerformRequest(h.Engine, consts.MethodGet, "/buildVersion", nil).Result()
	if resp.StatusCode() != consts.StatusOK {
		t.Errorf("GET /buildVersion status = %d, want 200", resp.StatusCode())
	}
	if e := mustUnmarshalEnvelope(t, resp.Body()); e.Code != 0 || e.Message != "ok" {
		t.Errorf("GET /buildVersion envelope = %+v", e)
	}

	// 自定义路径
	h2 := server.New(server.WithHostPorts("127.0.0.1:0"))
	RegisterHealthRoutes(h2, WithPingPath("/livez"), WithHealthPath("/readyz"), WithVersionPath("/version"))
	for _, path := range []string{"/livez", "/readyz", "/version"} {
		resp := ut.PerformRequest(h2.Engine, consts.MethodGet, path, nil).Result()
		if resp.StatusCode() != consts.StatusOK {
			t.Errorf("GET %s status = %d, want 200 (custom path not registered?)", path, resp.StatusCode())
		}
	}
}

func TestNamedChecker(t *testing.T) {
	called := false
	checker := NamedChecker("my-checker", func(ctx context.Context) error {
		called = true
		return nil
	})

	if checker.Name != "my-checker" {
		t.Errorf("expected name 'my-checker', got '%s'", checker.Name)
	}

	// Test via public API (NewHealthHandler)
	handler := NewHealthHandler(WithCheckers(checker))
	ctx := context.Background()
	c := app.NewContext(0)
	handler(ctx, c)

	if c.Response.StatusCode() != consts.StatusOK {
		t.Errorf("expected status 200, got %d", c.Response.StatusCode())
	}
	if !called {
		t.Error("expected checker to be called")
	}
}

// TestNewHealthHandler_ParallelExecution 验证检查并发执行：
// 两个各需 100ms 的检查总耗时应接近 100ms 而非串行的 200ms。
func TestNewHealthHandler_ParallelExecution(t *testing.T) {
	slow := func(ctx context.Context) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	}
	c1 := NamedChecker("slow1", slow)
	c2 := NamedChecker("slow2", slow)

	handler := NewHealthHandler(WithCheckers(c1, c2))

	start := time.Now()
	handler(context.Background(), app.NewContext(0))
	elapsed := time.Since(start)

	if elapsed >= 200*time.Millisecond {
		t.Errorf("checks appear to run serially: elapsed = %v, want < 200ms", elapsed)
	}
}

// TestNewHealthHandler_CheckTimeout 验证总超时硬截止：
// 不感知 context 取消的慢 checker 也会在超时后被收割为 timeout，返回 503。
func TestNewHealthHandler_CheckTimeout(t *testing.T) {
	hung := NamedChecker("hung", func(ctx context.Context) error {
		time.Sleep(2 * time.Second) // 故意不感知 ctx
		return nil
	})
	ok := NamedChecker("ok", func(ctx context.Context) error { return nil })

	handler := NewHealthHandler(WithCheckers(hung, ok), WithCheckTimeout(50*time.Millisecond))

	start := time.Now()
	c := app.NewContext(0)
	handler(context.Background(), c)
	elapsed := time.Since(start)

	if c.Response.StatusCode() != consts.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", c.Response.StatusCode())
	}
	if elapsed >= 2*time.Second {
		t.Errorf("timeout not enforced: elapsed = %v, want ~50ms", elapsed)
	}

	e := mustUnmarshalEnvelope(t, c.Response.Body())
	var data struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v (data=%s)", err, e.Data)
	}
	if len(data.Checks) != 2 || data.Checks[0].Error != "timeout" {
		t.Errorf("unexpected checks: %+v", data.Checks)
	}
	if data.Checks[1].Status != "ok" {
		t.Errorf("healthy checker should be ok: %+v", data.Checks[1])
	}
}

// TestNewHealthHandler_ResultOrderStable 验证并发执行后结果仍按注册顺序输出。
func TestNewHealthHandler_ResultOrderStable(t *testing.T) {
	names := []string{"a", "b", "c", "d"}
	checkers := make([]*Checker, 0, len(names))
	for _, n := range names {
		n := n
		checkers = append(checkers, NamedChecker(n, func(ctx context.Context) error {
			time.Sleep(time.Duration(len(n)%2) * 5 * time.Millisecond)
			return nil
		}))
	}

	handler := NewHealthHandler(WithCheckers(checkers...))
	c := app.NewContext(0)
	handler(context.Background(), c)

	e := mustUnmarshalEnvelope(t, c.Response.Body())
	var data struct {
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if len(data.Checks) != len(names) {
		t.Fatalf("checks = %d, want %d", len(data.Checks), len(names))
	}
	for i, want := range names {
		if data.Checks[i].Name != want {
			t.Errorf("checks[%d].name = %q, want %q (order not stable)", i, data.Checks[i].Name, want)
		}
	}
}

// TestNewHealthHandler_CheckerPanic_ErrorResult 回归：checkTimeout<=0 路径
// 依赖"每个 checker 恰好投递一次结果"，checker panic 曾跳过投递让 <-ch
// 永久阻塞（/health 挂死）。钉住：panic 就地收编为 error 结果，503 正常返回。
func TestNewHealthHandler_CheckerPanic_ErrorResult(t *testing.T) {
	bad := NamedChecker("panicky", func(ctx context.Context) error {
		panic("checker boom")
	})
	ok := NamedChecker("healthy", func(ctx context.Context) error { return nil })

	handler := NewHealthHandler(WithCheckers(bad, ok))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c := app.NewContext(0)

	done := make(chan struct{})
	go func() { handler(ctx, c); close(done) }()

	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("handler blocked: checker panic did not deliver a result")
	}

	if c.Response.StatusCode() != consts.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (one checker panicked)", c.Response.StatusCode())
	}
	e := mustUnmarshalEnvelope(t, c.Response.Body())
	if !strings.Contains(string(e.Data), "panicky") {
		t.Errorf("results should mention panicking checker, got %s", string(e.Data))
	}
}

// ==================== Degraded 软依赖降级（#6） ====================

// TestNewHealthHandler_DegradedOnly 软依赖降级：仅 degraded 无硬失败时
// HTTP 仍 200（就绪不失败），data.status="degraded" 供告警，Error 填充。
func TestNewHealthHandler_DegradedOnly(t *testing.T) {
	soft := NamedChecker("cache", func(ctx context.Context) error {
		return Degraded(errors.New("redis timeout"))
	})
	hard := NamedChecker("db", func(ctx context.Context) error { return nil })

	handler := NewHealthHandler(WithCheckers(soft, hard))
	c := app.NewContext(0)
	handler(context.Background(), c)

	if c.Response.StatusCode() != consts.StatusOK {
		t.Errorf("status = %d, want 200 (degraded must not fail readiness)", c.Response.StatusCode())
	}
	e := mustUnmarshalEnvelope(t, c.Response.Body())
	if e.Code != 0 {
		t.Errorf("body code = %d, want 0", e.Code)
	}
	if e.Message != "service degraded" {
		t.Errorf("message = %q, want %q", e.Message, "service degraded")
	}

	var data struct {
		Status string `json:"status"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Status != "degraded" {
		t.Errorf("data.status = %q, want %q", data.Status, "degraded")
	}
	if data.Checks[0].Status != "degraded" {
		t.Errorf("checks[0].status = %q, want %q", data.Checks[0].Status, "degraded")
	}
	if data.Checks[0].Error == "" {
		t.Error("degraded check should fill Error field")
	}
	if data.Checks[1].Status != "ok" {
		t.Errorf("checks[1].status = %q, want %q", data.Checks[1].Status, "ok")
	}
}

// TestNewHealthHandler_DegradedPlusError 硬失败压过降级：任一 error → 503。
func TestNewHealthHandler_DegradedPlusError(t *testing.T) {
	soft := NamedChecker("cache", func(ctx context.Context) error {
		return Degraded(errors.New("cache down"))
	})
	hard := NamedChecker("db", func(ctx context.Context) error {
		return errors.New("db down")
	})

	handler := NewHealthHandler(WithCheckers(soft, hard))
	c := app.NewContext(0)
	handler(context.Background(), c)

	if c.Response.StatusCode() != consts.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", c.Response.StatusCode())
	}
	e := mustUnmarshalEnvelope(t, c.Response.Body())
	var data struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Status != "error" {
		t.Errorf("data.status = %q, want %q", data.Status, "error")
	}
}

// TestNewHealthHandler_DegradedUnwrap Degraded(err) 支持 errors.Unwrap 链，
// 且不因总超时被误改判为 timeout。
func TestNewHealthHandler_DegradedUnwrap(t *testing.T) {
	base := errors.New("connection refused")
	if !errors.Is(Degraded(base), base) {
		t.Error("Degraded should wrap via Unwrap so errors.Is works")
	}
	var deg *DegradedError
	if !errors.As(Degraded(base), &deg) {
		t.Fatal("errors.As should find DegradedError")
	}
	if deg.Unwrap() != base {
		t.Errorf("Unwrap = %v, want %v", deg.Unwrap(), base)
	}

	// 总超时窗口内 checker 返回 Degraded：仍记 degraded，不改判 timeout
	handler := NewHealthHandler(
		WithCheckers(NamedChecker("soft", func(ctx context.Context) error {
			return Degraded(errors.New("soft down"))
		})),
		WithCheckTimeout(time.Second),
	)
	c := app.NewContext(0)
	handler(context.Background(), c)
	e := mustUnmarshalEnvelope(t, c.Response.Body())
	var data struct {
		Checks []struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Checks[0].Status != "degraded" {
		t.Errorf("status = %q, want degraded (soft-dep semantics win over timeout)", data.Checks[0].Status)
	}
}

// ==================== VerboseErrors 错误脱敏（#6） ====================

// TestNewHealthHandler_ErrorRedactedByDefault 默认脱敏：Error = "check failed"，
// 不暴露底层 err.Error()（防连接串/凭据经 /health 泄漏）。
func TestNewHealthHandler_ErrorRedactedByDefault(t *testing.T) {
	handler := NewHealthHandler(WithCheckers(
		NamedChecker("db", func(ctx context.Context) error {
			return errors.New("dial tcp://user:secret@10.0.0.1:6379: connect refused")
		}),
	))
	c := app.NewContext(0)
	handler(context.Background(), c)

	e := mustUnmarshalEnvelope(t, c.Response.Body())
	var data struct {
		Checks []struct {
			Error string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Checks[0].Error != "check failed" {
		t.Errorf("Error = %q, want %q (must be redacted by default)", data.Checks[0].Error, "check failed")
	}
	if strings.Contains(string(e.Data), "secret") {
		t.Errorf("verbose-off must not leak err.Error(), got %s", e.Data)
	}
}

// TestNewHealthHandler_VerboseErrors WithVerboseErrors() 开启后
// CheckResult.Error 暴露 err.Error()。
func TestNewHealthHandler_VerboseErrors(t *testing.T) {
	handler := NewHealthHandler(
		WithCheckers(NamedChecker("db", func(ctx context.Context) error {
			return errors.New("dial tcp: connection refused")
		})),
		WithVerboseErrors(),
	)
	c := app.NewContext(0)
	handler(context.Background(), c)

	e := mustUnmarshalEnvelope(t, c.Response.Body())
	var data struct {
		Checks []struct {
			Error string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Checks[0].Error != "dial tcp: connection refused" {
		t.Errorf("Error = %q, want %q (verbose should expose err.Error())",
			data.Checks[0].Error, "dial tcp: connection refused")
	}
}

// TestNewHealthHandler_VerboseDegraded verbose 开启时 degraded 也透出底层错误。
func TestNewHealthHandler_VerboseDegraded(t *testing.T) {
	handler := NewHealthHandler(
		WithCheckers(NamedChecker("cache", func(ctx context.Context) error {
			return Degraded(errors.New("cache miss rate high"))
		})),
		WithVerboseErrors(),
	)
	c := app.NewContext(0)
	handler(context.Background(), c)

	e := mustUnmarshalEnvelope(t, c.Response.Body())
	var data struct {
		Checks []struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Checks[0].Status != "degraded" {
		t.Errorf("status = %q, want degraded", data.Checks[0].Status)
	}
	if data.Checks[0].Error != "cache miss rate high" {
		t.Errorf("Error = %q, want %q", data.Checks[0].Error, "cache miss rate high")
	}
}

// TestNewHealthHandler_TimeoutStillRedacted timeout 文案不随 verbose 改变
// （本身就是脱敏标记，不携带底层细节）。
func TestNewHealthHandler_TimeoutStillRedacted(t *testing.T) {
	hung := NamedChecker("hung", func(ctx context.Context) error {
		time.Sleep(2 * time.Second)
		return nil
	})
	handler := NewHealthHandler(
		WithCheckers(hung),
		WithCheckTimeout(50*time.Millisecond),
		WithVerboseErrors(),
	)
	c := app.NewContext(0)
	handler(context.Background(), c)

	e := mustUnmarshalEnvelope(t, c.Response.Body())
	var data struct {
		Checks []struct {
			Error string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Checks[0].Error != "timeout" {
		t.Errorf("Error = %q, want %q", data.Checks[0].Error, "timeout")
	}
}

// ==================== 空路径跳过注册（#5） ====================

// TestRegisterHealthRoutes_EmptyPathSkips 空路径 = 跳过该路由注册，
// 三个端点各自独立可关。
func TestRegisterHealthRoutes_EmptyPathSkips(t *testing.T) {
	// 全部跳过：所有默认路径均 404
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	RegisterHealthRoutes(h,
		WithPingPath(""), WithHealthPath(""), WithVersionPath(""),
	)
	for _, path := range []string{"/ping", "/health", "/buildVersion"} {
		resp := ut.PerformRequest(h.Engine, consts.MethodGet, path, nil).Result()
		if resp.StatusCode() == consts.StatusOK {
			t.Errorf("GET %s = 200, want 404 (empty path should skip registration)", path)
		}
	}

	// 只留 /ping：/health 与 /buildVersion 404
	h2 := server.New(server.WithHostPorts("127.0.0.1:0"))
	RegisterHealthRoutes(h2, WithHealthPath(""), WithVersionPath(""))
	resp := ut.PerformRequest(h2.Engine, consts.MethodGet, "/ping", nil).Result()
	if resp.StatusCode() != consts.StatusOK {
		t.Errorf("GET /ping = %d, want 200", resp.StatusCode())
	}
	for _, path := range []string{"/health", "/buildVersion"} {
		resp = ut.PerformRequest(h2.Engine, consts.MethodGet, path, nil).Result()
		if resp.StatusCode() == consts.StatusOK {
			t.Errorf("GET %s = 200, want 404 (should be skipped)", path)
		}
	}

	// 只留 /buildVersion
	h3 := server.New(server.WithHostPorts("127.0.0.1:0"))
	RegisterHealthRoutes(h3, WithPingPath(""), WithHealthPath(""))
	resp = ut.PerformRequest(h3.Engine, consts.MethodGet, "/buildVersion", nil).Result()
	if resp.StatusCode() != consts.StatusOK {
		t.Errorf("GET /buildVersion = %d, want 200", resp.StatusCode())
	}
}

// TestRegisterHealthRoutes_VersionUsesVersionh 确认 /buildVersion 经
// versionh.Handler 注册（私有 buildVersionHandler 副本已删除）。
func TestRegisterHealthRoutes_VersionUsesVersionh(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	RegisterHealthRoutes(h, WithPingPath(""), WithHealthPath(""))

	resp := ut.PerformRequest(h.Engine, consts.MethodGet, "/buildVersion", nil).Result()
	e := mustUnmarshalEnvelope(t, resp.Body())
	if e.Code != 0 || e.Message != "ok" {
		t.Errorf("envelope = %+v, want code=0 message=ok (versionh.Handler shape)", e)
	}
	var data struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Version == "" {
		t.Error("expected non-empty version from versionh.Handler")
	}
}
