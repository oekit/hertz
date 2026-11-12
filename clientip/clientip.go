// Package clientip 把 Hertz 请求适配到 pkg/convert 的客户端 IP 解析原语。
//
// 背景：Hertz 自带的 c.ClientIP() 默认信任**任意来源**的 X-Forwarded-For——直连方
// 可以随意伪造 XFF，于是限流 key / 审计 / 访问控制三处的 IP 口径只能各写一遍、
// 各自漂移（有的从左向右取 XFF 第一个值，把攻击者伪造的值当成客户端 IP）。
//
// 本包把「采信判定」交给 pkg/convert.ResolveClientIP——与 net/http 侧
// pkg/convert.GetClientIP 同一份实现；Hertz 侧只负责把对端地址与代理头取出来。
//
// 热路径形态（#174/#237）：清单形态的 GetClientIP / GetClientIPWithXRI 按清单
// 身份（切片底层数组 + 长度）做 O(1) 快缓存，每份清单只 ParseCIDR 一次；缓存
// 命中后请求期只剩网段包含判定。同一底层数组的清单被逐请求传入（启动期配置的
// 标准形态）时零哈希、零深比较；调用方每请求构造新切片时退到内容指纹缓存
// （仍免逐条 ParseCIDR）。前提：清单传入后不得原地改写条目——按身份缓存，
// 改写会拿到旧集合，变更清单请构造新切片。更高要求的场景（清单在配置加载期
// 可得）推荐 [New] / [NewWithXRI] 预构造 [Resolver]——构造期严格解析、非法
// 清单返回 error，不经过请求路径。
package clientip

import (
	"encoding/binary"
	"hash/maphash"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/logx"
	"github.com/oekit/pkg/convert"
)

// clientipLog 是本包日志门面（懒解析；AGENTS.md：SDK/基础包必须 Delegating）。
var clientipLog = logx.Delegating("clientip")

// GetClientIP 按可信代理 CIDR 列表解析 Hertz 请求的客户端 IP。
//
// 采信规则与 pkg/convert.ResolveClientIPWithSet 完全一致：
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
//
// 性能与失败语义（#174/#224/#237）：清单按身份/内容两级缓存，每份清单只在
// 首次调用时解析一次，缓存命中后请求期只剩网段包含判定；解析为**严格模式**
// ——清单含非法条目时 fail-closed（不信任任何代理头，回退对端地址）并每份
// 坏清单告警一次。不 panic（#224）：panic 落在请求热路径，Recovery 缺失时
// 冲垮连接、存在时每请求 500 + panic 栈噪声。需要在启动期拿到错误而非运行期
// 降级的，用 [New]（返回 error）；热路径拿得到预解析集合的，用
// [GetClientIPWithSet]。
func GetClientIP(c *app.RequestContext, trustedProxies []string) string {
	if c == nil {
		return ""
	}
	return convert.ResolveClientIPWithSet(
		remoteAddr(c),
		c.Request.Header.Get("X-Forwarded-For"),
		trustedProxySetFor(trustedProxies),
	)
}

// GetClientIPWithDefaultProxies 用 pkg/convert 的默认可信代理列表（内网 + 回环）
// 解析客户端 IP——与 net/http 侧 pkg/convert.GetClientIP 的默认口径一致。
func GetClientIPWithDefaultProxies(c *app.RequestContext) string {
	return GetClientIP(c, defaultTrustedProxiesList)
}

// defaultTrustedProxiesList 是 convert.DefaultTrustedProxies() 的包内单一副本
// （#237 附带项）：pkg 侧每次调用都新拷贝一份清单，逐请求复制既多一次分配，
// 又让默认清单永远吃不到身份快路径。包内持有一份固定切片后，默认变体与显式
// 清单同样享受 O(1) 身份命中（副本归本包持有，外部改写不影响）。
var defaultTrustedProxiesList = cloneStrings(convert.DefaultTrustedProxies())

