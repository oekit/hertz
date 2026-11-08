package clientip

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/oekit/pkg/convert"
)

// TestGetClientIP_NilContext 空上下文不 panic，返回空串。
func TestGetClientIP_NilContext(t *testing.T) {
	if got := GetClientIP(nil, nil); got != "" {
		t.Errorf("GetClientIP(nil) = %q, want empty string", got)
	}
	if got := GetClientIPWithDefaultProxies(nil); got != "" {
		t.Errorf("GetClientIPWithDefaultProxies(nil) = %q, want empty string", got)
	}
}

// TestGetClientIP_UntrustedPeerIgnoresProxyHeaders 直连方（对端不在可信网段内）
// 伪造的代理头一律不采信——这是采信口径的核心防线。
func TestGetClientIP_UntrustedPeerIgnoresProxyHeaders(t *testing.T) {
	c := app.NewContext(0)
	c.Request.Header.Set("X-Forwarded-For", "9.9.9.9, 1.2.3.4")
	c.Request.Header.Set("X-Real-IP", "8.8.8.8")

	// app.NewContext 造出的上下文没有连接，对端地址是 0.0.0.0:0 —— 不可信。
	if got := GetClientIP(c, nil); got != "0.0.0.0" {
		t.Errorf("GetClientIP(untrusted peer) = %q, want 0.0.0.0 (proxy headers must be ignored)", got)
	}
	if got := GetClientIP(c, []string{"10.0.0.0/8"}); got != "0.0.0.0" {
		t.Errorf("GetClientIP(untrusted peer) = %q, want 0.0.0.0 (proxy headers must be ignored)", got)
	}

	// 对端可信时才采信 XFF（且从右向左取第一个非可信 IP）。
	if got := GetClientIP(c, []string{"0.0.0.0/8"}); got != "1.2.3.4" {
		t.Errorf("GetClientIP(trusted peer) = %q, want 1.2.3.4 (rightmost non-trusted XFF)", got)
	}
}

// startIPServer 启动一个回显客户端 IP 的测试服务器，返回地址与关闭函数。
// resolve 决定服务端用哪种口径解析客户端 IP。
func startIPServer(t *testing.T, resolve func(*app.RequestContext) string) (addr string, cleanup func()) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	opt := config.NewOptions([]config.Option{})
	opt.Listener = ln
	engine := route.NewEngine(opt)
	engine.GET("/ip", func(ctx context.Context, c *app.RequestContext) {
		c.String(200, "%s", resolve(c))
	})
	go func() {
		_ = engine.Run()
	}()
	addr = ln.Addr().String()
	time.Sleep(30 * time.Millisecond)
	return addr, func() {
		_ = engine.Close()
		_ = ln.Close()
	}
}

// requestIP 向测试服务器发请求（可带 XFF / X-Real-IP 头），返回服务端解析出的 IP。
func requestIP(t *testing.T, addr, xff, xri string) string {
	t.Helper()
	c, err := client.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	req, resp := protocol.AcquireRequest(), protocol.AcquireResponse()
	defer func() {
		protocol.ReleaseRequest(req)
		protocol.ReleaseResponse(resp)
	}()
	req.SetRequestURI("http://" + addr + "/ip")
	req.SetMethod("GET")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if xri != "" {
		req.Header.Set("X-Real-IP", xri)
	}
	if err := c.Do(context.Background(), req, resp); err != nil {
		t.Fatalf("do request: %v", err)
	}
	return string(resp.Body())
}

// TestGetClientIP_RealRequest 真实请求路径：客户端来自 127.0.0.1（默认可信网段内），
// 服务端采信 XFF；伪造值在最左端时被忽略，取的是从右向左的第一个非可信 IP。
func TestGetClientIP_RealRequest(t *testing.T) {
	addr, cleanup := startIPServer(t, func(c *app.RequestContext) string {
		return GetClientIP(c, []string{"127.0.0.0/8"})
	})
	defer cleanup()

	if got := requestIP(t, addr, "9.9.9.9, 1.2.3.4, 127.0.0.1", ""); got != "1.2.3.4" {
		t.Errorf("clientip = %q, want 1.2.3.4 (rightmost non-trusted XFF; forged 9.9.9.9 ignored)", got)
	}
	// ⚠️ 默认形态**不采信 X-Real-IP**（pkg#106）：单跳头无法区分「代理写的」与
	// 「客户端自带的」，网关默认透传客户端自带的该头。
	if got := requestIP(t, addr, "", "5.6.7.8"); got != "127.0.0.1" {
		t.Errorf("clientip = %q, want 127.0.0.1 (X-Real-IP must not be trusted by default)", got)
	}
	if got := requestIP(t, addr, "", ""); got != "127.0.0.1" {
		t.Errorf("clientip = %q, want 127.0.0.1 (peer address, no proxy headers)", got)
	}
}

