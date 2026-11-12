package hertztracer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/hertz-contrib/tracer/common"
	opentracing "github.com/opentracing/opentracing-go"
	opentracinglog "github.com/opentracing/opentracing-go/log"
)

// --- #177 回归：client 中间件必须真建 span 并注入 trace 上下文 ---
//
// 此前 NewClientMiddleware(_ ...TracerOption) 直接转发上游 ClientCtx——后者只把 ctx
// 里已有的 metainfo 写成 HTTP 头：不建 client span、不注入任何 trace 上下文，
// 且显式传入的 TracerOption 被无条件丢弃（_ ...TracerOption）。出站调用在 trace 里不可见，
// 下游 ServerCtx 因查不到 JAEGERSPANCONTEXT 另起根 span，分布式链路断裂。

// injectableTracer 记录 StartSpan / SetTag / LogFields，并把 Inject(Binary) 的
// 载荷写成固定标记——供断言「span 上下文进了请求头」。
type injectableTracer struct {
	mu    sync.Mutex
	spans []*spySpan
}

func (t *injectableTracer) StartSpan(operationName string, opts ...opentracing.StartSpanOption) opentracing.Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &spySpan{name: operationName, tags: map[string]any{}}
	t.spans = append(t.spans, s)
	return s
}

func (t *injectableTracer) Inject(sc opentracing.SpanContext, format any, carrier any) error {
	if _, ok := format.(opentracing.BuiltinFormat); !ok && format != opentracing.Binary {
		return opentracing.ErrUnsupportedFormat
	}
	buf, ok := carrier.(*bytes.Buffer)
	if !ok {
		return opentracing.ErrInvalidCarrier
	}
	_, err := buf.WriteString("mock-span-context-payload")
	return err
}

func (t *injectableTracer) Extract(_ any, _ any) (opentracing.SpanContext, error) {
	return nil, opentracing.ErrUnsupportedFormat
}

func (t *injectableTracer) recorded() []*spySpan {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*spySpan(nil), t.spans...)
}

// failingInjectTracer 只在 Inject 时报错（钉「Inject 失败不中断调用」）。
type failingInjectTracer struct{ injectableTracer }

func (t *failingInjectTracer) Inject(opentracing.SpanContext, any, any) error {
	return opentracing.ErrUnsupportedFormat
}

// spySpan 记录 SetTag / LogFields（Finish 幂等）。
type spySpan struct {
	name string
	tags map[string]any
	logs []string
}

func (s *spySpan) SetOperationName(name string) opentracing.Span { s.name = name; return s }
func (s *spySpan) SetTag(key string, value any) opentracing.Span {
	s.tags[key] = value
	return s
}
func (s *spySpan) LogFields(fields ...opentracinglog.Field) {
	rec := &logRecorder{}
	for _, f := range fields {
		f.Marshal(rec)
	}
	s.logs = append(s.logs, rec.parts...)
}

// logRecorder 是 opentracing log.Encoder 的最小实现：把字段拍平成 "k=v" 串。
type logRecorder struct{ parts []string }

