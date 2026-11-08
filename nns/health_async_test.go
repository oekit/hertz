package nns

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ereg "github.com/oekit/registry"
)

// --- #167 回归：回调异步派发，单个慢/挂死回调不得拖住轮询主循环 ---

// spyDiscoverer 是带 Discover 计数与可变实例集的最小 Discoverer 实现
// （registrymocks.Discoverer 无计数能力；计数用于断言「挂死回调不再
// 阻塞其它服务的 Discover」）。
type spyDiscoverer struct {
	mu    sync.Mutex
	svc   map[string][]ereg.Instance
	count int64
}

func newSpyDiscoverer(svc map[string][]ereg.Instance) *spyDiscoverer {
	return &spyDiscoverer{svc: svc}
}

func (d *spyDiscoverer) Discover(_ context.Context, serviceName string) ([]ereg.Instance, error) {
	d.mu.Lock()
	d.count++
	insts := d.svc[serviceName]
	d.mu.Unlock()
	out := make([]ereg.Instance, len(insts))
	copy(out, insts)
	return out, nil
}

func (d *spyDiscoverer) DiscoverAll(_ context.Context, serviceName string) ([]ereg.Instance, error) {
	return d.Discover(context.Background(), serviceName)
}

func (d *spyDiscoverer) discoverCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

func (d *spyDiscoverer) setInstances(serviceName string, insts []ereg.Instance) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.svc[serviceName] = insts
}

// TestInstanceWatcher167_SlowCallbackDoesNotBlockOtherServices 是 #167 的
// 核心回归：svc-a 的回调挂死期间，svc-b 的 Discover 轮询必须照常推进
// （修复前 checkAll 在 Start 的 ticker goroutine 内同步串行执行回调——
// 单回调挂 5s，期间所有服务的变更感知被压后、ticker tick 被丢弃）。
func TestInstanceWatcher167_SlowCallbackDoesNotBlockOtherServices(t *testing.T) {
	disc := newSpyDiscoverer(map[string][]ereg.Instance{
		"svc-a": {{IP: "10.0.0.1", Port: 8080}},
		"svc-b": {{IP: "10.0.0.2", Port: 8080}},
	})

	release := make(chan struct{})
	defer close(release) // 兜底放行，防测试失败时 goroutine 泄漏挂死
	h := NewInstanceWatcher(disc,
		WithWatchServices("svc-a", "svc-b"),
		WithWatchCallback(func(evt InstanceEvent) {
			if evt.Service == "svc-a" {
				<-release // svc-a 回调挂死
			}
		}),
	)

	// 首轮：svc-a 派发（挂死）+ svc-b 派发。
	h.checkAll(context.Background())
	afterFirst := disc.discoverCount()
	if afterFirst < 2 {
		t.Fatalf("first round must discover both services, got %d Discover calls", afterFirst)
	}

	// 第二轮立即执行：svc-a 回调仍挂着，但 checkAll 不等它（修复前此处
	// 要先同步等 svc-a 回调 5s 超时才轮到 svc-b 的 Discover）。
	done := make(chan struct{})
	go func() {
		h.checkAll(context.Background())
		close(done)
	}()
	select {
	case <-done:
		// checkAll 在挂死回调仍在执行的情况下完成——异步派发生效。
	case <-time.After(2 * time.Second):
		t.Fatal("checkAll blocked on hung svc-a callback: dispatch is still synchronous (#167)")
	}
	if disc.discoverCount() <= afterFirst {
		t.Fatal("svc-b was not re-discovered while svc-a callback is hung (#167)")
	}
}

