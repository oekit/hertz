package govhertz

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/oekit/core/governance"
)

// TestLimiterMiddleware_RouteTemplatePolicy 钉住：path 级策略按**路由模板**
// 匹配——参数化路由（/order/:id）此前用原始路径（/order/123）查策略，
// 永远命中不了 path 级策略（只落到全局键）⇒ 治理策略对参数化路由形同虚设。
func TestLimiterMiddleware_RouteTemplatePolicy(t *testing.T) {
	store := governance.NewStore(nil)
	cfg, _ := governance.Parse([]byte(`{"limiter":{"GET:/order/:id":{"rate":1,"burst":1}}}`))
	store.Update(cfg)

	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewLimiterMiddleware(store))
	h.GET("/order/:id", func(_ context.Context, c *app.RequestContext) {
		c.String(200, "ok")
	})

	if w := ut.PerformRequest(h.Engine, "GET", "/order/1", nil); w.Code != 200 {
		t.Fatalf("first request = %d, want 200", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/order/2", nil); w.Code != 429 {
		t.Errorf("second request = %d, want 429 (route-template policy must apply)", w.Code)
	}
}

// TestTimeoutMiddleware_RouteTemplatePolicy 同上：超时策略按路由模板匹配。
func TestTimeoutMiddleware_RouteTemplatePolicy(t *testing.T) {
	store := governance.NewStore(nil)
	cfg, _ := governance.Parse([]byte(`{"timeout_ms":{"/slow/:id":50}}`))
	store.Update(cfg)

	var gotDeadline bool
	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewTimeoutMiddleware(store))
	h.GET("/slow/:id", func(ctx context.Context, c *app.RequestContext) {
		_, gotDeadline = ctx.Deadline()
		c.Next(ctx)
	})
	ut.PerformRequest(h.Engine, "GET", "/slow/1", nil)

	if !gotDeadline {
		t.Error("timeout policy must match the route template, not the raw path")
	}
}

// TestRoutePath_FallsBackToRawPath 未匹配到路由时（无路由模板）回退原始路径，
// 保持既有行为。
func TestRoutePath_FallsBackToRawPath(t *testing.T) {
	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.GET("/order/:id", func(_ context.Context, c *app.RequestContext) {
		c.String(200, "ok")
	})
	w := ut.PerformRequest(h.Engine, "GET", "/order/42", nil)
	if w.Code != 200 {
		t.Fatalf("request = %d, want 200", w.Code)
	}
}

// TestLimiterMiddleware_DynamicPolicy 行为级:无配置放行 → 配置限流后超限 429
// → 策略更新后放行窗口变化(动态收敛的核心价值)。
func TestLimiterMiddleware_DynamicPolicy(t *testing.T) {
	store := governance.NewStore(nil)
	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewLimiterMiddleware(store))
	h.POST("/order", func(_ context.Context, c *app.RequestContext) {
		c.String(200, "ok")
	})

	// 无配置:全放行
	for i := range 5 {
		if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 200 {
			t.Fatalf("no-config request %d: got %d, want 200", i, w.Code)
		}
	}

	// 配置限流:rate=2 burst=2 → 前两个 200,之后 429
	cfg, _ := governance.Parse([]byte(`{"limiter":{"POST:/order":{"rate":2,"burst":2}}}`))
	store.Update(cfg)
	codes := make([]int, 4)
	for i := range codes {
		codes[i] = ut.PerformRequest(h.Engine, "POST", "/order", nil).Code
	}
	if codes[0] != 200 || codes[1] != 200 {
		t.Errorf("first two = %v, want 200,200", codes[:2])
	}
	if codes[2] != 429 || codes[3] != 429 {
		t.Errorf("over-rate = %v, want 429,429", codes[2:])
	}

	// 策略更新(#169):只换 policy 参数,桶态保留——此刻桶已被打空(tokens≈0),
	// 切换瞬间不得凭空补满放行(旧实现把 tokens 重置为新 Rate,等价突发放行);
	// 等新速率自然补满(r=10/s,睡 150ms ≈ 1.5 token,burst=1 钳到 1)才放行
	cfg2, _ := governance.Parse([]byte(`{"limiter":{"POST:/order":{"rate":10,"burst":1}}}`))
	store.Update(cfg2)
	if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 429 {
		t.Errorf("right after policy switch = %d, want 429 (bucket state must be preserved, not reset)", w.Code)
	}
	time.Sleep(150 * time.Millisecond) // 按 rate=10 自然补满
	if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 200 {
		t.Errorf("after natural refill = %d, want 200 (new policy must take effect)", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 429 {
		t.Errorf("after refill second = %d, want 429 (burst=1)", w.Code)
	}
}

// TestTimeoutMiddleware_InjectsDeadline 行为级:配置超时后,下游 handler 的
// ctx 携带 deadline(ctx 感知依赖据此提前中止)。
func TestTimeoutMiddleware_InjectsDeadline(t *testing.T) {
	store := governance.NewStore(nil)
	cfg, _ := governance.Parse([]byte(`{"timeout_ms":{"/slow":50}}`))
	store.Update(cfg)

	var gotDeadline bool
	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewTimeoutMiddleware(store))
	h.GET("/slow", func(ctx context.Context, c *app.RequestContext) {
		_, gotDeadline = ctx.Deadline()
		c.Next(ctx)
	})
	ut.PerformRequest(h.Engine, "GET", "/slow", nil)
	if !gotDeadline {
		t.Error("downstream ctx should carry deadline from timeout_ms config")
	}
	// 未配置路径不注入
	h.GET("/open", func(ctx context.Context, c *app.RequestContext) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("unconfigured path should have no deadline")
		}
		c.Next(ctx)
	})
	ut.PerformRequest(h.Engine, "GET", "/open", nil)
}

