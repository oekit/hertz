package govhertz

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/yi-nology/ekit-core/governance"
)

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
	for i := 0; i < 5; i++ {
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

	// 策略更新:rate=1 burst=1 → 立即按新策略(第一发 200,第二发 429)
	cfg2, _ := governance.Parse([]byte(`{"limiter":{"POST:/order":{"rate":1,"burst":1}}}`))
	store.Update(cfg2)
	time.Sleep(20 * time.Millisecond) // 确保至少 1 token 补充
	if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 200 {
		t.Errorf("after policy update first = %d, want 200", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "POST", "/order", nil); w.Code != 429 {
		t.Errorf("after policy update second = %d, want 429 (burst=1)", w.Code)
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
		for i := 0; i < 50; i++ {
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
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				ut.PerformRequest(h.Engine, "POST", "/race", nil)
			}
		}()
	}
	wg.Wait()
	<-done
}

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
	for i := 0; i < 6; i++ {
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
