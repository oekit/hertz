// Package clientip 把 Hertz 请求适配到 pkg/convert 的客户端 IP 解析原语。
//
// 背景：Hertz 自带的 c.ClientIP() 默认信任**任意来源**的 X-Forwarded-For——直连方
// 可以随意伪造 XFF，于是限流 key / 审计 / 访问控制三处的 IP 口径只能各写一遍、
// 各自漂移（有的从左向右取 XFF 第一个值，把攻击者伪造的值当成客户端 IP）。
//
// 本包把「采信判定」交给 pkg/convert.ResolveClientIP——与 net/http 侧
// pkg/convert.GetClientIP 同一份实现；Hertz 侧只负责把对端地址与代理头取出来。
package clientip

import (
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/pkg/convert"
)

// GetClientIP 按可信代理 CIDR 列表解析 Hertz 请求的客户端 IP。
//
// 采信规则与 pkg/convert.ResolveClientIP 完全一致：
//   - 对端不在可信代理内 ⇒ 忽略一切代理头（只返回对端地址）；
//   - 对端在可信代理内 ⇒ XFF 从右向左跳过可信代理，取第一个非可信 IP；
//     XFF 缺失或整条链都没有合法候选时回退到对端地址；XFF 整条链都可信时回退到最左端。
//
// ⚠️ 本函数**不采信 X-Real-IP**（pkg#106）——X-Real-IP 是单跳头，HTTP 头本身
// 无法区分「代理写的」与「客户端自带的」：大量网关（Envoy / Higress / Traefik）
// 默认不写该头，但会原样透传客户端自带的，于是客户端可以任意改写自己的客户端 IP
// （限流/锁定计数桶被绕过、审计与访问日志的来源 IP 可被改写）。确需采信改用
// GetClientIPWithXRI。
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
		trustedProxies,
	)
}

// GetClientIPWithDefaultProxies 用 pkg/convert 的默认可信代理列表（内网 + 回环）
// 解析客户端 IP——与 net/http 侧 pkg/convert.GetClientIP 的默认口径一致。
func GetClientIPWithDefaultProxies(c *app.RequestContext) string {
	return GetClientIP(c, convert.DefaultTrustedProxies())
}

// GetClientIPWithXRI 在 GetClientIP 的基础上**额外**采信 X-Real-IP。
//
// 仅在部署形态能保证「可信代理会覆写（而非追加/透传）X-Real-IP」时使用——
// 例如 nginx 的 proxy_set_header X-Real-IP $remote_addr（覆写语义）。若可信代理
// 只是透传客户端自带的 X-Real-IP，本函数等同于把客户端 IP 的决定权交给客户端
// （pkg#106）。
func GetClientIPWithXRI(c *app.RequestContext, trustedProxies []string) string {
	if c == nil {
		return ""
	}
	return convert.ResolveClientIPWithXRI(
		remoteAddr(c),
		c.Request.Header.Get("X-Forwarded-For"),
		c.Request.Header.Get("X-Real-IP"),
		trustedProxies,
	)
}

// GetClientIPWithDefaultProxiesAndXRI 用默认可信代理列表并额外采信 X-Real-IP。
//
// 前置条件同 GetClientIPWithXRI：可信代理必须覆写 X-Real-IP。
func GetClientIPWithDefaultProxiesAndXRI(c *app.RequestContext) string {
	return GetClientIPWithXRI(c, convert.DefaultTrustedProxies())
}

// GetClientIPWithSet 用预解析的可信代理集合解析客户端 IP（热路径形态）。
//
// 与 GetClientIP 的采信规则逐字相同，差别只有一处：清单在构造期解析一次，
// 请求期不再逐条 ParseCIDR（pkg#107）。set 为 nil 时等价于「不信任任何代理头」。
func GetClientIPWithSet(c *app.RequestContext, set *convert.TrustedProxySet) string {
	if c == nil {
		return ""
	}
	return convert.ResolveClientIPWithSet(
		remoteAddr(c),
		c.Request.Header.Get("X-Forwarded-For"),
		set,
	)
}

// remoteAddr 取请求的对端地址（"ip:port" 形态）。
// Hertz 的 RequestContext.RemoteAddr 保证非 nil（无连接时给零值地址）。
func remoteAddr(c *app.RequestContext) string {
	return c.RemoteAddr().String()
}
