// Package transportx 把 Hertz RequestContext 适配为 core/transport 抽象。
//
// 经 NewServerContext 注入后，框架无关的中间件（core/chain + middleware/chainmw）
// 即可协议无关地读写 header、读取 Kind/Endpoint/Operation。
//
// #216 治本：header/transporter 适配的单源在 core/transport/hertzbridge
// （core PR #145 下沉，middleware/transporter.go 同构三胞胎随单源收敛），
// 本包退化为薄转发——仓内不再持有 requestHeader/replyHeader 同构副本，
// #163 的 EqualFold 键匹配口径、replyHeader.Add 的追加语义随单源固化。
package transportx

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/core/transport"
	"github.com/oekit/core/transport/hertzbridge"
)

// NewServerTransport 把服务端 RequestContext 适配为 Transporter。
// Kind=http；Endpoint 为 Host；Operation 为请求 PATH。
// 返回的 Transporter 持有 RequestContext 引用，生命周期与请求一致。
//
// 实现委托 hertzbridge 单源适配，本包不另持实现。
func NewServerTransport(c *app.RequestContext) transport.Transporter {
	return hertzbridge.NewServerTransport(c)
}

// NewServerContext 返回注入服务端 Transporter 的 context。
func NewServerContext(ctx context.Context, c *app.RequestContext) context.Context {
	return hertzbridge.NewServerContext(ctx, c)
}