// TestInstanceWatcher167_CallbackPanicDoesNotAffectOthers panic 收编：一个
// 回调 panic（async.GoSafe recover）不得影响同服务其它回调的执行，也不得
// 打断后续轮次的派发。
func TestInstanceWatcher167_CallbackPanicDoesNotAffectOthers(t *testing.T) {
	disc := newSpyDiscoverer(map[string][]ereg.Instance{
		"svc-a": {{IP: "10.0.0.1", Port: 8080}},
	})

	var mu sync.Mutex
	goodCalls := 0
	h := NewInstanceWatcher(disc,
		WithWatchServices("svc-a"),
		WithWatchCallback(func(InstanceEvent) { panic("boom: bad callback") }),
		WithWatchCallback(func(InstanceEvent) {
			mu.Lock()
			goodCalls++
			mu.Unlock()
		}),
	)

	// 第一轮：坏回调 panic 被 GoSafe 收编，好回调照常执行。
	h.checkService(context.Background(), "svc-a")
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return goodCalls >= 1
	})
	// 等第一轮两个回调 goroutine（含 panic 展开）全部排空——否则第二轮的
	// 派发决策可能撞上 inflight>0 被跳过（#133 语义），goodCalls 永远到不了 2。
	waitFor(t, 5*time.Second, func() bool {
		v, ok := h.inflight.Load("svc-a")
		return !ok || v.(*atomic.Int32).Load() == 0
	})

	// 第二轮（实例集变更以触发重派发）：inflight 已被 panic 的 goroutine
	// 归还（defer 计数），派发不因上一轮的 panic 而卡死；好回调再次执行。
	disc.setInstances("svc-a", []ereg.Instance{
		{IP: "10.0.0.1", Port: 8080},
		{IP: "10.0.0.2", Port: 8080},
	})
	h.checkService(context.Background(), "svc-a")
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := goodCalls
		mu.Unlock()
		if n >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("dispatch stalled after callback panic: goodCalls = %d, want >= 2 (#167)", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestInstanceWatcher167_PerCallbackEventIsolation 并发派发下各回调拿到的是
// 各自的深拷贝：一个回调改事件不得污染另一回调看到的事件（#167 串行改并发
// 后，共享 evt 会构成数据竞争）。
func TestInstanceWatcher167_PerCallbackEventIsolation(t *testing.T) {
	disc := newSpyDiscoverer(map[string][]ereg.Instance{
		"svc-a": {{Service: "svc-a", IP: "10.0.0.1", Port: 8080, Metadata: map[string]string{"v": "1"}}},
	})

	start := make(chan struct{})
	var mu sync.Mutex
	secondSaw := ""
	bothDone := make(chan struct{})
	var once sync.Once
	h := NewInstanceWatcher(disc,
		WithWatchServices("svc-a"),
		WithWatchCallback(func(evt InstanceEvent) {
			// 等第二个回调先记录原值，再改自己那份，制造并发交错窗口
			<-start
			evt.Instances[0].Metadata["v"] = "mutated-by-first"
			once.Do(func() { close(bothDone) })
		}),
		WithWatchCallback(func(evt InstanceEvent) {
			mu.Lock()
			secondSaw = evt.Instances[0].Metadata["v"]
			mu.Unlock()
			close(start) // 放行第一个回调的修改
			once.Do(func() { close(bothDone) })
		}),
	)

	h.checkService(context.Background(), "svc-a")
	select {
	case <-bothDone:
	case <-time.After(2 * time.Second):
		t.Fatal("callbacks did not both complete")
	}

	mu.Lock()
	defer mu.Unlock()
	if secondSaw != "1" {
		t.Fatalf("second callback saw mutated metadata %q, want pristine \"1\" (#167 per-callback isolation)", secondSaw)
	}
	cached := h.GetHealthyInstances("svc-a")
	if cached[0].Metadata["v"] != "1" {
		t.Fatalf("cache corrupted by callback mutation: %q", cached[0].Metadata["v"])
	}
}

// TestInstanceWatcher167_InflightSkipsWhileHung 挂死回调存在期间，同服务的
// 重复派发仍被 inflight 计数拦截（#133 语义在异步派发下保持），排空后
// 重 diff 补发。
func TestInstanceWatcher167_InflightSkipsWhileHung(t *testing.T) {
	disc := newSpyDiscoverer(map[string][]ereg.Instance{
		"svc-a": {{IP: "10.0.0.1", Port: 8080}},
	})

	release := make(chan struct{})
	unblock := func() {
		select {
		case <-release:
		default:
		}
	}
	defer unblock()
	h := NewInstanceWatcher(disc,
		WithWatchServices("svc-a"),
		WithWatchCallback(func(InstanceEvent) { <-release }),
	)

	h.checkService(context.Background(), "svc-a")
	waitFor(t, 2*time.Second, func() bool {
		v, ok := h.inflight.Load("svc-a")
		return ok && v.(*atomic.Int32).Load() > 0
	})

	// 变更实例集后再次轮询：inflight>0 ⇒ 跳过派发（cache 不动，#133）。
	disc.setInstances("svc-a", []ereg.Instance{
		{IP: "10.0.0.1", Port: 8080},
		{IP: "10.0.0.2", Port: 8080},
	})
	h.checkService(context.Background(), "svc-a")
	if got := len(h.GetHealthyInstances("svc-a")); got != 1 {
		t.Fatalf("skip round consumed change into cache: %d instances, want 1 (#133)", got)
	}

	// 放行后下一轮应重 diff 并派发 2 实例（cache 补发语义）。
	close(release)
	waitFor(t, 2*time.Second, func() bool {
		v, ok := h.inflight.Load("svc-a")
		return !ok || v.(*atomic.Int32).Load() == 0
	})
	h.checkService(context.Background(), "svc-a")
	if got := len(h.GetHealthyInstances("svc-a")); got != 2 {
		t.Fatalf("cache after redispatch = %d instances, want 2", got)
	}
}