// TestGetClientIPWithXRI_RealRequest 显式开启后才采信 X-Real-IP。
func TestGetClientIPWithXRI_RealRequest(t *testing.T) {
	addr, cleanup := startIPServer(t, func(c *app.RequestContext) string {
		return GetClientIPWithXRI(c, []string{"127.0.0.0/8"})
	})
	defer cleanup()

	if got := requestIP(t, addr, "", "5.6.7.8"); got != "5.6.7.8" {
		t.Errorf("clientip = %q, want 5.6.7.8 (X-Real-IP honored when explicitly enabled)", got)
	}
	// XFF 优先于 X-Real-IP。
	if got := requestIP(t, addr, "1.2.3.4", "5.6.7.8"); got != "1.2.3.4" {
		t.Errorf("clientip = %q, want 1.2.3.4 (XFF wins over X-Real-IP)", got)
	}
}

// TestGetClientIP_NilContext 的 XRI 变体同样不 panic。
func TestGetClientIPWithXRI_NilContext(t *testing.T) {
	if got := GetClientIPWithXRI(nil, nil); got != "" {
		t.Errorf("GetClientIPWithXRI(nil) = %q, want empty string", got)
	}
	if got := GetClientIPWithDefaultProxiesAndXRI(nil); got != "" {
		t.Errorf("GetClientIPWithDefaultProxiesAndXRI(nil) = %q, want empty string", got)
	}
	if got := GetClientIPWithSet(nil, nil); got != "" {
		t.Errorf("GetClientIPWithSet(nil) = %q, want empty string", got)
	}
}

// TestGetClientIP_RealRequestUntrustedPeer 可信网段不含对端时，代理头一律不采信。
func TestGetClientIP_RealRequestUntrustedPeer(t *testing.T) {
	addr, cleanup := startIPServer(t, func(c *app.RequestContext) string {
		return GetClientIP(c, []string{"10.0.0.0/8"})
	})
	defer cleanup()

	if got := requestIP(t, addr, "9.9.9.9, 1.2.3.4", ""); got != "127.0.0.1" {
		t.Errorf("clientip = %q, want 127.0.0.1 (untrusted peer must not use proxy headers)", got)
	}
}

// TestGetClientIPWithDefaultProxies_RealRequest 默认可信网段含 127.0.0.0/8，
// 与 net/http 侧 pkg/convert.GetClientIP 的默认口径一致。
func TestGetClientIPWithDefaultProxies_RealRequest(t *testing.T) {
	addr, cleanup := startIPServer(t, GetClientIPWithDefaultProxies)
	defer cleanup()

	if got := requestIP(t, addr, "9.9.9.9, 1.2.3.4, 127.0.0.1", ""); got != "1.2.3.4" {
		t.Errorf("clientip = %q, want 1.2.3.4 (default trusted proxies include 127.0.0.0/8)", got)
	}
	if got := requestIP(t, addr, "", ""); got != "127.0.0.1" {
		t.Errorf("clientip = %q, want 127.0.0.1 (peer address, no proxy headers)", got)
	}
}

// --- #174 回归：默认变体按清单指纹缓存（每份清单只解析一次），非法条目 fail-fast ---
//
// 此前 GetClientIP / GetClientIPWithXRI 把 []string 直接转给
// convert.ResolveClientIP，后者每次调用都把整份清单 ParseCIDR 一遍
// （每请求热路径开销），且非法条目错误被吞掉（静默降级为「不信任任何代理头」）。

// mustPanicValue 断言 fn panic 并返回 panic 值（fail-fast 语义的测试钩子）。
func mustPanicValue(t *testing.T, fn func()) any {
	t.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		fn()
	}()
	if recovered == nil {
		t.Fatal("expected panic, got none")
	}
	return recovered
}

