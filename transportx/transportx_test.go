package transportx

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/core/transport"
)

func newTestRequestContext(t *testing.T, host, path string) *app.RequestContext {
	t.Helper()
	c := app.NewContext(8)
	c.Request.Reset()
	c.Request.SetRequestURI("http://" + host + path)
	c.Request.Header.Set("X-Trace-ID", "abc")
	c.Request.Header.Add("X-Multi", "a")
	c.Request.Header.Add("X-Multi", "b")
	return c
}

func TestNewServerTransport(t *testing.T) {
	c := newTestRequestContext(t, "svc.local:8080", "/api/v1/ping")
	tr := NewServerTransport(c)

	if tr.Kind() != transport.KindHTTP {
		t.Fatalf("kind = %s, want http", tr.Kind())
	}
	if tr.Endpoint() != "svc.local:8080" {
		t.Fatalf("endpoint = %q", tr.Endpoint())
	}
	if tr.Operation() != "/api/v1/ping" {
		t.Fatalf("operation = %q", tr.Operation())
	}
}

func TestRequestHeaderRoundTrip(t *testing.T) {
	c := newTestRequestContext(t, "h", "/")
	tr := NewServerTransport(c)
	h := tr.RequestHeader()

	if got := h.Get("X-Trace-ID"); got != "abc" {
		t.Fatalf("Get = %q, want abc", got)
	}
	if vs := h.Values("X-Multi"); len(vs) != 2 || vs[0] != "a" || vs[1] != "b" {
		t.Fatalf("Values = %v", vs)
	}
	h.Set("X-Trace-ID", "def")
	if got := h.Get("X-Trace-ID"); got != "def" {
		t.Fatalf("Set broken: %q", got)
	}
	h.Add("X-Multi", "c")
	if vs := h.Values("X-Multi"); len(vs) != 3 {
		t.Fatalf("Add broken: %v", vs)
	}
	h.Del("X-Multi")
	if vs := h.Values("X-Multi"); len(vs) != 0 {
		t.Fatalf("Del broken: %v", vs)
	}
	keys := h.Keys()
	if len(keys) != 1 || keys[0] != "X-Trace-Id" {
		// hertz 规范化键名（X-Trace-ID → X-Trace-Id）
		t.Fatalf("Keys = %v", keys)
	}
}

func TestReplyHeaderWriteBack(t *testing.T) {
	c := newTestRequestContext(t, "h", "/")
	tr := NewServerTransport(c)
	reply := tr.ReplyHeader()
	reply.Set("X-Resp", "ok")
	if got := c.Response.Header.Get("X-Resp"); got != "ok" {
		t.Fatalf("reply write-back broken: %q", got)
	}
}

// TestReplyHeaderAdd_Appends 钉住 reply header 的 Add 语义：**追加**而非覆盖。
//
// 此前 replyHeader.Add 误用 Set（覆盖）——同 key 多次 Add 只剩最后一个值，
// 与 requestHeader.Add 及 core/transport 的 Header.Add 契约都不符。
func TestReplyHeaderAdd_Appends(t *testing.T) {
	c := newTestRequestContext(t, "h", "/")
	tr := NewServerTransport(c)
	reply := tr.ReplyHeader()

	reply.Add("X-Multi", "a")
	reply.Add("X-Multi", "b")
	if vs := reply.Values("X-Multi"); len(vs) != 2 || vs[0] != "a" || vs[1] != "b" {
		t.Fatalf("Add must append, got %v", vs)
	}
	if got := c.Response.Header.Peek("X-Multi"); string(got) != "a" {
		t.Fatalf("response header = %q, want a (first appended value)", got)
	}

	// Set 仍是单值（覆盖）语义——与 Add 区分开。
	// 注意：hertz 的 setArg 只替换首个同键值，混用 Add/Set 的结果由 hertz 决定，
	// 本用例只钉住「Set 不追加」。
	reply.Set("X-Single", "c")
	if vs := reply.Values("X-Single"); len(vs) != 1 || vs[0] != "c" {
		t.Fatalf("Set must overwrite, got %v", vs)
	}
}