func (r *logRecorder) add(k, v string)        { r.parts = append(r.parts, k+"="+v) }
func (r *logRecorder) EmitString(k, v string) { r.add(k, v) }
func (r *logRecorder) EmitBool(k string, v bool) {
	r.add(k, strconv.FormatBool(v))
}
func (r *logRecorder) EmitInt(k string, v int)       { r.add(k, strconv.Itoa(v)) }
func (r *logRecorder) EmitInt32(k string, v int32)   { r.add(k, strconv.FormatInt(int64(v), 10)) }
func (r *logRecorder) EmitInt64(k string, v int64)   { r.add(k, strconv.FormatInt(v, 10)) }
func (r *logRecorder) EmitUint32(k string, v uint32) { r.add(k, strconv.FormatUint(uint64(v), 10)) }
func (r *logRecorder) EmitUint64(k string, v uint64) { r.add(k, strconv.FormatUint(v, 10)) }
func (r *logRecorder) EmitFloat32(k string, v float32) {
	r.add(k, strconv.FormatFloat(float64(v), 'g', -1, 32))
}
func (r *logRecorder) EmitFloat64(k string, v float64) {
	r.add(k, strconv.FormatFloat(v, 'g', -1, 64))
}
func (r *logRecorder) EmitError(k string, v error) { r.add(k, v.Error()) }
func (r *logRecorder) EmitObject(k string, v any)  { r.add(k, fmt.Sprint(v)) }
func (r *logRecorder) EmitLazyLogger(v opentracinglog.LazyLogger) {
	v(r)
}
func (s *spySpan) LogKV(_ ...any)                              {}
func (s *spySpan) Log(_ opentracing.LogData)                   {}
func (s *spySpan) LogEvent(_ string)                           {}
func (s *spySpan) LogEventWithPayload(_ string, _ any)         {}
func (s *spySpan) Finish()                                     {}
func (s *spySpan) FinishWithOptions(opentracing.FinishOptions) {}
func (s *spySpan) Tracer() opentracing.Tracer                  { return nil }
func (s *spySpan) Event(_ string)                              {}
func (s *spySpan) EventWithPayload(_ string, _ any)            {}
func (s *spySpan) SetBaggageItem(_, _ string) opentracing.Span { return s }
func (s *spySpan) BaggageItem(_ string) string                 { return "" }
func (s *spySpan) Context() opentracing.SpanContext            { return nil }

// headerVisitAdapter 让 hertz 请求头可被 metainfo.FromHTTPHeader 读取
// （等价上游 StringHeader 适配器，测试内最小实现）。
type headerVisitAdapter protocol.RequestHeader

func (h *headerVisitAdapter) Visit(f func(k, v string)) {
	(*protocol.RequestHeader)(h).VisitAll(func(key, value []byte) {
		f(string(key), string(value))
	})
}

// runClientMW 以固定请求跑一遍 client 中间件，返回写入后的请求头与 endpoint 错误。
func runClientMW(t *testing.T, mw client.Middleware, ctx context.Context, endpoint client.Endpoint) *protocol.Request {
	t.Helper()
	req, resp := protocol.AcquireRequest(), protocol.AcquireResponse()
	req.SetMethod("GET")
	req.SetRequestURI("/ping?x=1")
	err := mw(endpoint)(ctx, req, resp)
	if err != nil {
		t.Fatalf("middleware returned unexpected error: %v", err)
	}
	return req
}

// TestClientMiddlewareStartsSpanAndInjects 钉住核心两件事：
// 1) 用配置的 tracer 产生 client span；2) 下游 ServerCtx 的接续键
// （JAEGERSPANCONTEXT，经 metainfo 的 rpc-persist- 前缀头）出现在出站请求头里，
// 且能被 metainfo.FromHTTPHeader + GetValue 读回（与上游 ServerCtx 读路径逐字对齐）。
func TestClientMiddlewareStartsSpanAndInjects(t *testing.T) {
	tr := &injectableTracer{}
	mw := NewClientMiddleware(WithTracer(tr))
	var captured *protocol.Request
	req := runClientMW(t, mw, context.Background(),
		func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			captured = req
			return nil
		})

	spans := tr.recorded()
	if len(spans) != 1 {
		t.Fatalf("client middleware must create exactly one span, got %d", len(spans))
	}
	if spans[0].name != "GET /ping" {
		t.Errorf("default client operation name = %q, want %q", spans[0].name, "GET /ping")
	}

	// 出站头里有 span 上下文（rpc-persist-jaegerspancontext）。
	if got := string(captured.Header.Peek("rpc-transit-jaegerspancontext")); got != "mock-span-context-payload" {
		t.Fatalf("outbound request must carry the injected span context, got %q", got)
	}
	// 与上游 ServerCtx 的读路径对齐：FromHTTPHeader → GetValue(SpanContextLabel)。
	ctx := metainfo.FromHTTPHeader(context.Background(), (*headerVisitAdapter)(&captured.Header))
	if v, ok := metainfo.GetValue(ctx, common.SpanContextLabel); !ok || v != "mock-span-context-payload" {
		t.Errorf("ServerCtx read path must recover the span context, got %q ok=%v", v, ok)
	}
	_ = req
}

