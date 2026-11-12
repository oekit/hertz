package nns

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/oekit/concurrency/async"
	ereg "github.com/oekit/registry"
	registrymocks "github.com/oekit/registry/registrymocks"
)

func TestNewInstanceWatcher_PanicsOnNilDiscoverer(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil discoverer")
		}
	}()
	NewInstanceWatcher(nil)
}

func TestNewInstanceWatcher_Defaults(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}
	h := NewInstanceWatcher(disc)

	assert.DeepEqual(t, 10*time.Second, h.interval)
	assert.DeepEqual(t, 0, len(h.services))
	assert.NotNil(t, h.cache)
}

func TestNewInstanceWatcher_WithOptions(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}
	called := false

	h := NewInstanceWatcher(disc,
		WithWatchInterval(5*time.Second),
		WithWatchServices("svc-a", "svc-b"),
		WithWatchCallback(func(evt InstanceEvent) { called = true }),
	)

	assert.DeepEqual(t, 5*time.Second, h.interval)
	assert.DeepEqual(t, 2, len(h.services))
	assert.DeepEqual(t, 1, len(h.callbacks))
	_ = called
}

// TestWithWatchInterval_NonPositivePanics 回归（#218）：非正间隔必须**构造期**
// panic。此前无校验，0/负值存活到 Start 内 time.NewTicker 才 panic("non-positive
// interval for NewTicker")，而 Start 由业务自行起 goroutine 调用，panic 未包
// recover 即进程退出。fail-fast 口径对齐同文件 NewInstanceWatcher 的
// nil discoverer。
func TestWithWatchInterval_NonPositivePanics(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}
	for name, d := range map[string]time.Duration{"zero": 0, "negative": -time.Second} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("WithWatchInterval(%v) must panic at construction (构造期 fail-fast, #218)", d)
				}
			}()
			NewInstanceWatcher(disc, WithWatchInterval(d))
		})
	}
}

// TestWithWatchInterval_PositiveAccepted 钉住合法间隔不受构造期校验影响。
func TestWithWatchInterval_PositiveAccepted(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}
	h := NewInstanceWatcher(disc, WithWatchInterval(time.Millisecond))
	assert.DeepEqual(t, time.Millisecond, h.interval)
}

func TestInstanceWatcher_GetHealthyInstances_Empty(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}
	h := NewInstanceWatcher(disc)

	instances := h.GetHealthyInstances("nonexistent")
	assert.DeepEqual(t, 0, len(instances))
}

func TestInstanceWatcher_GetHealthyInstances_Cached(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}
	h := NewInstanceWatcher(disc)

	h.mu.Lock()
	h.cache["svc-a"] = []ereg.Instance{
		{IP: "10.0.0.1", Port: 8080},
	}
	h.mu.Unlock()

	instances := h.GetHealthyInstances("svc-a")
	assert.DeepEqual(t, 1, len(instances))
	assert.DeepEqual(t, "10.0.0.1", instances[0].IP)
}

func TestInstanceWatcher_GetHealthyInstances_ReturnsCopy(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}
	h := NewInstanceWatcher(disc)

	h.mu.Lock()
	h.cache["svc-a"] = []ereg.Instance{
		{IP: "10.0.0.1", Port: 8080},
	}
	h.mu.Unlock()

	instances1 := h.GetHealthyInstances("svc-a")
	instances2 := h.GetHealthyInstances("svc-a")

	// Modify first result
	if len(instances1) > 0 {
		instances1[0].IP = "modified"
	}

	// Second result should be unaffected
	assert.DeepEqual(t, "10.0.0.1", instances2[0].IP)
}

func TestInstanceWatcher_StartStop(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: map[string][]ereg.Instance{
		"svc-a": {{IP: "10.0.0.1", Port: 8080}},
	}}
	h := NewInstanceWatcher(disc,
		WithWatchInterval(50*time.Millisecond),
		WithWatchServices("svc-a"),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go h.Start(ctx)

	time.Sleep(100 * time.Millisecond)
	cancel()
	h.Stop()

	instances := h.GetHealthyInstances("svc-a")
	assert.DeepEqual(t, 1, len(instances))
}

func TestInstanceWatcher_CallbackTriggered(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: map[string][]ereg.Instance{
		"svc-a": {{IP: "10.0.0.1", Port: 8080}},
	}}

	var mu sync.Mutex
	var events []InstanceEvent
	h := NewInstanceWatcher(disc,
		WithWatchInterval(50*time.Millisecond),
		WithWatchServices("svc-a"),
		WithWatchCallback(func(evt InstanceEvent) {
			mu.Lock()
			events = append(events, evt)
			mu.Unlock()
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go h.Start(ctx)

	time.Sleep(150 * time.Millisecond)
	cancel()
	h.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Error("expected at least one callback event")
	}
	assert.DeepEqual(t, "svc-a", events[0].Service)
}

func TestInstanceWatcher_StopIdempotent(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}
	h := NewInstanceWatcher(disc)

	h.Stop()
	h.Stop() // Should not panic
}