// TestGetClientIP_InvalidEntryFailsFast 清单含非法条目时默认变体必须 fail-fast
// （panic），不得静默跳过——静默跳过把「配置写错」变成「安全策略降级」且不可观测
// （pkg#104 的形态）。
func TestGetClientIP_InvalidEntryFailsFast(t *testing.T) {
	c := app.NewContext(0)
	v := mustPanicValue(t, func() {
		_ = GetClientIP(c, []string{"10.0.0.0/8", "not-a-cidr"})
	})
	msg, ok := v.(string)
	if !ok {
		t.Fatalf("panic value = %T, want string", v)
	}
	if !strings.Contains(msg, "not-a-cidr") {
		t.Errorf("panic message %q must mention the invalid entry", msg)
	}
	// 错误已缓存：第二次调用（同清单）恒 fail-fast，行为一致。
	v2 := mustPanicValue(t, func() {
		_ = GetClientIP(c, []string{"10.0.0.0/8", "not-a-cidr"})
	})
	if v2.(string) != msg {
		t.Errorf("cached failure must be deterministic: %q vs %q", v2, msg)
	}
}

// TestGetClientIPWithXRI_InvalidEntryFailsFast XRI 变体同样 fail-fast。
func TestGetClientIPWithXRI_InvalidEntryFailsFast(t *testing.T) {
	c := app.NewContext(0)
	v := mustPanicValue(t, func() {
		_ = GetClientIPWithXRI(c, []string{"203.0.113.5/33"})
	})
	if !strings.Contains(v.(string), "203.0.113.5/33") {
		t.Errorf("panic message %q must mention the invalid entry", v)
	}
}

// TestGetClientIPWithDefaultProxies_Valid 内置默认清单必须可解析（否则默认
// 变体在修复后每次调用都 panic）。
func TestGetClientIPWithDefaultProxies_Valid(t *testing.T) {
	c := app.NewContext(0)
	if got := GetClientIPWithDefaultProxies(c); got != "0.0.0.0" {
		t.Errorf("GetClientIPWithDefaultProxies = %q, want 0.0.0.0", got)
	}
}

// TestGetClientIP_SemanticsAlignedWithWithSet 清单变体（缓存形态）与预构造变体
// 的采信语义必须逐条对齐（含 XFF 从右向左扫描、非法段跳过、整链可信回退最左端、
// 空清单只认对端）。
func TestGetClientIP_SemanticsAlignedWithWithSet(t *testing.T) {
	c := app.NewContext(0)
	c.Request.Header.Set("X-Forwarded-For", "9.9.9.9, 1.2.3.4, 10.0.0.7")
	c.Request.Header.Set("X-Real-IP", "8.8.8.8")

	lists := [][]string{
		nil,
		{},
		{"10.0.0.0/8"},
		{"0.0.0.0/8"},
		{"10.0.0.0/8", "0.0.0.0/0"},
		{"203.0.113.5"}, // 裸 IP 归一为单主机网段
	}
	for _, list := range lists {
		set, err := convert.NewTrustedProxySet(list)
		if err != nil {
			t.Fatalf("NewTrustedProxySet(%v): %v", list, err)
		}
		want := GetClientIPWithSet(c, set)
		if got := GetClientIP(c, list); got != want {
			t.Errorf("GetClientIP(%v) = %q, want %q (aligned with GetClientIPWithSet)", list, got, want)
		}
		// XRI 变体与 convert 的 WithSetAndXRI 对齐。
		wantXRI := convert.ResolveClientIPWithSetAndXRI(
			remoteAddr(c), c.Request.Header.Get("X-Forwarded-For"),
			c.Request.Header.Get("X-Real-IP"), set)
		if got := GetClientIPWithXRI(c, list); got != wantXRI {
			t.Errorf("GetClientIPWithXRI(%v) = %q, want %q", list, got, wantXRI)
		}
	}

	// 语义钉：对端可信时 XFF 从右向左取第一个非可信 IP。
	if got := GetClientIP(c, []string{"0.0.0.0/0"}); got != "9.9.9.9" {
		t.Errorf("all-trusted XFF = %q, want 9.9.9.9 (leftmost original client)", got)
	}
	// 对端（0.0.0.0）可信、10.0.0.7 可信（10/8）⇒ 取 1.2.3.4（右起第一个非可信）。
	if got := GetClientIP(c, []string{"10.0.0.0/8", "0.0.0.0/32"}); got != "1.2.3.4" {
		t.Errorf("partial trusted = %q, want 1.2.3.4 (rightmost non-trusted)", got)
	}
}

