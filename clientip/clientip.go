// Package clientip 把 Hertz 请求适配到 pkg/convert 的客户端 IP 解析原语。
//
// 背景：Hertz 自带的 c.ClientIP() 默认信任**任意来源**的 X-Forwarded-For——直连方
// 可以随意伪造 XFF，于是限流 key / 审计 / 访问控制三处的 IP 口径只能各写一遍、
// 各自漂移（有的从左向右取 XFF 第一个值，把攻击者伪造的值当成客户端 IP）。
//
// 本包把「采信判定」交给 pkg/convert.ResolveClientIP——与 net/http 侧
// pkg/convert.GetClientIP 同一份实现；Hertz 侧只负责把对端地址与两个代理头取出来。
package clientip

import (
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/pkg/convert"
)

// GetClientIP 按可信代理 CIDR 列表解析 Hertz 请求的客户端 IP。
//
// 采信规则与 pkg/convert.ResolveClientIP 完全一致：
//   - 对端不在可信代理内 ⇒ 忽略一切代理头（XFF / X-Real-IP 都不看），只返回对端地址；
//   - 对端在可信代理内 ⇒ XFF 从右向左跳过可信代理，取第一个非可信 IP；
//     XFF 缺失时回退 X-Real-IP；XFF 整条链都可信时回退到最左端。
//
// trustedProxies 为 nil 或空时只认对端地址（fail-closed：未声明可信来源的 XFF
// 一律不采信）。
func GetClientIP(c *app.RequestContext, trustedProxies []string) string {
	if c == nil {
		return ""
	}
	return convert.ResolveClientIP(
		remoteAddr(c),
		c.Request.Header.Get("X-Forwarded-For"),
		c.Request.Header.Get("X-Real-IP"),
		trustedProxies,
	)
}

// GetClientIPWithDefaultProxies 用 pkg/convert 的默认可信代理列表（内网 + 回环）
// 解析客户端 IP——与 net/http 侧 pkg/convert.GetClientIP 的默认口径一致。
func GetClientIPWithDefaultProxies(c *app.RequestContext) string {
	return GetClientIP(c, convert.DefaultTrustedProxies())
}

// remoteAddr 取请求的对端地址（"ip:port" 形态）。
// Hertz 的 RequestContext.RemoteAddr 保证非 nil（无连接时给零值地址）。
func remoteAddr(c *app.RequestContext) string {
	return c.RemoteAddr().String()
}
