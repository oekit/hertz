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
//
// ⚠️ 链内不得放 core/chain.Timeout（#343）：Timeout 超时先到即向 Wrap 返回
// DeadlineExceeded → Wrap 侧 mw.Fail 写错误响应；被弃的下游 goroutine 仍在
// 后台跑（Timeout 的语义边界：不理会 ctx 取消的下游无法强制中断），其后对
// c 的任何响应写入都与 Fail 的错误体落进**同一份** *app.RequestContext
// 响应缓冲——双写/交错写，响应面损坏且无信号。hertz 链的超时治理走
// hertz/governance 的 NewTimeoutMiddleware：只注入 per-request deadline、
// 由下游 ctx 感知依赖提前中止，治理侧不掐断响应（cancel 时机 #235 已钉：
// 不早于 deadline）。
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
//
// 被适配 handler 的 c.Abort() 会终止**整条 chain 链**（含 terminal，#289）：
// hertz 的 Abort 只置本包看不到的 hertz ctx.index、停的是 hertz 自己的
// handlers 链，而 chain 的 next 是 core/chain 另一条链——不显式检查
// IsAborted 时，mw.ErrorResp 这类「写完 401/403/500 即 Abort」的认证/限流
// 短路在混编栈里会被绕过（下游照常执行并可能把拒绝响应覆写成 200）。
func FromHertz(h app.HandlerFunc) chain.Middleware {
	return func(next chain.Handler) chain.Handler {
		return func(ctx context.Context, req any) (any, error) {
			c, ok := req.(*app.RequestContext)
			if !ok {
				return next(ctx, req)
			}
			h(ctx, c)
			if c.IsAborted() {
				return nil, nil
			}
			return next(ctx, req)
		}
	}
}

var errNotRequestContext = contextDeadError{}

type contextDeadError struct{}

func (contextDeadError) Error() string {
	return "hertz/chainmw: req is not *app.RequestContext（须经 Wrap 注入）"
}
