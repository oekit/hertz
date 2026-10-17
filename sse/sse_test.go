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
			for i := 0; i < 100; i++ {
				_ = s.Send("tick", i)
			}
		}()
		time.Sleep(5 * time.Millisecond)
		_ = s.Close()
	})
	defer cleanup()

	_ = requestSSE(t, addr)
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
