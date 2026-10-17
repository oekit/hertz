package render

import (
	"fmt"

	"github.com/cloudwego/hertz/pkg/app/server/render"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/vmihailenco/msgpack/v5"
)

// msgpackContentType 是 MessagePack over HTTP 的标准 Content-Type(由
// msgpack-rpc 社区约定;hertz 没有内置,业务可调 WithContentType 改写)。
const msgpackContentType = "application/x-msgpack"

// MessagePack 用 MessagePack 二进制编码响应体。业务给 Data(任意 Go
// 值),自动 msgpack 编码 + 写入 resp。
//
// 用法:
//
//	ctx.Render(200, hertzrender.MessagePack{Data: payload})
//
// 优势:二进制编码体积比 JSON 小 30%~50%,适合 IoT / 移动端弱网场景。
//
// 官方依据:https://github.com/vmihailenco/msgpack(v5 Go 实现)。
type MessagePack struct {
	Data any
}

// Render 实现 render.Render interface。
func (r MessagePack) Render(resp *protocol.Response) error {
	r.WriteContentType(resp)
	body, err := msgpack.Marshal(r.Data)
	if err != nil {
		return fmt.Errorf("hertzrender: MessagePack marshal: %w", err)
	}
	resp.AppendBody(body)
	return nil
}

// WriteContentType 实现 render.Render interface。
func (r MessagePack) WriteContentType(resp *protocol.Response) {
	resp.Header.SetContentType(msgpackContentType)
}

// 确保 MessagePack 满足 render.Render(编译期检查)。
var _ render.Render = MessagePack{}
