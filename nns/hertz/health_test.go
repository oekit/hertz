package hertz

import (
	"context"
	"sync"
	"testing"
	"time"

	registrymocks "github.com/yi-nology/ekit-registry/registrymocks"
	"github.com/yi-nology/ekit-registry/registry"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
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
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]registry.Instance)}
	h := NewInstanceWatcher(disc)

	assert.DeepEqual(t, 10*time.Second, h.interval)
	assert.DeepEqual(t, 0, len(h.services))
	assert.NotNil(t, h.cache)
}

func TestNewInstanceWatcher_WithOptions(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]registry.Instance)}
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

func TestInstanceWatcher_GetHealthyInstances_Empty(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]registry.Instance)}
	h := NewInstanceWatcher(disc)

	instances := h.GetHealthyInstances("nonexistent")
	assert.DeepEqual(t, 0, len(instances))
}

func TestInstanceWatcher_GetHealthyInstances_Cached(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]registry.Instance)}
	h := NewInstanceWatcher(disc)

	h.mu.Lock()
	h.cache["svc-a"] = []registry.Instance{
		{IP: "10.0.0.1", Port: 8080},
	}
	h.mu.Unlock()

	instances := h.GetHealthyInstances("svc-a")
	assert.DeepEqual(t, 1, len(instances))
	assert.DeepEqual(t, "10.0.0.1", instances[0].IP)
}

func TestInstanceWatcher_GetHealthyInstances_ReturnsCopy(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]registry.Instance)}
	h := NewInstanceWatcher(disc)

	h.mu.Lock()
	h.cache["svc-a"] = []registry.Instance{
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
	disc := &registrymocks.Discoverer{InstancesMap: map[string][]registry.Instance{
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
	disc := &registrymocks.Discoverer{InstancesMap: map[string][]registry.Instance{
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
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]registry.Instance)}
	h := NewInstanceWatcher(disc)

	h.Stop()
	h.Stop() // Should not panic
}

func TestInstancesChanged_DifferentLength(t *testing.T) {
	a := []registry.Instance{{IP: "10.0.0.1", Port: 8080}}
	b := []registry.Instance{{IP: "10.0.0.1", Port: 8080}, {IP: "10.0.0.2", Port: 8080}}
	if !instancesChanged(a, b) {
		t.Error("expected true for different length")
	}
}

func TestInstancesChanged_DifferentIP(t *testing.T) {
	a := []registry.Instance{{IP: "10.0.0.1", Port: 8080}}
	b := []registry.Instance{{IP: "10.0.0.2", Port: 8080}}
	if !instancesChanged(a, b) {
		t.Error("expected true for different IP")
	}
}

func TestInstancesChanged_DifferentPort(t *testing.T) {
	a := []registry.Instance{{IP: "10.0.0.1", Port: 8080}}
	b := []registry.Instance{{IP: "10.0.0.1", Port: 9090}}
	if !instancesChanged(a, b) {
		t.Error("expected true for different port")
	}
}

func TestInstancesChanged_Same(t *testing.T) {
	a := []registry.Instance{{IP: "10.0.0.1", Port: 8080}}
	b := []registry.Instance{{IP: "10.0.0.1", Port: 8080}}
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
		cache: map[string][]registry.Instance{
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

// TestInstanceWatcher_CallbackMutationDoesNotCorruptCache 验证回调收到的
// evt.Instances 是缓存专属深拷贝（review L8）：回调中修改事件实例（含
// Metadata map）不得污染 h.cache，也不得影响下一次轮询的 prev diff 基线。
// 若事件别名缓存，第一次回调的修改会让第二次轮询误报 instancesChanged。
func TestInstanceWatcher_CallbackMutationDoesNotCorruptCache(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: map[string][]registry.Instance{
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

	// 第一次轮询：触发事件，回调修改其收到的实例列表
	h.checkService(context.Background(), "svc-a")

	mu.Lock()
	gotCount := eventCount
	mu.Unlock()
	if gotCount != 1 {
		t.Fatalf("expected 1 callback event after first poll, got %d", gotCount)
	}

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
	mu.Lock()
	gotCount = eventCount
	mu.Unlock()
	if gotCount != 1 {
		t.Errorf("expected no extra event after second poll, got %d total", gotCount)
	}
}