// TestLimiterMiddleware_ConcurrentUpdateRace 回归：policy 锁外读、锁内写构成
// data race（流量与 Nacos 配置推送并发时）。钉住：并发 Update + 并发请求下
// -race 干净。
// TestLimiterMiddleware_GlobalPolicySharedBucket 钉住：前缀/全局策略下按
// **命中的配置键**分桶——不同 path 共享同一桶，桶表条目数受配置约束，
// 高基数 path 不会造成无界增长。
func TestLimiterMiddleware_GlobalPolicySharedBucket(t *testing.T) {
	store := governance.NewStore(nil)
	cfg, _ := governance.Parse([]byte(`{"limiter":{"":{"rate":2,"burst":2}}}`))
	store.Update(cfg)

	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewLimiterMiddleware(store))
	h.POST("/:p", func(_ context.Context, c *app.RequestContext) {
		c.String(200, "ok")
	})

	// 两个不同 path 交替请求：共享 rate=2 burst=2 的全局桶，合计只放行 2 个
	allowed := 0
	for i := range 6 {
		path := "/a"
		if i%2 == 1 {
			path = "/b"
		}
		if w := ut.PerformRequest(h.Engine, "POST", path, nil); w.Code == 200 {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("global policy should share one bucket across paths, allowed=%d, want 2", allowed)
	}
}

func TestLimiterMiddleware_ConcurrentUpdateRace(t *testing.T) {
	store := governance.NewStore(nil)
	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewLimiterMiddleware(store))
	h.POST("/race", func(_ context.Context, c *app.RequestContext) {
		c.String(200, "ok")
	})

	done := make(chan struct{})
	// 并发配置推送：交替更新策略
	go func() {
		defer close(done)
		for range 50 {
			cfg, err := governance.Parse([]byte(`{"limiter":{"POST:/race":{"rate":100,"burst":100}}}`))
			if err != nil {
				t.Error(err)
				return
			}
			store.Update(cfg)
		}
	}()
	// 并发流量
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				ut.PerformRequest(h.Engine, "POST", "/race", nil)
			}
		})
	}
	wg.Wait()
	<-done
}

// ==================== 快照代际合并（#169） ====================