// TestTrustedProxySetFor_CachesByContent 同一份清单只解析一次；清单变更
// （内容变化）即刻可见——缓存的键是清单内容，不是「第一次见到的清单」。
func TestTrustedProxySetFor_CachesByContent(t *testing.T) {
	listA := []string{"10.0.0.0/8"}
	listB := []string{"10.0.0.0/8", "0.0.0.0/32"} // 追加对端（0.0.0.0）网段

	setA1 := trustedProxySetFor(listA)
	setA2 := trustedProxySetFor(listA)
	if setA1 != setA2 {
		t.Error("same list must reuse the cached set (pointer identity)")
	}
	if _, err := lookupProxySetBucket(getOrCreateProxySetBucket(proxyListHash(listA)), listA); err != nil || setA1 == nil {
		t.Error("list A must be cached")
	}

	// 调用方事后改写自己的切片，不得污染缓存判定（缓存键持深拷贝）。
	mutated := []string{"10.0.0.0/8"}
	if trustedProxySetFor(mutated) != setA1 {
		t.Error("equal-content list must hit the cache")
	}
	mutated[0] = "bogus-not-a-cidr"
	if trustedProxySetFor([]string{"10.0.0.0/8"}) != setA1 {
		t.Error("cache must hold its own deep copy (caller slice mutation must not corrupt it)")
	}

	// 清单变更即刻可见：对端 0.0.0.0 在 A 下不可信（XFF 不采信）、在 B 下可信
	// （XFF 从右向左取第一个非可信 IP）。
	c := app.NewContext(0)
	c.Request.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := GetClientIP(c, listA); got != "0.0.0.0" {
		t.Errorf("listA: clientip = %q, want 0.0.0.0 (peer untrusted, XFF ignored)", got)
	}
	if got := GetClientIP(c, listB); got != "1.2.3.4" {
		t.Errorf("listB: clientip = %q, want 1.2.3.4 (peer trusted, XFF honored)", got)
	}
}

// TestTrustedProxySetFor_Concurrent 并发调用（不同清单）结果恒正确——缓存
// 读写必须并发安全（-race 下验证）。
func TestTrustedProxySetFor_Concurrent(t *testing.T) {
	lists := [][]string{
		{"10.0.0.0/8"},
		{"0.0.0.0/0"},
		{"172.16.0.0/12", "192.168.0.0/16"},
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			list := lists[i%len(lists)]
			c := app.NewContext(0)
			c.Request.Header.Set("X-Forwarded-For", "1.2.3.4")
			_ = GetClientIPWithXRI(c, list)
			// 重复调用必须命中缓存且结果一致。
			first := GetClientIP(c, list)
			second := GetClientIP(c, list)
			if first != second {
				t.Errorf("inconsistent result across cache hits: %q vs %q", first, second)
			}
		}(i)
	}
	wg.Wait()
}

// TestTrustedProxySetFor_CacheLimit 缓存条目达到上限后退化为逐请求解析
// （正确性不变），防御病态用法打爆内存。
func TestTrustedProxySetFor_CacheLimit(t *testing.T) {
	// 先把缓存填满（一条 entry 一个极小清单，解析成本可忽略）。
	for i := 0; proxySetCacheSize.Load() < proxySetCacheLimit; i++ {
		_ = trustedProxySetFor([]string{fmt.Sprintf("10.%d.%d.%d/32", (i>>16)&0xff, (i>>8)&0xff, i&0xff)})
	}
	// 上限之后的清单不再入缓存，但仍解析正确。
	list := []string{"203.0.113.0/24"}
	_ = trustedProxySetFor(list)
	if set, err := lookupProxySetBucket(getOrCreateProxySetBucket(proxyListHash(list)), list); set != nil || err != nil {
		t.Error("entries beyond the limit must not be cached")
	}
	c := app.NewContext(0)
	if got := GetClientIP(c, list); got != "0.0.0.0" {
		t.Errorf("beyond-limit list must still resolve correctly, got %q", got)
	}
}

// --- Resolver：预构造形态（构造期严格解析 + error，热路径零清单处理） ---

func TestResolver_NewStrict(t *testing.T) {
	if _, err := New([]string{"10.0.0.0/8", "oops"}); err == nil {
		t.Error("New must reject invalid entries with an error (construction-time fail-fast)")
	}
	if _, err := New(nil); err != nil {
		t.Errorf("New(nil) must be legal (fail-closed), got %v", err)
	}
}

