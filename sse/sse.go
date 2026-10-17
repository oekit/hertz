// Package sse 提供 Hertz SSE（Server-Sent Events）封装。
//
// 底层即 Hertz 官方实现 github.com/cloudwego/hertz/pkg/protocol/sse
// （教程：https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/sse/）。
// Stream 是官方 sse.Writer 的并发安全便捷封装（JSON 事件、幂等 Close）；
// 需要精细控制事件字段（retry、多行 data 等）时可直接使用 Event/Write，
// 也可绕过本包直接使用官方 API——二者完全互换。
//
// 服务端:
//
//	h.GET("/stream", func(ctx context.Context, c *app.RequestContext) {
//	    // 断线重连时 EventSource 自动携带 Last-Event-ID
//	    if id := sse.LastEventID(c); id != "" {
//	        // 从 id 对应的位置续传...
//	    }
//	    s := sse.NewStream(c)
//	    defer s.Close()
//	    _ = s.Send("progress", map[string]any{"percent": 50})
//	    _ = s.SendRaw("id-1", "message", []byte("hello"))
//	})
//
// 客户端（Hertz 作为 HTTP client）:
//
//	sse.AddAcceptMIME(req)            // Accept: text/event-stream
//	sse.SetLastEventID(req, lastID)   // 有状态续传时告知服务端
//	r, err := sse.NewReader(resp)
//	// 服务端正常断开时 ForEach 返回 nil，其他为 io 错误或 ctx 取消
//	err = r.ForEach(ctx, func(e *sse.Event) error { ... return nil })
//
// 注意（官方约定）：Writer 的生命周期必须与 handler 一致，不要在 handler
// 返回后异步后台写入。
package sse

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
	hertzsse "github.com/cloudwego/hertz/pkg/protocol/sse"
)

// ErrStreamClosed 表示流已关闭，不能再写入事件。
var ErrStreamClosed = errors.New("sse: stream closed")

// Stream 封装官方 hertzsse.Writer，提供便捷的事件发送 API。
//
// 创建后通过 Send 发送事件，结束时必须调用 Close 释放底层资源。
// closed 状态由 mutex 保护，支持并发 Send + Close 而无竞态。
type Stream struct {
	w      *hertzsse.Writer
	mu     sync.Mutex
	closed bool
}

// NewStream 创建一个 SSE 流（底层为官方 hertzsse.NewWriter，
// 自动设置 Content-Type: text/event-stream 与 Cache-Control: no-cache）。
// 调用方在发送完所有事件后必须调用 Close。
func NewStream(c *app.RequestContext) *Stream {
	return &Stream{w: hertzsse.NewWriter(c)}
}

// Send 发送一个 JSON 序列化的事件。
//
// eventType 为 SSE 事件类型（客户端通过 EventSource.addEventListener 监听），
// data 会被 JSON 序列化为事件数据。data 为 nil 时发送空 data。
func (s *Stream) Send(eventType string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStreamClosed
	}

	var payload []byte
	if data != nil {
		var err error
		payload, err = json.Marshal(data)
		if err != nil {
			return err
		}
	}

	return s.w.WriteEvent("", eventType, payload)
}

// SendRaw 发送原始字节事件（不做序列化）。
//
// 适用于 data 已是字符串/字节切片，或需自定义格式的场景。
// id 为空时不写入 id 字段。
func (s *Stream) SendRaw(id, eventType string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStreamClosed
	}
	return s.w.WriteEvent(id, eventType, data)
}

// KeepAlive 发送一个注释行（:keep-alive），保持连接活跃。
func (s *Stream) KeepAlive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStreamClosed
	}
	return s.w.WriteKeepAlive()
}

// WriteComment 写入一条自定义注释行（":comment"）。
// 注释不会被视为事件，常用于自定义心跳或带语义的探活信息。
func (s *Stream) WriteComment(comment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStreamClosed
	}
	return s.w.WriteComment(comment)
}

// Close 关闭流，发送收尾 chunk 保证优雅退出（官方 Writer.Close 语义）。
// handler 返回后 Hertz 也会自动调用，显式调用可确保时序。
// 调用后不能再发送事件。多次调用是安全的。
func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.w == nil {
		return nil
	}
	return s.w.Close()
}

// LastEventID 返回客户端请求携带的最后事件 ID（Last-Event-ID 头）。
// 浏览器 EventSource / 官方 Reader 断线重连时自动携带，用于有状态续传；
// 首次连接返回空串。等价于官方 hertzsse.GetLastEventID。
func LastEventID(c *app.RequestContext) string {
	return hertzsse.GetLastEventID(&c.Request)
}

// Event 是官方 hertzsse.Event 的类型别名，供需要精细控制事件字段
// （id/type/data/retry）的调用方使用。
type Event = hertzsse.Event

// Write 写入一个完整的 SSE 事件，支持 id/type/data/retry 全部字段。
func (s *Stream) Write(e *Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStreamClosed
	}
	return s.w.Write(e)
}

// --- 客户端侧：官方 API 的显式透出，调用方无需再直接 import hertzsse ---

// Reader 是官方 hertzsse.Reader 的类型别名。
// Reader 自动记录最后读到的事件 ID，可通过 LastEventID() 获取。
type Reader = hertzsse.Reader

// NewReader 基于客户端响应创建 SSE 读取器（等价于官方 hertzsse.NewReader）。
// 响应必须已完成头部接收（Hertz client.Do 返回即满足）。
func NewReader(resp *protocol.Response) (*Reader, error) {
	return hertzsse.NewReader(resp)
}

// AddAcceptMIME 为客户端请求添加 Accept: text/event-stream 头
// （等价于官方 hertzsse.AddAcceptMIME）。
func AddAcceptMIME(req *protocol.Request) {
	hertzsse.AddAcceptMIME(req)
}

// SetLastEventID 为客户端请求设置 Last-Event-ID 头，
// 有状态续传时告知服务端从哪个事件继续（等价于官方 hertzsse.SetLastEventID）。
func SetLastEventID(req *protocol.Request, id string) {
	hertzsse.SetLastEventID(req, id)
}
