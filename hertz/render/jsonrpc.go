// Package render 提供业务特定的 hertz Render interface 实现,与 hertz
// 官方 render 子包(https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/render/)
// 风格一致。业务在 handler 调 ctx.Render(code, hertzrender.JSONRPC{...}) 即可。
//
// 官方依据:https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/render/
// 锚点"自定义渲染"段:
//
//	type Render interface {
//	    Render(resp *protocol.Response) error
//	    WriteContentType(resp *protocol.Response)
//	}
//
// 官方 JSON/XML/HTML Render 在 hertz-contrib,hertz-render 子包内置;本包
// 补全业务特定协议(JSON-RPC 2.0 / MessagePack / CSV),ekit 仓库 0 业务
// 假设(纯类型 + 编码)。
package render

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/hertz/pkg/app/server/render"
	"github.com/cloudwego/hertz/pkg/protocol"
)

// jsonRPCContentType 是 JSON-RPC 2.0 over HTTP 的标准 Content-Type。
const jsonRPCContentType = "application/json"

// JSONRPCVersion 是 JSON-RPC 2.0 协议版本号常量,业务可不显式指定 Version。
const JSONRPCVersion = "2.0"

// JSONRPCError 描述 JSON-RPC 2.0 错误对象。Code 必须是整数,Message 是
// 人类可读描述,Data 是可选的额外上下文。详细见 JSON-RPC 2.0 spec
// (https://www.jsonrpc.org/specification)。
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSONRPC 是 JSON-RPC 2.0 响应包装。Result 与 Error 互斥:成功时 Result
// 非 nil,Error 为 nil;失败时 Error 非 nil,Result 为 nil。
//
// 用法:
//
//	ctx.Render(200, hertzrender.JSONRPC{
//	    Result: map[string]any{"pong": true},
//	    ID:     1,
//	})
//
// 或错误响应:
//
//	ctx.Render(200, hertzrender.JSONRPC{
//	    Error: &hertzrender.JSONRPCError{Code: -32601, Message: "Method not found"},
//	    ID:    1,
//	})
type JSONRPC struct {
	Version string
	Result  any
	Error   *JSONRPCError
	ID      any
}

// jsonRPCEnvelope 是 JSON-RPC 2.0 实际线协议格式(内部用,不导出)。
// Result 用 RawMessage + omitempty：成功响应必须携带 result（void 过程
// 也要有 "result":null，规范禁止两者皆无），失败响应必须只带 error。
type jsonRPCEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
	ID      any             `json:"id"`
}

// Render 实现 render.Render interface。
func (r JSONRPC) Render(resp *protocol.Response) error {
	// hertz ctx.Render 不会自动调 WriteContentType(只有 bodyAllowedForStatus
	// 为 false 才调),所以这里显式设 Content-Type,跟 hertz 内置 JSONRender
	// 行为一致。
	r.WriteContentType(resp)
	if r.Version == "" {
		r.Version = JSONRPCVersion
	}
	env := jsonRPCEnvelope{
		JSONRPC: r.Version,
		ID:      r.ID,
	}
	if r.Error != nil {
		env.Error = r.Error
	} else {
		// void/nil Result 也编码为字面 null，保证成功响应始终有 result 字段。
		raw, err := json.Marshal(r.Result)
		if err != nil {
			return fmt.Errorf("hertzrender: JSONRPC marshal: %w", err)
		}
		env.Result = raw
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("hertzrender: JSONRPC marshal: %w", err)
	}
	resp.AppendBody(body)
	return nil
}

// WriteContentType 实现 render.Render interface。
func (r JSONRPC) WriteContentType(resp *protocol.Response) {
	resp.Header.SetContentType(jsonRPCContentType)
}

// 确保 JSONRPC 满足 render.Render(编译期检查)。
var _ render.Render = JSONRPC{}