func TestResolver_Semantics(t *testing.T) {
	r, err := New([]string{"0.0.0.0/0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := app.NewContext(0)
	c.Request.Header.Set("X-Forwarded-For", "9.9.9.9, 1.2.3.4")
	c.Request.Header.Set("X-Real-IP", "8.8.8.8")
	if got := r.GetClientIP(c); got != "9.9.9.9" {
		t.Errorf("Resolver.GetClientIP = %q, want 9.9.9.9 (leftmost original client)", got)
	}
	// New 构造（未声明 XRI 口径）：GetClientIPWithXRI 不得静默开启 XRI 采信。
	if got := r.GetClientIPWithXRI(c); got != "9.9.9.9" {
		t.Errorf("GetClientIPWithXRI on non-XRI resolver = %q, want 9.9.9.9", got)
	}

	rx, err := NewWithXRI([]string{"0.0.0.0/0"})
	if err != nil {
		t.Fatalf("NewWithXRI: %v", err)
	}
	if got := rx.GetClientIP(c); got != "9.9.9.9" {
		t.Errorf("GetClientIP must ignore X-Real-IP even on XRI resolver, got %q", got)
	}
	if got := rx.GetClientIPWithXRI(c); got != "9.9.9.9" {
		t.Errorf("GetClientIPWithXRI = %q, want 9.9.9.9 (XFF wins over X-Real-IP)", got)
	}

	// XFF 缺失时 XRI 变体才用 X-Real-IP。
	c2 := app.NewContext(0)
	c2.Request.Header.Set("X-Real-IP", "8.8.8.8")
	if got := rx.GetClientIPWithXRI(c2); got != "8.8.8.8" {
		t.Errorf("GetClientIPWithXRI(no XFF) = %q, want 8.8.8.8", got)
	}
	if got := r.GetClientIP(c2); got != "0.0.0.0" {
		t.Errorf("GetClientIP(no XFF) = %q, want 0.0.0.0 (X-Real-IP untrusted)", got)
	}
}

func TestResolver_NilSafe(t *testing.T) {
	c := app.NewContext(0)
	c.Request.Header.Set("X-Forwarded-For", "1.2.3.4")
	// nil set / nil resolver：fail-closed（不信任任何代理头，与 GetClientIPWithSet(nil) 一致）。
	if got := NewFromSet(nil).GetClientIP(c); got != "0.0.0.0" {
		t.Errorf("NewFromSet(nil).GetClientIP = %q, want 0.0.0.0", got)
	}
	var nilResolver *Resolver
	if got := nilResolver.GetClientIP(c); got != "0.0.0.0" {
		t.Errorf("nil resolver GetClientIP = %q, want 0.0.0.0 (fail-closed)", got)
	}
	if got := nilResolver.GetClientIP(nil); got != "" {
		t.Errorf("nil resolver + nil ctx = %q, want empty", got)
	}
	// NewFromSet 复用已有集合：与包级 WithSet 变体逐字一致。
	set, err := convert.NewTrustedProxySet([]string{"0.0.0.0/0"})
	if err != nil {
		t.Fatalf("NewTrustedProxySet: %v", err)
	}
	if got := NewFromSet(set).GetClientIP(c); got != GetClientIPWithSet(c, set) {
		t.Errorf("NewFromSet semantics diverge from GetClientIPWithSet: %q vs %q",
			NewFromSet(set).GetClientIP(c), GetClientIPWithSet(c, set))
	}
}

// --- 基准（#174 验收判据：量化清单形态 vs 预构造形态） ---

// benchList 是 100 条 CIDR 的可信代理清单（多级网关 + 云 LB 段的典型规模）。
var benchList = func() []string {
	out := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		out = append(out, fmt.Sprintf("10.%d.%d.0/24", i/256, i%256))
	}
	return out
}()

// BenchmarkGetClientIP_ListVariant_CacheHit 缓存命中路径：每请求只剩指纹计算
// + 网段包含判定。
func BenchmarkGetClientIP_ListVariant_CacheHit(b *testing.B) {
	c := app.NewContext(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = GetClientIP(c, benchList)
	}
}

// BenchmarkGetClientIPWithSet_PreParsed 预构造基线：WithSet / Resolver 形态。
func BenchmarkGetClientIPWithSet_PreParsed(b *testing.B) {
	set, err := convert.NewTrustedProxySet(benchList)
	if err != nil {
		b.Fatalf("NewTrustedProxySet: %v", err)
	}
	c := app.NewContext(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = GetClientIPWithSet(c, set)
	}
}

// BenchmarkGetClientIP_ParseEveryRequest 模拟修复前形态：每请求把整份清单
// ParseCIDR 一遍（pkg/convert.ResolveClientIP 的清单路径）。
func BenchmarkGetClientIP_ParseEveryRequest(b *testing.B) {
	c := app.NewContext(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = convert.ResolveClientIP(remoteAddr(c), c.Request.Header.Get("X-Forwarded-For"), benchList)
	}
}