// TestApplySnapshot_GenerationMatrix 表驱动钉住快照→桶的合并规则：
// 只有**新于**桶代际的快照才允许改写策略（单向推进），且改写只换 policy
// 参数、tokens/last 保留。旧实现按策略值比较改写：热更窗口内拿到旧快照的
// 迟到请求把已收敛的桶整桶回退 + tokens 清零重置，flap 窗口内反复发生
// 等价一次突发放行。
func TestApplySnapshot_GenerationMatrix(t *testing.T) {
	p1 := governance.LimiterPolicy{Rate: 1, Burst: 1}
	p2 := governance.LimiterPolicy{Rate: 5, Burst: 5}

	tests := []struct {
		name      string
		bucketGen int64
		bucketPol governance.LimiterPolicy
		snapGen   int64
		snapPol   governance.LimiterPolicy
		wantPol   governance.LimiterPolicy
		wantGen   int64
	}{
		{
			name:      "newer snapshot adopts policy",
			bucketGen: 1, bucketPol: p1,
			snapGen: 2, snapPol: p2,
			wantPol: p2, wantGen: 2,
		},
		{
			// #169 回归：热更（P1→P2）后拿到 P1 快照的迟到请求不得把桶回退
			name:      "stale snapshot cannot roll back policy",
			bucketGen: 2, bucketPol: p2,
			snapGen: 1, snapPol: p1,
			wantPol: p2, wantGen: 2,
		},
		{
			// 同代际重放（快照与桶同代）无事可做
			name:      "equal generation is a no-op",
			bucketGen: 2, bucketPol: p2,
			snapGen: 2, snapPol: p2,
			wantPol: p2, wantGen: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &bucket{gen: tt.bucketGen, policy: tt.bucketPol, tokens: 0.4, last: time.Now()}
			applySnapshot(b, tt.snapPol, tt.snapGen)

			b.mu.Lock()
			gotPol, gotGen, gotTokens := b.policy, b.gen, b.tokens
			b.mu.Unlock()

			if gotPol != tt.wantPol {
				t.Errorf("policy = %+v, want %+v", gotPol, tt.wantPol)
			}
			if gotGen != tt.wantGen {
				t.Errorf("gen = %d, want %d", gotGen, tt.wantGen)
			}
			// 桶态保留是本次修复的核心口径：清零重置等价突发放行
			if gotTokens != 0.4 {
				t.Errorf("tokens = %v, want 0.4 (bucket state must survive policy swap)", gotTokens)
			}
		})
	}
}

// TestApplySnapshot_NewerSnapshotKeepsTokens 策略改写只换参数：桶内已积累
// 的状态（含已消耗后的余量）不因配置切换被凭空补满。
func TestApplySnapshot_NewerSnapshotKeepsTokens(t *testing.T) {
	pOld := governance.LimiterPolicy{Rate: 100, Burst: 100}
	pNew := governance.LimiterPolicy{Rate: 1, Burst: 1}
	b := &bucket{gen: 1, policy: pOld, tokens: 37.5, last: time.Now()}

	applySnapshot(b, pNew, 2)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens != 37.5 {
		t.Errorf("tokens = %v, want 37.5 (switching to a smaller policy must not refill the bucket)", b.tokens)
	}
	if b.policy != pNew {
		t.Errorf("policy = %+v, want %+v", b.policy, pNew)
	}
}

// TestLimiterMiddleware_PolicySwitchNoFreeRefill 回归 #169（行为级）：热更
// 切换瞬间桶态保留——已被打空的桶不会因换策略被重置补满（旧实现 tokens
// 重置为新 Rate，配置 flap 窗口内反复清零等价突发放行）。
func TestLimiterMiddleware_PolicySwitchNoFreeRefill(t *testing.T) {
	store := governance.NewStore(nil)
	cfg1, _ := governance.Parse([]byte(`{"limiter":{"POST:/order":{"rate":1,"burst":1}}}`))
	store.Update(cfg1)

	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewLimiterMiddleware(store))
	h.POST("/order", func(_ context.Context, c *app.RequestContext) {
		c.String(200, "ok")
	})

	// gen1: 打空桶（满桶放行一发，随后 429）
	if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 200 {
		t.Fatalf("first request = %d, want 200", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 429 {
		t.Fatalf("second request = %d, want 429 (bucket drained)", w.Code)
	}

	// gen2: 调大策略（rate=10 burst=1000）——切换瞬间不得凭空放行：
	// tokens≈0 保留，新速率需 ~100ms 才补满 1 个
	cfg2, _ := governance.Parse([]byte(`{"limiter":{"POST:/order":{"rate":10,"burst":1000}}}`))
	store.Update(cfg2)
	if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 429 {
		t.Errorf("right after policy switch = %d, want 429 (no free refill on policy change)", w.Code)
	}
	time.Sleep(150 * time.Millisecond) // rate=10/s → ≥1 token
	if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 200 {
		t.Errorf("after natural refill = %d, want 200 (new policy governs refill)", w.Code)
	}
}