func TestInstancesChanged_DifferentLength(t *testing.T) {
	a := []ereg.Instance{{IP: "10.0.0.1", Port: 8080}}
	b := []ereg.Instance{{IP: "10.0.0.1", Port: 8080}, {IP: "10.0.0.2", Port: 8080}}
	if !instancesChanged(a, b) {
		t.Error("expected true for different length")
	}
}

func TestInstancesChanged_DifferentIP(t *testing.T) {
	a := []ereg.Instance{{IP: "10.0.0.1", Port: 8080}}
	b := []ereg.Instance{{IP: "10.0.0.2", Port: 8080}}
	if !instancesChanged(a, b) {
		t.Error("expected true for different IP")
	}
}

func TestInstancesChanged_DifferentPort(t *testing.T) {
	a := []ereg.Instance{{IP: "10.0.0.1", Port: 8080}}
	b := []ereg.Instance{{IP: "10.0.0.1", Port: 9090}}
	if !instancesChanged(a, b) {
		t.Error("expected true for different port")
	}
}

func TestInstancesChanged_Same(t *testing.T) {
	a := []ereg.Instance{{IP: "10.0.0.1", Port: 8080}}
	b := []ereg.Instance{{IP: "10.0.0.1", Port: 8080}}
	if instancesChanged(a, b) {
		t.Error("expected false for same instances")
	}
}

func TestInstancesChanged_BothEmpty(t *testing.T) {
	if instancesChanged(nil, nil) {
		t.Error("expected false for both empty")
	}
}

func TestGetHealthyInstances_MetadataIsolation(t *testing.T) {
	watcher := &InstanceWatcher{
		cache: map[string][]ereg.Instance{
			"svc": {
				{Service: "svc", IP: "10.0.0.1", Port: 8080, Metadata: map[string]string{"version": "v1"}},
			},
		},
	}

	instances1 := watcher.GetHealthyInstances("svc")
	instances2 := watcher.GetHealthyInstances("svc")

	instances1[0].Metadata["version"] = "mutated"
	instances1[0].Metadata["injected"] = "bad"

	if instances2[0].Metadata["version"] != "v1" {
		t.Errorf("cache corrupted: got version=%q, want %q", instances2[0].Metadata["version"], "v1")
	}
	if _, ok := instances2[0].Metadata["injected"]; ok {
		t.Error("cache corrupted: injected key leaked from caller mutation")
	}
}

func TestEventTypes(t *testing.T) {
	assert.DeepEqual(t, EventType(0), EventAdded)
	assert.DeepEqual(t, EventType(1), EventRemoved)
	assert.DeepEqual(t, EventType(2), EventChanged)
}

