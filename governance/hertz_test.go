package govhertz

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/oekit/concurrency/async"
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

// ==================== 超时键形态（#233） ====================

// TestTimeoutMiddleware_MethodPathKey 回归 #233：按共享治理模板文档写的
// "METHOD:path" 精确键必须在 hertz 侧生效。此前复用 kitex 口径的
// LookupTimeout(path, "")——第一档查的是 path+":"，METHOD:path 键静默失效
// （无 deadline，超时保护整体不生效且无任何日志）。
func TestTimeoutMiddleware_MethodPathKey(t *testing.T) {
	store := governance.NewStore(nil)
	cfg, _ := governance.Parse([]byte(`{"timeout_ms":{"GET:/slow":50}}`))
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
	h.POST("/slow", func(ctx context.Context, c *app.RequestContext) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("POST must not hit the GET:/slow timeout key")
		}
		c.Next(ctx)
	})

	ut.PerformRequest(h.Engine, "GET", "/slow", nil)
	if !gotDeadline {
		t.Error(`timeout_ms["GET:/slow"] must produce a deadline for GET /slow`)
	}
	ut.PerformRequest(h.Engine, "POST", "/slow", nil)
}

// TestTimeoutMiddleware_PathSegmentPrefixNoDotCrossHit 回归 #233 复现 B：
// 前缀档必须按「/」段边界匹配，不得沿用 service 名的「.」段口径——只配
// "/api" 不得命中 /api.v1/users（命中范围超出配置意图）。
func TestTimeoutMiddleware_PathSegmentPrefixNoDotCrossHit(t *testing.T) {
	store := governance.NewStore(nil)
	cfg, _ := governance.Parse([]byte(`{"timeout_ms":{"/api":50}}`))
	store.Update(cfg)

	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewTimeoutMiddleware(store))
	h.GET("/api.v1/users", func(ctx context.Context, c *app.RequestContext) {
		if _, ok := ctx.Deadline(); ok {
			t.Error(`"/api" must not match /api.v1/users ("."-segment prefix tier is service-name semantics, not path semantics)`)
		}
		c.Next(ctx)
	})
	ut.PerformRequest(h.Engine, "GET", "/api.v1/users", nil)
}

// TestTimeoutMiddleware_PathPrefixTier 钉住前缀档的正确语义：method 限定键
// 与裸 path 键都按「/」段边界命中路由模板（/order/:id → 前缀 /order）。
func TestTimeoutMiddleware_PathPrefixTier(t *testing.T) {
	store := governance.NewStore(nil)
	cfg, _ := governance.Parse([]byte(`{"timeout_ms":{"GET:/order":50}}`))
	store.Update(cfg)

	var gotDeadline bool
	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewTimeoutMiddleware(store))
	h.GET("/order/:id", func(ctx context.Context, c *app.RequestContext) {
		_, gotDeadline = ctx.Deadline()
		c.Next(ctx)
	})
	ut.PerformRequest(h.Engine, "GET", "/order/1", nil)
	if !gotDeadline {
		t.Error(`timeout_ms["GET:/order"] must match /order/:id via "/"-segment prefix tier`)
	}
}

// TestTimeoutMiddleware_GlobalKey 钉住全局 "" 键兜底。
func TestTimeoutMiddleware_GlobalKey(t *testing.T) {
	store := governance.NewStore(nil)
	cfg, _ := governance.Parse([]byte(`{"timeout_ms":{"":50}}`))
	store.Update(cfg)

	var gotDeadline bool
	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewTimeoutMiddleware(store))
	h.GET("/anything", func(ctx context.Context, c *app.RequestContext) {
		_, gotDeadline = ctx.Deadline()
		c.Next(ctx)
	})
	ut.PerformRequest(h.Engine, "GET", "/anything", nil)
	if !gotDeadline {
		t.Error(`timeout_ms[""] (global default) must produce a deadline`)
	}
}

// TestTimeoutMiddleware_AsyncHandlerDeadline 回归 #235：异步 handler 把工作
// 交给后台 goroutine 后立即返回，ctx 必须保持存活到 deadline 到期才取消——
// 此前 defer cancel() 挂在中间件返回上，后台 goroutine 的超时值实际为 0ms。
func TestTimeoutMiddleware_AsyncHandlerDeadline(t *testing.T) {
	store := governance.NewStore(nil)
	const timeoutMs = 120
	cfg, _ := governance.Parse([]byte(`{"timeout_ms":{"/async":` + itoa(timeoutMs) + `}}`))
	store.Update(cfg)

	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewTimeoutMiddleware(store))

	cancelled := make(chan time.Duration, 1)
	h.GET("/async", func(ctx context.Context, c *app.RequestContext) {
		start := time.Now()
		async.GoSafe(func() {
			<-ctx.Done()
			cancelled <- time.Since(start)
		})
		// 立即返回：响应照常写出，后台 goroutine 继续持有 ctx（SSE/异步
		// 推送的常见写法）
	})
	go func() { _ = ut.PerformRequest(h.Engine, "GET", "/async", nil) }()

	select {
	case elapsed := <-cancelled:
		// 下界：不得在 deadline 一半之前取消（旧缺陷=0ms）；上界：3 倍超时
		// 之外说明定时器没生效。窗口余量防 CI 慢调度。
		if elapsed < 60*time.Millisecond {
			t.Errorf("ctx cancelled after %v, want >=60ms (0ms means middleware-return cancel, the #235 bug)", elapsed)
		}
		if elapsed > 3*timeoutMs*time.Millisecond {
			t.Errorf("ctx cancelled after %v, want ~%dms (deadline timer must still fire)", elapsed, timeoutMs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("async goroutine never observed ctx cancellation within 2s")
	}
}

// TestTimeoutMiddleware_NoConfigNoDeadline 钉住：无配置（含 timeout_ms 键
// 不存在）时不注入 deadline，行为与既往一致。
func TestTimeoutMiddleware_NoConfigNoDeadline(t *testing.T) {
	store := governance.NewStore(nil)

	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewTimeoutMiddleware(store))
	h.GET("/open", func(ctx context.Context, c *app.RequestContext) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("empty store should not inject a deadline")
		}
		c.Next(ctx)
	})
	ut.PerformRequest(h.Engine, "GET", "/open", nil)
}

