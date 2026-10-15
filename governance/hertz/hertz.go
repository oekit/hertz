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

// bucket 是极简令牌桶(policy 变化时整体重建,不做原地扩缩)。
type bucket struct {
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

// NewLimiterMiddleware 返回动态限流中间件:按 "METHOD:path" → "path" → ""
// 从 Store 取 LimiterPolicy,超限返 429。策略变化自动重建桶。
//
// 桶按**命中的配置键**分片(而非原始请求路径):精确策略 "POST:/order" 仍按
// path 独立计数;前缀/全局策略下同组 path 共享一个桶(高基数 path 不会撑爆
// 桶表,且聚合容量与配置语义一致)。
func NewLimiterMiddleware(store *governance.Store) app.HandlerFunc {
	var buckets sync.Map // 命中的配置键 → *bucket
	return func(ctx context.Context, c *app.RequestContext) {
		pol, key, ok := store.Load().LookupLimiterKey(string(c.Method()), string(c.Path()))
		if !ok {
			c.Next(ctx)
			return
		}
		now := time.Now()
		v, _ := buckets.LoadOrStore(key, &bucket{policy: pol, tokens: float64(pol.Rate), last: now})
		b := v.(*bucket)
		// policy 比较与替换必须整体持锁:allow 在锁内读 policy,锁外读构成 data race
		b.mu.Lock()
		if b.policy != pol {
			b.policy = pol
			b.tokens = float64(pol.Rate)
		}
		b.mu.Unlock()
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
func NewTimeoutMiddleware(store *governance.Store) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if ms, ok := store.Load().LookupTimeout(string(c.Path()), ""); ok && ms > 0 {
			tctx, cancel := context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
			defer cancel()
			ctx = tctx
		}
		c.Next(ctx)
	}
}
