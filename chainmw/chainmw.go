// Package chainmw 把 core/chain 中间件栈接入 Hertz handler 链。
//
// Wrap 生成的 app.HandlerFunc 自动注入 core/transport 服务端上下文
// （经 hertz/transportx），链内中间件即可协议无关地读写 header；
// 终端 handler 收到的 req 即 *app.RequestContext。
//
// 用法：
//
//	h.Use(chainmw.Wrap([]chain.Middleware{
//	    chain.Recovery(),
//	    chainmw2.AccessLog(),          // middleware/chainmw，transport 感知
//	}, func(ctx context.Context, c *app.RequestContext) {
//	    c.String(200, "ok")
//	}))
package chainmw

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/core/chain"
	"github.com/oekit/hertz/transportx"
	mw "github.com/oekit/middleware"
)

// WrapOption 配置 Wrap 行为。
type WrapOption func(*wrapConfig)

type wrapConfig struct {
	onError func(ctx context.Context, c *app.RequestContext, err error)
}

// WithOnError 注入错误出口：链返回 error（含 recovery 转换的 *chain.PanicError）
// 时回调。默认经 middleware.Fail 写统一响应格式；注入后完全接管。
func WithOnError(fn func(ctx context.Context, c *app.RequestContext, err error)) WrapOption {
	return func(cfg *wrapConfig) { cfg.onError = fn }
}

// Wrap 把中间件栈包装为 Hertz handler：req 形参即 *app.RequestContext，
// 自动注入 server Transporter；terminal 收 (ctx, c)。
// 链返回非 nil error 时走错误出口（默认 middleware.Fail 统一响应格式）。
func Wrap(mws []chain.Middleware, terminal app.HandlerFunc, opts ...WrapOption) app.HandlerFunc {
	cfg := &wrapConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	// 终端：chain.Handler 形态包装 Hertz handler（req 即 *app.RequestContext）
	terminalHandler := chain.Handler(func(ctx context.Context, req any) (any, error) {
		c, ok := req.(*app.RequestContext)
		if !ok {
			return nil, errNotRequestContext
		}
		terminal(ctx, c)
		return nil, nil
	})

	stacked := chain.Chain(mws...)(terminalHandler)

	return func(ctx context.Context, c *app.RequestContext) {
		ctx = transportx.NewServerContext(ctx, c)
		_, err := stacked(ctx, c)
		if err == nil {
			return
		}
		if cfg.onError != nil {
			cfg.onError(ctx, c, err)
			return
		}
		mw.Fail(ctx, c, err)
	}
}

// FromHertz 把 Hertz handler 适配为 chain.Middleware（混编用）：
// resp 恒为 nil（Hertz handler 直接写响应缓冲），错误透传。
func FromHertz(h app.HandlerFunc) chain.Middleware {
	return func(next chain.Handler) chain.Handler {
		return func(ctx context.Context, req any) (any, error) {
			c, ok := req.(*app.RequestContext)
			if !ok {
				return next(ctx, req)
			}
			h(ctx, c)
			return next(ctx, req)
		}
	}
}

var errNotRequestContext = contextDeadError{}

type contextDeadError struct{}

func (contextDeadError) Error() string {
	return "hertz/chainmw: req is not *app.RequestContext（须经 Wrap 注入）"
}
