package chainmw

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/core/chain"
	"github.com/oekit/core/transport"
	mw "github.com/oekit/middleware"
)

func TestWrap_TerminalReceivesRequestContext(t *testing.T) {
	c := app.NewContext(8)
	c.Request.Reset()
	c.Request.SetRequestURI("http://svc.local:8080/api/ping")

	var gotPath string
	handler := Wrap(nil, func(ctx context.Context, c *app.RequestContext) {
		gotPath = string(c.Path())
	})
	handler(context.Background(), c)

	if gotPath != "/api/ping" {
		t.Fatalf("terminal path = %q", gotPath)
	}
}

func TestWrap_InjectsServerTransport(t *testing.T) {
	c := app.NewContext(8)
	c.Request.Reset()
	c.Request.SetRequestURI("http://svc.local:8080/api/ping")
	c.Request.Header.Set("X-Tenant", "acme")
	c.Response.Header.Reset()

	var tenant string
	handler := Wrap([]chain.Middleware{
		func(next chain.Handler) chain.Handler {
			return func(ctx context.Context, req any) (any, error) {
				if tr, ok := transport.FromServerContext(ctx); ok {
					tenant = tr.RequestHeader().Get("X-Tenant")
					tr.ReplyHeader().Set("X-Back", "1")
				}
				return next(ctx, req)
			}
		},
	}, func(ctx context.Context, c *app.RequestContext) {})
	handler(context.Background(), c)

	if tenant != "acme" {
		t.Fatalf("header not readable via transport: %q", tenant)
	}
	if c.Response.Header.Get("X-Back") != "1" {
		t.Fatal("reply header write-back broken")
	}
}

func TestWrap_RecoveryOnErrorExit(t *testing.T) {
	c := app.NewContext(8)
	c.Request.Reset()
	c.Request.SetRequestURI("http://svc.local/x")

	var gotErr error
	handler := Wrap([]chain.Middleware{
		chain.Recovery(),
	}, func(ctx context.Context, c *app.RequestContext) {
		panic("boom")
	}, WithOnError(func(ctx context.Context, c *app.RequestContext, err error) {
		gotErr = err
	}))
	handler(context.Background(), c)

	var pe *chain.PanicError
	if !errors.As(gotErr, &pe) || !strings.Contains(pe.Error(), "boom") {
		t.Fatalf("onError err = %v, want *PanicError", gotErr)
	}
}

func TestFromHertz(t *testing.T) {
	c := app.NewContext(8)
	c.Request.Reset()
	c.Request.SetRequestURI("http://svc.local/x")

	var hertzRan, nextRan bool
	mw := FromHertz(func(ctx context.Context, c *app.RequestContext) {
		hertzRan = true
	})
	h := mw(func(ctx context.Context, req any) (any, error) {
		nextRan = true
		return nil, nil
	})
	h(context.Background(), c)
	if !hertzRan || !nextRan {
		t.Fatalf("FromHertz broken: hertz=%v next=%v", hertzRan, nextRan)
	}
}

// TestFromHertz_AbortStopsChain 钉住 #289:被适配 hertz handler 的 c.Abort()
// 必须终止**整条 chain 链**(含 terminal)。hertz 的 Abort 只停 hertz 自己的
// ctx.index 链,与 chain 的 next 无关——不检查 IsAborted 时,ErrorResp 这类
// 「写完 401 即 Abort」的认证短路会被绕过(terminal 照跑并覆写响应)。
func TestFromHertz_AbortStopsChain(t *testing.T) {
	c := app.NewContext(8)
	c.Request.Reset()
	c.Request.SetRequestURI("http://svc.local/api/secret")

	terminalRan := false
	handler := Wrap([]chain.Middleware{
		FromHertz(func(ctx context.Context, c *app.RequestContext) {
			mw.ErrorResp(c, 401, "unauthorized") // 写 401 响应 + c.Abort()
		}),
	}, func(ctx context.Context, c *app.RequestContext) {
		terminalRan = true
		c.String(200, "ok")
	})
	handler(context.Background(), c)

	if terminalRan {
		t.Fatal("terminal must not run after adapted hertz handler aborted (#289)")
	}
	if code := c.Response.StatusCode(); code != 401 {
		t.Errorf("status = %d, want 401 (denial response must not be overwritten)", code)
	}
}