// TestLookupTimeoutKey_Matrix 表驱动钉住 lookupTimeoutKey 的档序：
// "METHOD:path" → "path" → 「/」段前缀（每档先 method 限定再裸 path）→ ""。
func TestLookupTimeoutKey_Matrix(t *testing.T) {
	tests := []struct {
		name   string
		cfg    map[string]int
		method string
		path   string
		want   int
		wantOK bool
	}{
		{
			name:   "exact METHOD:path wins",
			cfg:    map[string]int{"GET:/slow": 10, "/slow": 20, "": 30},
			method: "GET", path: "/slow",
			want: 10, wantOK: true,
		},
		{
			name:   "bare path tier",
			cfg:    map[string]int{"/slow": 20, "": 30},
			method: "POST", path: "/slow",
			want: 20, wantOK: true,
		},
		{
			name:   "method-qualified prefix tier beats bare prefix",
			cfg:    map[string]int{"GET:/order": 10, "/order": 20},
			method: "GET", path: "/order/123",
			want: 10, wantOK: true,
		},
		{
			name:   "bare prefix tier",
			cfg:    map[string]int{"/order": 20},
			method: "POST", path: "/order/123",
			want: 20, wantOK: true,
		},
		{
			name:   "segment boundary: /orders is not a prefix of /order",
			cfg:    map[string]int{"/order": 20},
			method: "GET", path: "/orders",
			want: 0, wantOK: false,
		},
		{
			name:   "no dot-segment cross hit",
			cfg:    map[string]int{"/api": 20},
			method: "GET", path: "/api.v1/users",
			want: 0, wantOK: false,
		},
		{
			name:   "global fallback",
			cfg:    map[string]int{"": 30},
			method: "GET", path: "/whatever",
			want: 30, wantOK: true,
		},
		{
			name:   "nil map",
			cfg:    nil,
			method: "GET", path: "/slow",
			want: 0, wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lookupTimeoutKey(tt.cfg, tt.method, tt.path)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("lookupTimeoutKey(%v, %q, %q) = (%d, %v), want (%d, %v)",
					tt.cfg, tt.method, tt.path, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// ==================== 桶初始存量（#234） ====================

// TestNewBucket_InitialTokensMatrix 回归 #234：新桶初始令牌数 = 桶容量
// （Burst，0 时取 Rate），与 allow 的容量钳制同一口径。
func TestNewBucket_InitialTokensMatrix(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		pol        governance.LimiterPolicy
		wantTokens float64
	}{
		{name: "burst is capacity", pol: governance.LimiterPolicy{Rate: 1, Burst: 100}, wantTokens: 100},
		{name: "zero burst falls back to rate", pol: governance.LimiterPolicy{Rate: 5, Burst: 0}, wantTokens: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newBucket(1, tt.pol, now)
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.tokens != tt.wantTokens {
				t.Errorf("initial tokens = %v, want %v (capacity, not rate)", b.tokens, tt.wantTokens)
			}
		})
	}
}

// TestLimiterMiddleware_BurstInitialTokens 回归 #234（行为级）：rate=1
// burst=50 的新桶首个突发必须放行 50 个——此前初始存量=Rate，只放行 1 个，
// 其余 429（限流比配置严格一个数量级）。
func TestLimiterMiddleware_BurstInitialTokens(t *testing.T) {
	store := governance.NewStore(nil)
	cfg, _ := governance.Parse([]byte(`{"limiter":{"GET:/burst":{"rate":1,"burst":50}}}`))
	store.Update(cfg)

	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	h.Use(NewLimiterMiddleware(store))
	h.GET("/burst", func(_ context.Context, c *app.RequestContext) {
		c.String(200, "ok")
	})

	allowed := 0
	for range 60 {
		if ut.PerformRequest(h.Engine, "GET", "/burst", nil).Code == 200 {
			allowed++
		}
	}
	// 余量 1：补偿整轮循环里 rate=1/s 的自然回填（循环耗时 >1s 时 +1）
	if allowed < 50 || allowed > 51 {
		t.Errorf("allowed = %d, want 50 (burst capacity must be the initial tokens)", allowed)
	}
}

// itoa 是测试内的小工具：timeout_ms 数值拼进配置 JSON 用。
func itoa(n int) string {
	return strconv.Itoa(n)
}