// TestClientMiddleware_OptionsTakeEffect 钉住 TracerOption 真正生效（此前 _ ...TracerOption
// 把 WithTracer / 自定义 span 名原样丢弃）。
func TestClientMiddleware_OptionsTakeEffect(t *testing.T) {
	tr := &injectableTracer{}
	mw := NewClientMiddleware(
		WithTracer(tr),
		WithClientOperationName(func(req *protocol.Request) string { return "call-downstream" }),
	)
	runClientMW(t, mw, context.Background(), func(_ context.Context, _ *protocol.Request, _ *protocol.Response) error {
		return nil
	})

	spans := tr.recorded()
	if len(spans) != 1 {
		t.Fatalf("configured tracer must be used: spans = %d, want 1", len(spans))
	}
	if spans[0].name != "call-downstream" {
		t.Errorf("custom operation name = %q, want call-downstream", spans[0].name)
	}
}

// TestClientMiddleware_ErrorTagged 调用失败：span 打 error tag + 错误日志，
// 错误原样上抛（不吞）。
func TestClientMiddleware_ErrorTagged(t *testing.T) {
	tr := &injectableTracer{}
	mw := NewClientMiddleware(WithTracer(tr))
	sentinel := errors.New("downstream boom")
	err := mw(func(_ context.Context, _ *protocol.Request, _ *protocol.Response) error {
		return sentinel
	})(context.Background(), protocol.NewRequest("GET", "/ping", nil), protocol.AcquireResponse())
	if !errors.Is(err, sentinel) {
		t.Fatalf("endpoint error must be returned as-is, got %v", err)
	}
	spans := tr.recorded()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].tags["error"] != true {
		t.Errorf("failed call must set error tag, got %v", spans[0].tags)
	}
	found := false
	for _, l := range spans[0].logs {
		if strings.Contains(l, "downstream boom") {
			found = true
		}
	}
	if !found {
		t.Errorf("failed call must log the error, logs = %v", spans[0].logs)
	}
}

// TestClientMiddleware_MetainfoPassthroughPreserved 上游 ClientCtx 的既有行为
// （业务 metainfo → HTTP 头）必须原样保留。
func TestClientMiddleware_MetainfoPassthroughPreserved(t *testing.T) {
	mw := NewClientMiddleware(WithTracer(&injectableTracer{}))
	ctx := metainfo.WithPersistentValue(context.Background(), "biz-key", "biz-value")
	var captured *protocol.Request
	runClientMW(t, mw, ctx, func(_ context.Context, req *protocol.Request, _ *protocol.Response) error {
		captured = req
		return nil
	})
	if got := string(captured.Header.Peek("rpc-persist-biz-key")); got != "biz-value" {
		t.Errorf("business metainfo must still be written to headers, got %q", got)
	}
}

// TestClientMiddleware_NilContext nil ctx 不 panic（上游 ClientCtx 的 nil 防御
// 口径保留）。
func TestClientMiddleware_NilContext(t *testing.T) {
	mw := NewClientMiddleware(WithTracer(&injectableTracer{}))
	runClientMW(t, mw, nil, func(_ context.Context, _ *protocol.Request, _ *protocol.Response) error {
		return nil
	})
}

// TestClientMiddleware_InjectFailureKeepsChain 注入失败只跳过 span 上下文写入，
// 调用照常进行、span 照常产生（链路不中断）。
func TestClientMiddleware_InjectFailureKeepsChain(t *testing.T) {
	tr := &failingInjectTracer{}
	mw := NewClientMiddleware(WithTracer(tr))
	var captured *protocol.Request
	runClientMW(t, mw, context.Background(), func(_ context.Context, req *protocol.Request, _ *protocol.Response) error {
		captured = req
		return nil
	})
	if len(tr.recorded()) != 1 {
		t.Error("span must still be created when Inject fails")
	}
	if got := string(captured.Header.Peek("rpc-transit-jaegerspancontext")); got != "" {
		t.Errorf("no span context header must be written when Inject fails, got %q", got)
	}
}