func TestNewServerContext(t *testing.T) {
	c := newTestRequestContext(t, "svc.local:8080", "/api/ping")
	ctx := NewServerContext(context.Background(), c)
	got, ok := transport.FromServerContext(ctx)
	if !ok {
		t.Fatal("transporter not injected")
	}
	if got.Operation() != "/api/ping" {
		t.Fatalf("op = %q", got.Operation())
	}
}

// TestRequestHeaderValues_CaseInsensitive 表驱动（#163）：Values 的键匹配必须
// 与 Get 同口径（大小写不敏感）。hertz 存取键均规范化为 MIME canonical 形态
// （Set("X-Trace-ID",…) 落盘为 "X-Trace-Id"），此前 Values 原文比较——Get 命中
// 而同键 Values 返回 nil，同一适配器两套键口径。
func TestRequestHeaderValues_CaseInsensitive(t *testing.T) {
	c := newTestRequestContext(t, "h", "/")
	tr := NewServerTransport(c)
	h := tr.RequestHeader()

	cases := []struct {
		name    string
		key     string
		wantLen int
	}{
		{"canonical form", "X-Trace-Id", 1},
		{"raw input form", "X-Trace-ID", 1},
		{"lowercase", "x-trace-id", 1},
		{"uppercase", "X-TRACE-ID", 1},
		{"multi canonical", "X-Multi", 2},
		{"multi lowercase", "x-multi", 2},
		{"missing key", "X-Absent", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs := h.Values(tc.key)
			if len(vs) != tc.wantLen {
				t.Fatalf("Values(%q) = %v, want %d values（口径必须与 Get 一致，#163）", tc.key, vs, tc.wantLen)
			}
		})
	}

	// Get 与 Values 一致性：Get 命中的键 Values 必命中、值相同。
	if got := h.Get("x-trace-id"); got != "abc" {
		t.Fatalf("Get(x-trace-id) = %q, want abc", got)
	}
	if vs := h.Values("x-trace-id"); len(vs) != 1 || vs[0] != "abc" {
		t.Fatalf("Get 命中而 Values 落空（#163 禁止）：%v", vs)
	}
}

// TestReplyHeaderValues_CaseInsensitive replyHeader.Values 同病同修（#163）：
// Add 传入的小写键（hertz 规范化为 canonical）经任意大小写变体都能取回。
func TestReplyHeaderValues_CaseInsensitive(t *testing.T) {
	c := newTestRequestContext(t, "h", "/")
	tr := NewServerTransport(c)
	reply := tr.ReplyHeader()

	reply.Add("x-multi", "a")
	reply.Add("x-multi", "b")

	for _, key := range []string{"X-Multi", "x-multi", "X-MULTI", "x-MULTI"} {
		vs := reply.Values(key)
		if len(vs) != 2 || vs[0] != "a" || vs[1] != "b" {
			t.Fatalf("Values(%q) = %v, want [a b]（口径必须与 Get 一致，#163）", key, vs)
		}
	}
	if got := reply.Get("X-MULTI"); got != "a" {
		t.Fatalf("Get(X-MULTI) = %q, want a", got)
	}
}

// TestKeys_ReturnsCanonicalForm 钉住 Keys 的 canonical 形态契约（#163 godoc）：
// Set("X-Trace-ID",…) 后 Keys() 产出 hertz 规范化的 "X-Trace-Id"。
func TestKeys_ReturnsCanonicalForm(t *testing.T) {
	c := newTestRequestContext(t, "h", "/")
	tr := NewServerTransport(c)
	keys := tr.RequestHeader().Keys()
	if len(keys) != 2 || keys[0] != "X-Multi" || keys[1] != "X-Trace-Id" {
		t.Fatalf("Keys = %v, want [X-Multi X-Trace-Id]（canonical 形态，已排序）", keys)
	}
}
