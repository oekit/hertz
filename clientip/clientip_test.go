package clientip

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/route"
)

// TestGetClientIP_NilContext 空上下文不 panic，返回空串。
func TestGetClientIP_NilContext(t *testing.T) {
	if got := GetClientIP(nil, nil); got != "" {
		t.Errorf("GetClientIP(nil) = %q, want empty string", got)
	}
	if got := GetClientIPWithDefaultProxies(nil); got != "" {
		t.Errorf("GetClientIPWithDefaultProxies(nil) = %q, want empty string", got)
	}
}

// TestGetClientIP_UntrustedPeerIgnoresProxyHeaders 直连方（对端不在可信网段内）
// 伪造的代理头一律不采信——这是采信口径的核心防线。
func TestGetClientIP_UntrustedPeerIgnoresProxyHeaders(t *testing.T) {
	c := app.NewContext(0)
	c.Request.Header.Set("X-Forwarded-For", "9.9.9.9, 1.2.3.4")
	c.Request.Header.Set("X-Real-IP", "8.8.8.8")

	// app.NewContext 造出的上下文没有连接，对端地址是 0.0.0.0:0 —— 不可信。
	if got := GetClientIP(c, nil); got != "0.0.0.0" {
		t.Errorf("GetClientIP(untrusted peer) = %q, want 0.0.0.0 (proxy headers must be ignored)", got)
	}
	if got := GetClientIP(c, []string{"10.0.0.0/8"}); got != "0.0.0.0" {
		t.Errorf("GetClientIP(untrusted peer) = %q, want 0.0.0.0 (proxy headers must be ignored)", got)
	}

	// 对端可信时才采信 XFF（且从右向左取第一个非可信 IP）。
	if got := GetClientIP(c, []string{"0.0.0.0/8"}); got != "1.2.3.4" {
		t.Errorf("GetClientIP(trusted peer) = %q, want 1.2.3.4 (rightmost non-trusted XFF)", got)
	}
}

// startIPServer 启动一个回显客户端 IP 的测试服务器，返回地址与关闭函数。
// resolve 决定服务端用哪种口径解析客户端 IP。
func startIPServer(t *testing.T, resolve func(*app.RequestContext) string) (addr string, cleanup func()) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	opt := config.NewOptions([]config.Option{})
	opt.Listener = ln
	engine := route.NewEngine(opt)
	engine.GET("/ip", func(ctx context.Context, c *app.RequestContext) {
		c.String(200, "%s", resolve(c))
	})
	go func() {
		_ = engine.Run()
	}()
	addr = ln.Addr().String()
	time.Sleep(30 * time.Millisecond)
	return addr, func() {
		_ = engine.Close()
		_ = ln.Close()
	}
}

// requestIP 向测试服务器发请求（可带 XFF / X-Real-IP 头），返回服务端解析出的 IP。
func requestIP(t *testing.T, addr, xff, xri string) string {
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
	req.SetRequestURI("http://" + addr + "/ip")
	req.SetMethod("GET")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if xri != "" {
		req.Header.Set("X-Real-IP", xri)
	}
	if err := c.Do(context.Background(), req, resp); err != nil {
		t.Fatalf("do request: %v", err)
	}
	return string(resp.Body())
}

// TestGetClientIP_RealRequest 真实请求路径：客户端来自 127.0.0.1（默认可信网段内），
// 服务端采信 XFF；伪造值在最左端时被忽略，取的是从右向左的第一个非可信 IP。
func TestGetClientIP_RealRequest(t *testing.T) {
	addr, cleanup := startIPServer(t, func(c *app.RequestContext) string {
		return GetClientIP(c, []string{"127.0.0.0/8"})
	})
	defer cleanup()

	if got := requestIP(t, addr, "9.9.9.9, 1.2.3.4, 127.0.0.1", ""); got != "1.2.3.4" {
		t.Errorf("clientip = %q, want 1.2.3.4 (rightmost non-trusted XFF; forged 9.9.9.9 ignored)", got)
	}
	// ⚠️ 默认形态**不采信 X-Real-IP**（pkg#106）：单跳头无法区分「代理写的」与
	// 「客户端自带的」，网关默认透传客户端自带的该头。
	if got := requestIP(t, addr, "", "5.6.7.8"); got != "127.0.0.1" {
		t.Errorf("clientip = %q, want 127.0.0.1 (X-Real-IP must not be trusted by default)", got)
	}
	if got := requestIP(t, addr, "", ""); got != "127.0.0.1" {
		t.Errorf("clientip = %q, want 127.0.0.1 (peer address, no proxy headers)", got)
	}
}

// TestGetClientIPWithXRI_RealRequest 显式开启后才采信 X-Real-IP。
func TestGetClientIPWithXRI_RealRequest(t *testing.T) {
	addr, cleanup := startIPServer(t, func(c *app.RequestContext) string {
		return GetClientIPWithXRI(c, []string{"127.0.0.0/8"})
	})
	defer cleanup()

	if got := requestIP(t, addr, "", "5.6.7.8"); got != "5.6.7.8" {
		t.Errorf("clientip = %q, want 5.6.7.8 (X-Real-IP honored when explicitly enabled)", got)
	}
	// XFF 优先于 X-Real-IP。
	if got := requestIP(t, addr, "1.2.3.4", "5.6.7.8"); got != "1.2.3.4" {
		t.Errorf("clientip = %q, want 1.2.3.4 (XFF wins over X-Real-IP)", got)
	}
}

// TestGetClientIP_NilContext 的 XRI 变体同样不 panic。
func TestGetClientIPWithXRI_NilContext(t *testing.T) {
	if got := GetClientIPWithXRI(nil, nil); got != "" {
		t.Errorf("GetClientIPWithXRI(nil) = %q, want empty string", got)
	}
	if got := GetClientIPWithDefaultProxiesAndXRI(nil); got != "" {
		t.Errorf("GetClientIPWithDefaultProxiesAndXRI(nil) = %q, want empty string", got)
	}
	if got := GetClientIPWithSet(nil, nil); got != "" {
		t.Errorf("GetClientIPWithSet(nil) = %q, want empty string", got)
	}
}

// TestGetClientIP_RealRequestUntrustedPeer 可信网段不含对端时，代理头一律不采信。
func TestGetClientIP_RealRequestUntrustedPeer(t *testing.T) {
	addr, cleanup := startIPServer(t, func(c *app.RequestContext) string {
		return GetClientIP(c, []string{"10.0.0.0/8"})
	})
	defer cleanup()

	if got := requestIP(t, addr, "9.9.9.9, 1.2.3.4", ""); got != "127.0.0.1" {
		t.Errorf("clientip = %q, want 127.0.0.1 (untrusted peer must not use proxy headers)", got)
	}
}

// TestGetClientIPWithDefaultProxies_RealRequest 默认可信网段含 127.0.0.0/8，
// 与 net/http 侧 pkg/convert.GetClientIP 的默认口径一致。
func TestGetClientIPWithDefaultProxies_RealRequest(t *testing.T) {
	addr, cleanup := startIPServer(t, GetClientIPWithDefaultProxies)
	defer cleanup()

	if got := requestIP(t, addr, "9.9.9.9, 1.2.3.4, 127.0.0.1", ""); got != "1.2.3.4" {
		t.Errorf("clientip = %q, want 1.2.3.4 (default trusted proxies include 127.0.0.0/8)", got)
	}
	if got := requestIP(t, addr, "", ""); got != "127.0.0.1" {
		t.Errorf("clientip = %q, want 127.0.0.1 (peer address, no proxy headers)", got)
	}
}