// GetClientIPWithXRI 在 GetClientIP 的基础上**额外**采信 X-Real-IP。
//
// 仅在部署形态能保证「可信代理会覆写（而非追加/透传）X-Real-IP」时使用——
// 例如 nginx 的 proxy_set_header X-Real-IP $remote_addr（覆写语义）。若可信代理
// 只是透传客户端自带的 X-Real-IP，本函数等同于把客户端 IP 的决定权交给客户端
// （pkg#106）。
//
// 性能与失败语义同 GetClientIP（#174/#224）：清单两级缓存 + 严格解析，非法
// 条目 fail-closed（不信任代理头，回退对端地址）并告警一次，不 panic；启动期
// 要 error 用 [NewWithXRI]。
func GetClientIPWithXRI(c *app.RequestContext, trustedProxies []string) string {
	if c == nil {
		return ""
	}
	return convert.ResolveClientIPWithSetAndXRI(
		remoteAddr(c),
		c.Request.Header.Get("X-Forwarded-For"),
		c.Request.Header.Get("X-Real-IP"),
		trustedProxySetFor(trustedProxies),
	)
}

// GetClientIPWithDefaultProxiesAndXRI 用默认可信代理列表并额外采信 X-Real-IP。
//
// 前置条件同 GetClientIPWithXRI：可信代理必须覆写 X-Real-IP。
func GetClientIPWithDefaultProxiesAndXRI(c *app.RequestContext) string {
	return GetClientIPWithXRI(c, defaultTrustedProxiesList)
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

// Resolver 是预构造的可复用客户端 IP 解析器（热路径推荐形态，#174）。
//
// 清单在构造期解析一次，请求期只做网段包含判定，零 CIDR 解析；本类型不可变，
// 可安全跨 goroutine 共享。采信规则与包级函数逐字一致（含不采信 X-Real-IP 的
// 缺省口径，XRI 变体除外）。
//
// 与包级清单变体的差别：New / NewWithXRI 在**构造期**严格解析并返回 error
// （配置加载期 fail-fail-fast），不经过请求路径——消费方（如 pigeonbox/core）
// 由此不再需要绕开本包自持 TrustedProxySet。
type Resolver struct {
	set      *convert.TrustedProxySet
	trustXRI bool
}

// New 构造解析器：清单在构造期严格解析一次（非法条目返回 error，不静默跳过），
// 请求期零 CIDR 解析。
//
// 采信规则与 GetClientIP 一致（不采信 X-Real-IP）；nil / 空清单合法
// （fail-closed：不信任任何代理头）。
func New(trustedProxies []string) (*Resolver, error) {
	set, err := convert.NewTrustedProxySet(trustedProxies)
	if err != nil {
		return nil, err
	}
	return &Resolver{set: set}, nil
}

// NewWithXRI 构造额外采信 X-Real-IP 的解析器。
//
// 前置条件同 GetClientIPWithXRI：可信代理必须覆写 X-Real-IP，否则客户端可任意
// 改写自己的客户端 IP（pkg#106）。清单解析语义同 [New]（严格、构造期报错）。
func NewWithXRI(trustedProxies []string) (*Resolver, error) {
	set, err := convert.NewTrustedProxySet(trustedProxies)
	if err != nil {
		return nil, err
	}
	return &Resolver{set: set, trustXRI: true}, nil
}

// NewFromSet 用已有预解析集合构造（与 convert.NewTrustedProxySet 组合使用）。
// set 为 nil 时等价于「不信任任何代理头」（与 GetClientIPWithSet 的 nil 口径一致）。
func NewFromSet(set *convert.TrustedProxySet) *Resolver {
	return &Resolver{set: set}
}

// GetClientIP 解析客户端 IP——采信规则与包级 GetClientIP 逐字一致
// （XFF 从右向左、不采信 X-Real-IP）。
func (r *Resolver) GetClientIP(c *app.RequestContext) string {
	return r.resolve(c, false)
}

// GetClientIPWithXRI 解析客户端 IP 并额外采信 X-Real-IP——采信规则与包级
// GetClientIPWithXRI 逐字一致。仅当解析器经 NewWithXRI 构造（或等价的 XRI
// 口径）时才与 GetClientIP 有差异；New/NewFromSet 构造的解析器调用本方法
// 等价于 GetClientIP（不静默开启 XRI 采信）。
func (r *Resolver) GetClientIPWithXRI(c *app.RequestContext) string {
	return r.resolve(c, true)
}

// resolve 是 Resolver 的采信判定本体。wantXRI 为 true 时仅在解析器声明了
// XRI 采信口径（NewWithXRI）才读 X-Real-IP——口径由构造期声明，请求期不 guessing。
func (r *Resolver) resolve(c *app.RequestContext, wantXRI bool) string {
	if c == nil {
		return ""
	}
	if r == nil || r.set == nil {
		// fail-closed：无预解析集合（nil Resolver / nil set）⇒ 不信任任何代理头。
		return convert.ResolveClientIPWithSet(
			remoteAddr(c),
			c.Request.Header.Get("X-Forwarded-For"),
			nil,
		)
	}
	if wantXRI && r.trustXRI {
		return convert.ResolveClientIPWithSetAndXRI(
			remoteAddr(c),
			c.Request.Header.Get("X-Forwarded-For"),
			c.Request.Header.Get("X-Real-IP"),
			r.set,
		)
	}
	return convert.ResolveClientIPWithSet(
		remoteAddr(c),
		c.Request.Header.Get("X-Forwarded-For"),
		r.set,
	)
}

// ===== 清单缓存：身份快路径（#237） + 内容指纹（#174） =====

// proxySetEntry 是一份清单的解析结果：set 与 err 恰有一个非零。
// 错误同样缓存——同一份非法清单的行为在多次调用间保持一致（恒 fail-closed
// + 只告警一次，#224），且不重复解析。raw 是清单的深拷贝，用于无分配的缓存
// 命中判定（maphash 非密码学散列，桶内必须逐条精确比对才能复用）。
type proxySetEntry struct {
	raw []string
	set *convert.TrustedProxySet
	err error
	// warnOnce 保证同一份坏清单只告警一次（#224）。
	warnOnce sync.Once
}

// resolve 返回条目的预解析集合：非法清单 fail-closed——返回 nil（等价
// 「不信任任何代理头」，采信判定回退对端地址），同一份坏清单只告警一次。
// 此前 panic（#224）：panic 落在请求热路径，Recovery 缺失时冲垮连接、存在时
// 每请求 500 + panic 栈噪声；构造期要 error 的出口是 [New] / [NewWithXRI]。
func (e *proxySetEntry) resolve() *convert.TrustedProxySet {
	if e.err != nil {
		e.warnOnce.Do(func() { warnBadProxyList(e.err) })
		return nil
	}
	return e.set
}

// warnBadProxyList 是坏清单的告警出口（#224）。抽成变量便于单测断言（生产
// 指向 logx；告警次数由 proxySetEntry.warnOnce 控制为每份坏清单一次）。
var warnBadProxyList = func(err error) {
	clientipLog.Error("clientip: trusted proxy list contains invalid entry - failing closed (proxy headers not trusted, peer address returned); use clientip.New/NewWithXRI to get the error at startup",
		logx.ErrKey, err)
}

// proxySetBucket 是同一哈希桶内的条目链（哈希碰撞时 >1 条；常规部署恒 1 条）。
// 命中判定走读锁（并发共享），插入走写锁。
type proxySetBucket struct {
	mu      sync.RWMutex
	entries []*proxySetEntry
}

// proxySetBuckets 按清单内容哈希缓存解析结果（身份快路径未命中时的兜底，
// #237）。
//
// 清单是启动期配置、几乎不变 ⇒ 不同清单条数极少；配置变更 = 内容变化 = 新
// 哈希/新条目，天然可见。总量有上限（proxySetCacheLimit）：达到上限后新清单
// 退化为逐请求解析（正确性不变，只丢缓存收益），防御「把请求期输入当清单传入」
// 的病态用法打爆内存。
var proxySetBuckets sync.Map // uint64 → *proxySetBucket

// proxySetCacheSize 是内容缓存当前条目数（只增不减；条目极小，进程生命周期
// 内不需要淘汰）。
var proxySetCacheSize atomic.Int64

// proxySetByIdentity 是身份快路径缓存：切片身份（底层数组首元素指针 + 长度）
// → 条目。同一底层数组的同长度切片内容必然一致（前提：传入后不原地改写
// 条目，见 [trustedProxySetFor]）——命中判定 O(1)，兑现包文档「缓存命中后
// 只剩网段包含判定」的承诺（#237：此前每请求还要 O(n) 算指纹 + 深比较，
// 1000 条清单比预解析形态慢 2.5 倍）。
var proxySetByIdentity sync.Map // proxySetIdentityKey → *proxySetEntry

// proxySetIdentitySize 是身份缓存当前条目数（上限与内容缓存同款：防「每请求
// 造新切片」的病态用法撑爆身份缓存）。
var proxySetIdentitySize atomic.Int64

// proxySetCacheLimit 是单类缓存条目数上限（1024 份不同清单对「启动期配置」
// 形态已极端宽裕）；proxyBucketLimit 是单桶条目上限（压住碰撞链的扫描成本）。
const (
	proxySetCacheLimit = 1024
	proxyBucketLimit   = 8
)

// proxySetIdentityKey 是清单切片的身份键。len==0 无键（空清单恒 fail-closed，
// 无需缓存）。
type proxySetIdentityKey struct {
	arr *string
	n   int
}

func identityKeyOf(proxies []string) (proxySetIdentityKey, bool) {
	if len(proxies) == 0 {
		return proxySetIdentityKey{}, false
	}
	return proxySetIdentityKey{arr: &proxies[0], n: len(proxies)}, true
}

// trustedProxySetFor 返回清单对应的预解析集合。
//
// 两级缓存（#237）：
//   - 身份快路径（O(1)）：同一底层数组 + 长度的重复调用（启动期配置清单的
//     标准形态——清单被包级变量/配置结构体持有后逐请求传入）直接命中，请求期
//     零哈希、零深比较。前提：**清单传入后不得原地改写条目**——按切片身份
//     缓存，改写会拿到旧集合；变更清单应构造新切片（新身份，与「配置变更 =
//     新清单」的常规形态一致）。
//   - 内容指纹路径（O(n)，兜底）：身份未命中（调用方每请求构造新切片）按
//     内容指纹缓存，命中免逐条 ParseCIDR（#174 形态），命中后仍需算指纹。
//
// 清单含非法条目时 fail-closed（返回 nil，不信任任何代理头）+ 每份坏清单
// 只告警一次（#224），不 panic；启动期要 error 用 [New] / [NewWithXRI]。
func trustedProxySetFor(trustedProxies []string) *convert.TrustedProxySet {
	if len(trustedProxies) == 0 {
		return nil // 空/nil 清单恒 fail-closed，无需缓存
	}
	if k, ok := identityKeyOf(trustedProxies); ok {
		if v, hit := proxySetByIdentity.Load(k); hit {
			return v.(*proxySetEntry).resolve()
		}
	}
	entry := resolveProxySet(trustedProxies)
	// 身份缓存有界：达到上限不再存（病态「每请求新切片」用法退化为内容
	// 指纹路径，正确性不变）。
	if k, ok := identityKeyOf(trustedProxies); ok && proxySetIdentitySize.Load() < proxySetCacheLimit {
		if _, loaded := proxySetByIdentity.LoadOrStore(k, entry); !loaded {
			proxySetIdentitySize.Add(1)
		}
	}
	return entry.resolve()
}

// resolveProxySet 走内容指纹缓存解析清单：命中直接复用条目（零分配）；
// 未命中严格解析一次并缓存（错误也缓存）。缓存达到上限后退化为逐请求解析
// （正确性不变，只丢缓存收益）。
func resolveProxySet(trustedProxies []string) *proxySetEntry {
	h := proxyListHash(trustedProxies)
	if b, ok := proxySetBuckets.Load(h); ok {
		if e := lookupProxySetEntry(b.(*proxySetBucket), trustedProxies); e != nil {
			return e
		}
	}
	bkt := getOrCreateProxySetBucket(h)
	bkt.mu.Lock()
	// 双重检查：并发未命中时只插入一份条目。
	for _, e := range bkt.entries {
		if equalStrings(e.raw, trustedProxies) {
			bkt.mu.Unlock()
			return e
		}
	}
	// 条目数超限（全局或单桶）：不再缓存，逐请求解析（正确性不变）。
	if proxySetCacheSize.Load() >= proxySetCacheLimit || len(bkt.entries) >= proxyBucketLimit {
		bkt.mu.Unlock()
		return newProxySetEntry(trustedProxies)
	}
	e := newProxySetEntry(trustedProxies)
	bkt.entries = append(bkt.entries, e)
	proxySetCacheSize.Add(1)
	bkt.mu.Unlock()
	return e
}

// newProxySetEntry 严格解析清单为条目（set 与 err 恰有一个非零）。
func newProxySetEntry(trustedProxies []string) *proxySetEntry {
	set, err := convert.NewTrustedProxySet(trustedProxies)
	return &proxySetEntry{raw: cloneStrings(trustedProxies), set: set, err: err}
}

// lookupProxySetEntry 在桶内找清单完全一致的条目；未找到返回 nil。
func lookupProxySetEntry(bkt *proxySetBucket, trustedProxies []string) *proxySetEntry {
	bkt.mu.RLock()
	defer bkt.mu.RUnlock()
	for _, e := range bkt.entries {
		if equalStrings(e.raw, trustedProxies) {
			return e
		}
	}
	return nil
}

// getOrCreateProxySetBucket 返回哈希对应的桶（不存在则创建空桶）。
func getOrCreateProxySetBucket(h uint64) *proxySetBucket {
	if b, ok := proxySetBuckets.Load(h); ok {
		return b.(*proxySetBucket)
	}
	bkt := &proxySetBucket{}
	actual, _ := proxySetBuckets.LoadOrStore(h, bkt)
	return actual.(*proxySetBucket)
}

// proxyListHash 计算清单内容的散列（maphash，进程内种子）：长度前缀消除
// 条目边界歧义。仅作分桶用——正确性由桶内 deep-equal 保证，散列无需抗碰撞。
func proxyListHash(proxies []string) uint64 {
	var (
		h   maphash.Hash
		buf [8]byte
	)
	h.SetSeed(proxyListHashSeed)
	for _, p := range proxies {
		binary.LittleEndian.PutUint64(buf[:], uint64(len(p)))
		_, _ = h.Write(buf[:])
		_, _ = h.WriteString(p)
	}
	return h.Sum64()
}

var proxyListHashSeed = maphash.MakeSeed()

// equalStrings 深比较两个字符串切片（缓存命中判定，零分配）。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// cloneStrings 深拷贝字符串切片（缓存键持有自己的清单副本，调用方后续
// 改写切片内容不得影响缓存判定）。
func cloneStrings(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// remoteAddr 取请求的对端地址（"ip:port" 形态）。
// Hertz 的 RequestContext.RemoteAddr 保证非 nil（无连接时给零值地址）。
func remoteAddr(c *app.RequestContext) string {
	return c.RemoteAddr().String()
}
