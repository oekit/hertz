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

func (b *bucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	burst := b.policy.Burst
	if burst <= 0 {
		burst = b.policy.Rate
	}
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
		v, _ := buckets.LoadOrStore(key, &bucket{gen: gen, policy: pol, tokens: float64(pol.Rate), last: now})
		b := v.(*bucket)
		applySnapshot(b, pol, gen)
		if !b.allow(now) {
			c.AbortWithStatus(http.StatusTooManyRequests)
			return
		}
		c.Next(ctx)
	}
}

// NewTimeoutMiddleware 返回动态超时中间件:按 "path" → "" 从 Store 取
// 超时(毫秒),给下游 handler 的 ctx 注入 deadline(ctx 感知的依赖会
// 提前中止)。到期由下游 ctx 传播体现,本中间件不主动掐断响应。
//
// 路径取**路由模板**(见 [routePath])。
func NewTimeoutMiddleware(store *governance.Store) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if ms, ok := store.Load().LookupTimeout(routePath(c), ""); ok && ms > 0 {
			tctx, cancel := context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
			defer cancel()
			ctx = tctx
		}
		c.Next(ctx)
	}
}
