package adaptor

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
)

// MockConn 是给 adaptor handler 单测用的 network.Conn 实现。
//
// 背景（cloudwego/hertz#common/adaptor）：adaptor 的 http.ResponseWriter 通过
// rc.GetWriter() 写响应；ut.PerformRequest 从不 SetConn，GetWriter() 为 nil，
// WriteHeader 直接空指针。本类型补上 Writer/Reader 骨架，让 4 个导出 handler
// 可用 ut 风格单测。
//
// 注意：adaptor 路径的响应体写进 MockConn 的缓冲（见 Out()），**不会**出现在
// ctx.Response.Body()——那是 HijackWriter 直写 conn 的结果，不是 mock 写错了。
type MockConn struct {
	out []byte
}

// Out 返回已写出的响应字节（含状态行/头/体）。
func (m *MockConn) Out() []byte { return m.out }

// Writer

func (m *MockConn) Malloc(n int) ([]byte, error) {
	start := len(m.out)
	m.out = append(m.out, make([]byte, n)...)
	return m.out[start:], nil
}

func (m *MockConn) WriteBinary(b []byte) (int, error) {
	// b 通常指向刚 Malloc 出的切片：就地覆盖
	for i := 0; i+len(b) <= len(m.out); i++ {
		if len(b) > 0 && &m.out[i] == &b[0] {
			copy(m.out[i:], b)
			return len(b), nil
		}
	}
	m.out = append(m.out, b...)
	return len(b), nil
}

func (m *MockConn) Flush() error { return nil }

// Reader（adaptor 写路径不需要，给满接口）

func (m *MockConn) Peek(int) ([]byte, error) { return nil, io.EOF }
func (m *MockConn) Skip(int) error           { return io.EOF }
func (m *MockConn) Release() error           { return nil }
func (m *MockConn) Len() int                 { return 0 }
func (m *MockConn) ReadByte() (byte, error)  { return 0, io.EOF }
func (m *MockConn) ReadBinary(int) ([]byte, error) {
	return nil, io.EOF
}

// net.Conn

func (m *MockConn) Read([]byte) (int, error) { return 0, io.EOF }
func (m *MockConn) Write(p []byte) (int, error) {
	m.out = append(m.out, p...)
	return len(p), nil
}
func (m *MockConn) Close() error                     { return nil }
func (m *MockConn) LocalAddr() net.Addr              { return mockAddr{} }
func (m *MockConn) RemoteAddr() net.Addr             { return mockAddr{} }
func (m *MockConn) SetDeadline(time.Time) error      { return nil }
func (m *MockConn) SetReadDeadline(time.Time) error  { return nil }
func (m *MockConn) SetWriteDeadline(time.Time) error { return nil }

// network.Conn 超时

func (m *MockConn) SetReadTimeout(time.Duration) error  { return nil }
func (m *MockConn) SetWriteTimeout(time.Duration) error { return nil }

type mockAddr struct{}

func (mockAddr) Network() string { return "mock" }
func (mockAddr) String() string  { return "mock:0" }

// PerformRequest 以 ut 风格跑一次请求，但会先挂上 MockConn，使 adaptor 的
// 4 个导出 handler（HertzHandler / HertzHandlerFunc / StaticFSHandler /
// StaticDirHandler）可单测。返回状态码与响应字节。
//
// 响应来源两条路径：
//   - adaptor/HijackHandler：字节写进 MockConn（见 Out）
//   - 原生 handler（NativeStaticFSHandler 等 c.Write）：走 ctx.Response，
//     本函数把状态行/头/体拼成 HTTP 报文返回，便于统一断言
func PerformRequest(e *route.Engine, method, url string, body *ut.Body, headers ...ut.Header) (int, []byte) {
	ctx := ut.CreateUtRequestContext(method, url, body, headers...)
	conn := &MockConn{}
	ctx.SetConn(conn)
	e.ServeHTTP(context.Background(), ctx)

	code := ctx.Response.StatusCode()
	if out := conn.Out(); len(out) > 0 {
		return code, out
	}
	// 原生路径：拼 HTTP 报文（便于 Contains 断言头/体）
	var b []byte
	b = append(b, "HTTP/1.1 "...)
	b = append(b, []byte(itoa(code))...)
	b = append(b, " native\r\n"...)
	ctx.Response.Header.VisitAll(func(k, v []byte) {
		b = append(b, k...)
		b = append(b, ": "...)
		b = append(b, v...)
		b = append(b, "\r\n"...)
	})
	b = append(b, "\r\n"...)
	b = append(b, ctx.Response.Body()...)
	return code, b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// MustGetConn 从 RequestContext 取出 PerformRequest 注入的 MockConn。
// 在自定义 handler 里断言写出内容时使用。
func MustGetConn(ctx *app.RequestContext) (*MockConn, error) {
	if ctx == nil {
		return nil, errors.New("nil request context")
	}
	c, _ := ctx.GetConn().(*MockConn)
	if c == nil {
		return nil, errors.New("no MockConn attached (use adaptor.PerformRequest)")
	}
	return c, nil
}