// waitFor 轮询等待条件成立（同步测试辅助，deadline 内不成立即 fatal）。
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within deadline")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestInstanceWatcher_SkipRoundDoesNotConsumeChange 是 #133 的回归测试：
// 回调派发被跳过的轮次（上一轮回调仍在异步执行、未排空）里发生的实例变更
// **不得被 cache 消费掉**。
//
// 旧实现先 `h.cache[svc] = copied` 再判 pending-skip：挂死/慢回调期间注册中心
// 发生的变更进了 cache 但派发被跳过；回调恢复后下一轮 diff（prev 已是新值）
// 为 false——变更永久丢失，订阅方停留在旧实例集且无重试/补发。
//
// 复现路径（对齐 issue 实测：interval=200ms、回调睡 6s、t=600ms 注入第二实例）：
//  1. 首轮派发 Added（1 实例），回调挂在 release 上（模拟慢/挂死回调）；
//  2. 注册中心新增第二实例，第二轮 checkService 因 inflight>0 跳过派发——
//     此刻 cache 必须仍是 1 实例（变更未被消费）；
//  3. 放行首轮回调、排空后第三轮重 diff：变更被重新发现并派发（Changed，2 实例），
//     cache 推进为 2。
func TestInstanceWatcher_SkipRoundDoesNotConsumeChange(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: map[string][]ereg.Instance{
		"svc-a": {{IP: "10.0.0.1", Port: 8080}},
	}}

	events := make(chan InstanceEvent, 16)
	release := make(chan struct{})
	h := NewInstanceWatcher(disc,
		WithWatchServices("svc-a"),
		WithWatchCallback(func(evt InstanceEvent) {
			if len(evt.Instances) == 1 {
				// 首轮回调挂起，制造「上一轮派发仍在异步执行」
				<-release
			}
			events <- evt
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 第一轮：派发决策同步完成，回调在独立 goroutine 异步执行（#167），
	// checkService 立即返回（外层 GoSafe 包装保留，证明调用方不再被拖住）。
	firstDone := make(chan struct{})
	async.GoSafe(func() {
		h.checkService(ctx, "svc-a")
		close(firstDone)
	})

	// 等回调真正进入挂起态（inflight>0 时 Discover 已返回，之后对
	// disc.InstancesMap 的写入不会与 Discover 竞争）。
	waitFor(t, 2*time.Second, func() bool {
		v, ok := h.inflight.Load("svc-a")
		return ok && v.(*atomic.Int32).Load() > 0
	})

	// 注册中心变更：新增第二实例（发生在回调挂起期间）。
	disc.InstancesMap["svc-a"] = []ereg.Instance{
		{IP: "10.0.0.1", Port: 8080},
		{IP: "10.0.0.2", Port: 8080},
	}

	// 第二轮：inflight>0 → 跳过派发。核心回归断言：跳过轮不得消费变更。
	h.checkService(ctx, "svc-a")
	if got := len(h.GetHealthyInstances("svc-a")); got != 1 {
		t.Fatalf("skip round consumed the change into cache: cached = %d instances, want 1", got)
	}

	// 放行首轮回调并等 inflight 排空。
	close(release)
	<-firstDone
	waitFor(t, 2*time.Second, func() bool {
		v, ok := h.inflight.Load("svc-a")
		return !ok || v.(*atomic.Int32).Load() == 0
	})

	// 第三轮：重 diff（prev=1 vs current=2）→ 变更重新派发。
	h.checkService(ctx, "svc-a")

	// 首轮 Added（回调放行后才投递）。
	select {
	case evt := <-events:
		if evt.Type != EventAdded || len(evt.Instances) != 1 {
			t.Fatalf("first event = %v (len %d), want Added with 1 instance", evt.Type, len(evt.Instances))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first event not delivered after release")
	}
	// 被跳过轮次的变更必须补发——旧实现在此永久丢失。
	select {
	case evt := <-events:
		if evt.Type != EventChanged || len(evt.Instances) != 2 {
			t.Fatalf("redispatched event = %v (len %d), want Changed with 2 instances", evt.Type, len(evt.Instances))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("change permanently lost: no redispatch after callback drained (#133)")
	}
	if got := len(h.GetHealthyInstances("svc-a")); got != 2 {
		t.Fatalf("cache after redispatch = %d instances, want 2", got)
	}
}

// evt.Instances 是缓存专属深拷贝（review L8），#167 后进一步是回调专属
// 深拷贝：回调中修改事件实例（含 Metadata map）不得污染 h.cache，也不得
// 影响下一次轮询的 prev diff 基线。若事件别名缓存，第一次回调的修改会让
// 第二次轮询误报 instancesChanged。
func TestInstanceWatcher_CallbackMutationDoesNotCorruptCache(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: map[string][]ereg.Instance{
		"svc-a": {{Service: "svc-a", IP: "10.0.0.1", Port: 8080, Metadata: map[string]string{"version": "v1"}}},
	}}

	var mu sync.Mutex
	eventCount := 0
	h := NewInstanceWatcher(disc,
		WithWatchServices("svc-a"),
		WithWatchCallback(func(evt InstanceEvent) {
			// 恶意/无意的回调修改：改 slice 元素与共享 Metadata map
			evt.Instances[0].IP = "hacked"
			evt.Instances[0].Metadata["version"] = "mutated"
			evt.Instances[0].Metadata["injected"] = "bad"
			mu.Lock()
			eventCount++
			mu.Unlock()
		}),
	)

	// 第一次轮询：触发事件（回调异步执行，#167，等回调跑完再断言）
	h.checkService(context.Background(), "svc-a")
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return eventCount == 1
	})

	// 缓存必须保持原值
	cached := h.GetHealthyInstances("svc-a")
	if len(cached) != 1 {
		t.Fatalf("expected 1 cached instance, got %d", len(cached))
	}
	if cached[0].IP != "10.0.0.1" {
		t.Errorf("cache corrupted via event slice: IP = %q, want %q", cached[0].IP, "10.0.0.1")
	}
	if cached[0].Metadata["version"] != "v1" {
		t.Errorf("cache corrupted via event metadata: version = %q, want %q", cached[0].Metadata["version"], "v1")
	}
	if _, ok := cached[0].Metadata["injected"]; ok {
		t.Error("cache corrupted: injected key leaked from callback mutation")
	}

	// 第二次轮询：实例未变化，不得因回调的修改误报第二个事件
	h.checkService(context.Background(), "svc-a")
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	gotCount := eventCount
	mu.Unlock()
	if gotCount != 1 {
		t.Errorf("expected no extra event after second poll, got %d total", gotCount)
	}
}
