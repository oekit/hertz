// Package govhertz 把 governance.Store 消费成 hertz server 治理中间件
// (audit 5.2 阶段 1:hertz 侧治理)。kitex 侧继续用既有 NewGovernanceConfig,
// 阶段 2 迁移到同一份模板。
//
// 用法(bootstrap.Run 内):
//
//	store := governance.NewStore(nil)
//	// Nacos 监听回调里:cfg, _ := governance.Parse(data); store.Update(cfg)
//	h.Use(govhertz.NewLimiterMiddleware(store))  // 超限 429
//	h.Use(govhertz.NewTimeoutMiddleware(store))  // 注入 per-request deadline
package govhertz

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/oekit/core/governance"
)

// routePath 返回用于治理策略匹配的路径:优先取**路由模板**
// (如 "/order/:id"),未匹配到路由时回退原始请求路径。
//
// 此前用原始路径匹配:带路径参数的路由("/order/123")永远命中不了
// path 级策略(策略键是模板形态),只落到全局键 ⇒ 参数化路由的治理策略
// 形同虚设。静态路由的模板与原始路径一致,行为不变。
func routePath(c *app.RequestContext) string {
	if p := c.FullPath(); p != "" {
		return p
	}
	return string(c.Path())
}

// bucket 是极简令牌桶:策略热更只原地换 policy 参数(以配置代际单向推进,
// 见 applySnapshot),桶态(tokens/last)始终保留——清零重置等价突发放行
// (策略 flap 窗口内桶被反复清零,#169)。tokens 超出新 burst 由 allow 的
// 上限钳制自然收敛,不需要重建。
type bucket struct {
	gen    int64 // 已生效策略的配置代际(store 版本,单调递增)
	policy governance.LimiterPolicy
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// bucketCapacity 返回策略的桶容量（core LimiterPolicy 口径）：Burst，0 时
// 取 Rate。桶的初始令牌数（[newBucket]）与上限钳制（allow）必须同一来源——
// 两套口径会让突发上限实际不生效（#234）。
func bucketCapacity(pol governance.LimiterPolicy) int {
	if pol.Burst > 0 {
		return pol.Burst
	}
	return pol.Rate
}

func (b *bucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	burst := bucketCapacity(b.policy)
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * float64(b.policy.Rate)
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// newBucket 构造新桶：初始令牌数 = 桶容量（#234）。此前写死 pol.Rate——
// 容量口径在 allow 里按 Burst 钳制、存量口径却是 Rate，两套口径并存：
// 运维按「平时 1 QPS、允许 100 突发」配 rate=1 burst=100，实际首个突发只
// 放行 1 个，其余全 429，突发上限在新桶上不生效。初始存量改用与容量同一
// 来源的 [bucketCapacity]（Burst，0 时取 Rate），与令牌桶模型的标准语义
// （新桶满）一致。
func newBucket(gen int64, pol governance.LimiterPolicy, now time.Time) *bucket {
	return &bucket{gen: gen, policy: pol, tokens: float64(bucketCapacity(pol)), last: now}
}

// applySnapshot 把一份配置快照(带其发布代际 gen)合并进桶:只有**新于**
// 桶代际的快照才允许改写策略,且只换 policy 参数、不动 tokens/last。
//
// 为什么按代际单向推进(#169):热更窗口内并发请求各自 LoadVersion,快照
// 新旧不一——旧快照(P1)若按值比较改写桶,会把已收敛到 P2 的桶整桶回退,
// 并把 tokens 清零重置成 P1.Rate;flap 窗口内反复发生等价一次突发放行。
// 改为「代际大者为准」后旧快照按桶内现策略执行;新快照改写时保留桶态,
// 已消耗的配额不因配置切换被凭空补满。
func applySnapshot(b *bucket, pol governance.LimiterPolicy, gen int64) {
	// 代际比较与策略替换必须整体持锁:allow 在锁内读 policy,锁外读写构成 data race
	b.mu.Lock()
	if gen > b.gen {
		b.gen = gen
		b.policy = pol
	}
	b.mu.Unlock()
}

// NewLimiterMiddleware 返回动态限流中间件:按 "METHOD:path" → "path" → ""
// 从 Store 取 LimiterPolicy,超限返 429。策略变化按配置代际单向原地生效
// (旧快照不回退、不清桶态,见 [applySnapshot])。
//
// 路径取**路由模板**(见 [routePath])。
//
// 桶按**命中的配置键**分片(而非原始请求路径):精确策略 "POST:/order" 仍按
// path 独立计数;前缀/全局策略下同组 path 共享一个桶(高基数 path 不会撑爆
// 桶表,且聚合容量与配置语义一致)。
func NewLimiterMiddleware(store *governance.Store) app.HandlerFunc {
	var buckets sync.Map // 命中的配置键 → *bucket
	return func(ctx context.Context, c *app.RequestContext) {
		// LoadVersion 一次拿到一致的(配置, 代际)对:代际要与快照同源,
		// Load()+Version() 两次调用可能拿到「新值配旧代次」的自相矛盾对
		cfg, gen := store.LoadVersion()
		pol, key, ok := cfg.LookupLimiterKey(string(c.Method()), routePath(c))
		if !ok {
			c.Next(ctx)
			return
		}
		now := time.Now()
		// 双查模式（#223）：LoadOrStore 的 newBucket 参数无论命中与否都会求值，
		// 命中路径上每请求白分配一个含锁的 bucket（纯热路径垃圾）。先 Load
		// 命中直接用；miss 分支才构造并 LoadOrStore——并发冷启动下的双插由
		// LoadOrStore 的原子性兜住，输者只是丢弃新桶，分配收敛到冷启动/新键。
		v, ok := buckets.Load(key)
		if !ok {
			v, _ = buckets.LoadOrStore(key, newBucket(gen, pol, now))
		}
		b := v.(*bucket)
		applySnapshot(b, pol, gen)
		if !b.allow(now) {
			c.AbortWithStatus(http.StatusTooManyRequests)
			return
		}
		c.Next(ctx)
	}
}

// NewTimeoutMiddleware 返回动态超时中间件:按 "METHOD:path" → "path" →
// path 前缀最长匹配(按「/」段边界) → "" 从 Store 取超时(毫秒),给下游
// handler 的 ctx 注入 deadline(ctx 感知的依赖会提前中止)。到期由下游 ctx
// 传播体现,本中间件不主动掐断响应。
//
// 键形态与限流侧([NewLimiterMiddleware] → LookupLimiterKey)一致(#233):
// 此前复用面向 kitex 的 LookupTimeout(path, "")——第一档查的是
// path+":"(永远不是 "METHOD:path"),共享模板文档化的 {"GET:/slow":50}
// 在 hertz 侧静默失效;其「.」段前缀档是 service 名语义,套到路径上还会把
// /api 误命中到 /api.v1/users。
//
// 路径取**路由模板**(见 [routePath])。
//
// ⚠️ #235:deadline 到期前**不得**取消 ctx——cancel 不在中间件返回时调用
// (hertz 允许异步 handler 把工作交给后台 goroutine 后立即返回,中间件返回
// 即 defer cancel() 会把超时值变成 0ms)。子 ctx 由 WithTimeout 的内部定时器
// 在 deadline 到点自动取消,cancel 只用于提前释放,而我们恰恰不能提前。代价:
// 定时器存活至 deadline(有界:QPS × 超时值;hertz 的请求 ctx 源自每连接
// ctx、请求完成从不取消,没有可挂靠的「响应写完」钩子,提前释放与异步
// 正确性不可兼得——同步 handler 的提前释放正是本缺陷)。
func NewTimeoutMiddleware(store *governance.Store) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if ms, ok := lookupTimeoutKey(store.Load().TimeoutMs, string(c.Method()), routePath(c)); ok && ms > 0 {
			tctx, cancel := context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
			_ = cancel // #235:刻意不调用,到期由 WithTimeout 内部定时器取消(见 godoc)
			ctx = tctx
		}
		c.Next(ctx)
	}
}

