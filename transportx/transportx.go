// Package transportx 把 Hertz RequestContext 适配为 core/transport 抽象。
//
// 经 NewServerContext 注入后，框架无关的中间件（core/chain + middleware/chainmw）
// 即可协议无关地读写 header、读取 Kind/Endpoint/Operation。
package transportx

import (
	"context"
	"sort"
	"strings"

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

// Get 与 Values 键匹配同口径（#163）：hertz 存取键均规范化为 MIME canonical
// 形态（NormalizeHeaderKey），Get 大小写不敏感命中。
func (a *requestHeader) Get(key string) string { return a.h.Get(key) }

// Values 返回 key 的全部值，键匹配与 Get 同一口径——大小写不敏感（#163）。
//
// 此前为原文比较：VisitAll 产出的键是 hertz 规范化后的 canonical 形态
// （如 "X-Trace-Id"），调用方传 "x-trace-id" 这类大小写变体时 Get 命中、
// Values 返回 nil——同一适配器两套键口径，Get 命中而 Values 落空。
func (a *requestHeader) Values(key string) []string {
	var out []string
	a.h.VisitAll(func(k, v []byte) {
		if strings.EqualFold(string(k), key) {
			out = append(out, string(v))
		}
	})
	return out
}

func (a *requestHeader) Set(key, value string) { a.h.Set(key, value) }

func (a *requestHeader) Add(key, value string) { a.h.Add(key, value) }

func (a *requestHeader) Del(key string) { a.h.Del(key) }

// Keys 返回全部键（已排序，保证确定性）。键为 hertz 规范化的 MIME canonical
// 形态（如 Set("X-Trace-ID",…) 后 Keys() 产出 "X-Trace-Id"）——消费方应以
// 大小写不敏感方式回读（Get/Values 与 Get 同口径，#163），不要拿 Keys() 的
// 键做原文匹配。
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

// Get 与 Values 键匹配同口径（#163）：hertz 存取键均规范化为 MIME canonical
// 形态，Get 大小写不敏感命中。
func (a *replyHeader) Get(key string) string { return a.h.Get(key) }

// Values 返回 key 的全部值，键匹配与 Get 同一口径——大小写不敏感（#163，
// requestHeader.Values 同病同修）。
func (a *replyHeader) Values(key string) []string {
	var out []string
	a.h.VisitAll(func(k, v []byte) {
		if strings.EqualFold(string(k), key) {
			out = append(out, string(v))
		}
	})
	return out
}

func (a *replyHeader) Set(key, value string) { a.h.Set(key, value) }

// Add 追加（同 key 多值）——与 requestHeader.Add 及 core/transport 的
// Header.Add 语义一致；此前误用 Set（覆盖），后写的值会顶掉先写的值。
func (a *replyHeader) Add(key, value string) { a.h.Add(key, value) }

func (a *replyHeader) Del(key string) { a.h.Del(key) }

// Keys 返回全部键（已排序，保证确定性）；键为 hertz 规范化的 MIME canonical
// 形态，消费方应以大小写不敏感方式回读（Get/Values 与 Get 同口径，#163）。
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
