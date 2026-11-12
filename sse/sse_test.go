package sse

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/route"
)

// startSSEServer 启动一个带 SSE 端点的测试服务器，返回地址和关闭函数。
func startSSEServer(t *testing.T, handler app.HandlerFunc) (addr string, cleanup func()) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	opt := config.NewOptions([]config.Option{})
	opt.Listener = ln
	engine := route.NewEngine(opt)
	engine.GET("/stream", handler)
	go func() {
		_ = engine.Run()
	}()
	addr = ln.Addr().String()
	// 等待服务器就绪
	time.Sleep(30 * time.Millisecond)
	return addr, func() {
		_ = engine.Close()
		_ = ln.Close()
	}
}

// requestSSE 向测试服务器发起请求，返回响应体。
func requestSSE(t *testing.T, addr string) []byte {
	t.Helper()
	c, err := client.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	req, resp := protocol.AcquireRequest(), protocol.AcquireResponse()
	defer func() {
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
	}()
	req.SetRequestURI("http://" + addr + "/stream")
	req.SetMethod("GET")
	if err := c.Do(context.Background(), req, resp); err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp.Body()
}

func TestStream_Send_JSON(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.Send("progress", map[string]int{"percent": 75})
		_ = s.Send("done", map[string]string{"result": "ok"})
		_ = s.Close()
	})
	defer cleanup()

	body := requestSSE(t, addr)
	bodyStr := string(body)
	assert.DeepEqual(t, true, contains(bodyStr, "event: progress"))
	assert.DeepEqual(t, true, contains(bodyStr, `{"percent":75}`))
	assert.DeepEqual(t, true, contains(bodyStr, "event: done"))
	assert.DeepEqual(t, true, contains(bodyStr, `"result":"ok"`))
}

func TestStream_Send_NilData(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.Send("ping", nil)
		_ = s.Close()
	})
	defer cleanup()

	body := requestSSE(t, addr)
	assert.DeepEqual(t, true, contains(string(body), "event: ping"))
}

func TestStream_SendRaw(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.SendRaw("id-1", "message", []byte("hello"))
		_ = s.Close()
	})
	defer cleanup()

	body := string(requestSSE(t, addr))
	assert.DeepEqual(t, true, contains(body, "id: id-1"))
	assert.DeepEqual(t, true, contains(body, "event: message"))
	assert.DeepEqual(t, true, contains(body, "data: hello"))
}

func TestStream_Write_Event(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		e := &Event{}
		e.SetID("evt-1")
		e.SetEvent("update")
		e.SetDataString(`{"v":1}`)
		_ = s.Write(e)
		_ = s.Close()
	})
	defer cleanup()

	body := string(requestSSE(t, addr))
	assert.DeepEqual(t, true, contains(body, "id: evt-1"))
	assert.DeepEqual(t, true, contains(body, "event: update"))
	assert.DeepEqual(t, true, contains(body, `data: {"v":1}`))
}

func TestStream_Headers(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.Send("progress", "x")
		_ = s.Close()
	})
	defer cleanup()

	c, _ := client.NewClient()
	req, resp := protocol.AcquireRequest(), protocol.AcquireResponse()
	defer protocol.ReleaseRequest(req)
	defer protocol.ReleaseResponse(resp)
	req.SetRequestURI("http://" + addr + "/stream")
	req.SetMethod("GET")
	_ = c.Do(context.Background(), req, resp)

	ct := string(resp.Header.ContentType())
	assert.DeepEqual(t, true, contains(ct, "text/event-stream"))
	assert.DeepEqual(t, true, contains(resp.Header.Get("Cache-Control"), "no-cache"))
}

func TestStream_Send_AfterClose_ErrStreamClosed(t *testing.T) {
	// 创建一个已关闭的 stream（通过 Close 公共 API）
	s := &Stream{}
	_ = s.Close() // 首次关闭
	err := s.Send("progress", "data")
	assert.DeepEqual(t, ErrStreamClosed, err)
	assert.DeepEqual(t, ErrStreamClosed, s.SendRaw("1", "t", []byte("x")))
	assert.DeepEqual(t, ErrStreamClosed, s.KeepAlive())
	assert.DeepEqual(t, ErrStreamClosed, s.Write(&Event{}))
}

func TestStream_Close_Idempotent(t *testing.T) {
	s := &Stream{}
	_ = s.Close() // 首次关闭
	// 已关闭状态调用 Close 返回 nil，安全
	assert.Nil(t, s.Close())
}

func TestStream_ConcurrentSendClose_NoRace(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		go func() {
			for i := range 100 {
				_ = s.Send("tick", i)
			}
		}()
		time.Sleep(5 * time.Millisecond)
		_ = s.Close()
	})
	defer cleanup()

	_ = requestSSE(t, addr)
}

