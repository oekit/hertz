// Package transportx 把 Hertz RequestContext 适配为 core/transport 抽象。
//
// 经 NewServerContext 注入后，框架无关的中间件（core/chain + middleware/chainmw）
// 即可协议无关地读写 header、读取 Kind/Endpoint/Operation。
package transportx

import (
	"context"
	"sort"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/oekit/core/transport"
)

// NewServerTransport 把服务端 RequestContext 适配为 Transporter。
// Kind=http；Endpoint 为 Host；Operation 为请求 PATH。
// 返回的 Transporter 持有 RequestContext 引用，生命周期与请求一致。
func NewServerTransport(c *app.RequestContext) transport.Transporter {
	return &serverTransport{c: c}
}

type serverTransport struct {
	c *app.RequestContext
}

func (t *serverTransport) Kind() transport.Kind { return transport.KindHTTP }

func (t *serverTransport) Endpoint() string { return string(t.c.Host()) }

func (t *serverTransport) Operation() string { return string(t.c.Path()) }

func (t *serverTransport) RequestHeader() transport.Header {
	return &requestHeader{h: &t.c.Request.Header}
}

func (t *serverTransport) ReplyHeader() transport.Header {
	return &replyHeader{h: &t.c.Response.Header}
}

// requestHeader 适配 hertz RequestHeader。
type requestHeader struct {
	h *protocol.RequestHeader
}

func (a *requestHeader) Get(key string) string { return a.h.Get(key) }

func (a *requestHeader) Values(key string) []string {
	var out []string
	a.h.VisitAll(func(k, v []byte) {
		if string(k) == key {
			out = append(out, string(v))
		}
	})
	return out
}

func (a *requestHeader) Set(key, value string) { a.h.Set(key, value) }

func (a *requestHeader) Add(key, value string) { a.h.Add(key, value) }

func (a *requestHeader) Del(key string) { a.h.Del(key) }

func (a *requestHeader) Keys() []string {
	seen := make(map[string]struct{})
	a.h.VisitAll(func(k, _ []byte) {
		seen[string(k)] = struct{}{}
	})
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// replyHeader 适配 hertz ResponseHeader。
type replyHeader struct {
	h *protocol.ResponseHeader
}

func (a *replyHeader) Get(key string) string { return a.h.Get(key) }

func (a *replyHeader) Values(key string) []string {
	var out []string
	a.h.VisitAll(func(k, v []byte) {
		if string(k) == key {
			out = append(out, string(v))
		}
	})
	return out
}

func (a *replyHeader) Set(key, value string) { a.h.Set(key, value) }

func (a *replyHeader) Add(key, value string) { a.h.Set(key, value) }

func (a *replyHeader) Del(key string) { a.h.Del(key) }

func (a *replyHeader) Keys() []string {
	seen := make(map[string]struct{})
	a.h.VisitAll(func(k, _ []byte) {
		seen[string(k)] = struct{}{}
	})
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// NewServerContext 返回注入服务端 Transporter 的 context。
func NewServerContext(ctx context.Context, c *app.RequestContext) context.Context {
	return transport.NewServerContext(ctx, NewServerTransport(c))
}