// lookupTimeoutKey 按 hertz 键形态查超时:档序与 core LookupLimiterKey 的
// 路径口径一致(#233)——"METHOD:path" → "path" → path 前缀最长匹配(按
// 「/」段边界,每档先试 method 限定键再试裸 path) → ""。
//
// core 侧暂无 path 口径的 LookupTimeoutKey(issue #233 建议 1),本函数是
// hertz 侧的同构本地实现,core 提供后可整体替换。不能拿 LookupTimeout(path, "")
// 顶替:其第一档查 path+":"(永不命中 "METHOD:path"),第三档按「.」段边界
// 匹配 service 名前缀,对 path 是误命中面。
func lookupTimeoutKey(m map[string]int, method, path string) (int, bool) {
	if len(m) == 0 {
		return 0, false
	}
	if v, ok := m[method+":"+path]; ok {
		return v, true
	}
	if v, ok := m[path]; ok {
		return v, true
	}
	for _, p := range pathSegmentPrefixes(path) {
		if v, ok := m[method+":"+p]; ok {
			return v, true
		}
		if v, ok := m[p]; ok {
			return v, true
		}
	}
	v, ok := m[""]
	return v, ok
}

// pathSegmentPrefixes 返回 path 的全部真段前缀(不含 path 自身与空串),
// 从长到短:/order/123 → ["/order"]。与 core segmentPrefixes(path, "/") 同构:
// 分隔符必须落在段尾(/orders 不为 /order 产生候选),起始 "/" 不算边界
// (i 从 1 起,否则会产出空串)。
func pathSegmentPrefixes(path string) []string {
	var out []string
	for i := 1; i < len(path); i++ {
		if path[i] == '/' {
			out = append(out, path[:i])
		}
	}
	return out
}