// TestStream_Send_EmptyEventType_OmitsEventField 钉住（并登记）上游行为：
// eventType 为空时上游 Writer.Write 不写 "event:" 行（IsSetType 要求非空）
// ⇒ 事件按 SSE 规范落到默认类型 "message"，而不是变成"无类型事件"。
// 本包不兜底改写（改写会变更线上字节）。
func TestStream_Send_EmptyEventType_OmitsEventField(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.Send("", map[string]int{"percent": 75})
		_ = s.Close()
	})
	defer cleanup()

	body := string(requestSSE(t, addr))
	if contains(body, "event:") {
		t.Errorf("empty event type must not emit an event field: %q", body)
	}
	if !contains(body, "{\"percent\":75}") {
		t.Errorf("data missing: %q", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestStream_LastEventID 对齐官方教程：客户端通过 Last-Event-ID 头声明
// 续传位置，服务端用 sse.LastEventID 读取并回传。首次连接返回空串。
func TestStream_LastEventID(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.SendRaw("echo", "last-id", []byte("["+LastEventID(c)+"]"))
		_ = s.Close()
	})
	defer cleanup()

	c, err := client.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	do := func(setID func(*protocol.Request)) string {
		req, resp := protocol.AcquireRequest(), protocol.AcquireResponse()
		defer func() {
			protocol.ReleaseRequest(req)
			protocol.ReleaseResponse(resp)
		}()
		req.SetRequestURI("http://" + addr + "/stream")
		req.SetMethod("GET")
		setID(req)
		if err := c.Do(context.Background(), req, resp); err != nil {
			t.Fatalf("do request: %v", err)
		}
		return string(resp.Body())
	}

	// 首次连接：无 Last-Event-ID
	first := do(func(*protocol.Request) {})
	assert.DeepEqual(t, true, contains(first, "data: []"))

	// 重连：携带 Last-Event-ID
	reconnect := do(func(req *protocol.Request) { SetLastEventID(req, "evt-42") })
	assert.DeepEqual(t, true, contains(reconnect, "data: [evt-42]"))
}

// TestStream_WriteComment 对齐官方 Writer.WriteComment：注释行原样下发。
func TestStream_WriteComment(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.WriteComment("heartbeat-check")
		_ = s.Send("tick", 1)
		_ = s.Close()
	})
	defer cleanup()

	body := string(requestSSE(t, addr))
	assert.DeepEqual(t, true, contains(body, ":heartbeat-check"))
}

// TestClient_ReaderForEach 对齐官方教程客户端用法：
// AddAcceptMIME + NewReader + ForEach 逐事件消费，Reader 记录最后事件 ID。
func TestClient_ReaderForEach(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.SendRaw("id-1", "message", []byte("hello"))
		_ = s.SendRaw("id-2", "message", []byte("world"))
		_ = s.Close()
	})
	defer cleanup()

	c, err := client.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	req, resp := protocol.AcquireRequest(), protocol.AcquireResponse()
	defer func() {
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
	}()
	req.SetRequestURI("http://" + addr + "/stream")
	req.SetMethod("GET")
	AddAcceptMIME(req)

	if derr := c.Do(context.Background(), req, resp); derr != nil {
		t.Fatalf("do request: %v", derr)
	}

	r, err := NewReader(resp)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}

	var ids, datas []string
	// 服务端正常断开时 ForEach 返回 nil
	err = r.ForEach(context.Background(), func(e *Event) error {
		ids = append(ids, e.ID)
		datas = append(datas, string(e.Data))
		return nil
	})
	assert.Nil(t, err)
	assert.DeepEqual(t, []string{"id-1", "id-2"}, ids)
	assert.DeepEqual(t, []string{"hello", "world"}, datas)
	assert.DeepEqual(t, "id-2", r.LastEventID())
}

// TestStream_Send_NilData_EventDispatched 回归 #229：Send(type, nil) 必须写出
// data: 字段（此前无 data 行 ⇒ 按 SSE 规范事件不被派发，浏览器一个事件都收
// 不到），且官方 Reader 能收到该事件（空 data 事件被派发）。
func TestStream_Send_NilData_EventDispatched(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.Send("ping", nil)
		_ = s.Close()
	})
	defer cleanup()

	body := string(requestSSE(t, addr))
	// 线上字节：必须存在 data: 行（空值 data 字段）。
	assert.DeepEqual(t, true, contains(body, "event: ping"))
	assert.DeepEqual(t, true, contains(body, "data: "))

	// 客户端侧：官方 Reader 必须把该事件派发出来（data 为空）。
	cl, err := client.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	req, resp := protocol.AcquireRequest(), protocol.AcquireResponse()
	defer func() {
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
	}()
	req.SetRequestURI("http://" + addr + "/stream")
	req.SetMethod("GET")
	if err = cl.Do(context.Background(), req, resp); err != nil {
		t.Fatalf("do request: %v", err)
	}

	r, err := NewReader(resp)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	events := 0
	var gotType string
	err = r.ForEach(context.Background(), func(e *Event) error {
		events++
		gotType = e.Type
		return nil
	})
	if err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	if events != 1 {
		t.Fatalf("events = %d, want 1 (nil-data event must still be dispatched, #229)", events)
	}
	if gotType != "ping" {
		t.Errorf("event type = %q, want ping", gotType)
	}
}

// TestStream_Send_NilData_Bytes 钉住线上字节形态：Send("ping", nil) 产出
// "event: ping\ndata: \n\n"——空值 data 行让 data buffer 归一为空串后按规范
// 派发，客户端 event.data === ""。
func TestStream_Send_NilData_Bytes(t *testing.T) {
	addr, cleanup := startSSEServer(t, func(ctx context.Context, c *app.RequestContext) {
		s := NewStream(c)
		_ = s.Send("ping", nil)
		_ = s.Close()
	})
	defer cleanup()

	assert.DeepEqual(t, "event: ping\ndata: \n\n", string(requestSSE(t, addr)))
}
