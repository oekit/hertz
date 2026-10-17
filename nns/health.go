package nns

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/logx"
	ereg "github.com/oekit/registry/registry"
)

var healthLog = logx.Delegating("nns-hertz")

// EventType 实例变更事件类型。
type EventType int

const (
	// EventAdded 实例新增事件。
	EventAdded EventType = iota
	// EventRemoved 实例移除事件。
	EventRemoved
	// EventChanged 实例变更事件。
	EventChanged
)

// InstanceEvent 实例变更事件，包含事件类型、服务名和实例列表。
type InstanceEvent struct {
	Type      EventType
	Service   string
	Instances []ereg.Instance
}

// InstanceWatcher 服务健康检查器，定期轮询服务实例并在变更时触发回调。
//
// 基于 [ereg.Discoverer] 接口，与具体注册中心解耦。
type InstanceWatcher struct {
	discoverer ereg.Discoverer
	interval   time.Duration
	services   []string
	mu         sync.RWMutex
	cache      map[string][]ereg.Instance
	callbacks  []func(InstanceEvent)
	stopCh     chan struct{}
	stopOnce   sync.Once
	// inflight: serviceName → *atomic.Int32，仍在执行的回调 goroutine 数。
	// 超时只放弃等待、goroutine 仍在运行；若回调挂死，没有该计数则每轮
	// tick 都会新增一个泄漏 goroutine，无上限。计数 >0 时跳过本轮派发。
	inflight sync.Map
}

// WatchOption InstanceWatcher 的配置选项函数类型。
type WatchOption func(*InstanceWatcher)

// WithWatchInterval 设置健康检查的轮询间隔时间。
func WithWatchInterval(d time.Duration) WatchOption {
	return func(h *InstanceWatcher) { h.interval = d }
}

// WithWatchServices 设置需要健康检查的服务列表。
func WithWatchServices(services ...string) WatchOption {
	return func(h *InstanceWatcher) { h.services = services }
}

// WithWatchCallback 添加实例变更事件回调函数。
func WithWatchCallback(fn func(InstanceEvent)) WatchOption {
	return func(h *InstanceWatcher) { h.callbacks = append(h.callbacks, fn) }
}

// NewInstanceWatcher 根据服务发现器和可选参数创建健康检查器实例。
//
// discoverer 不能为 nil，否则 panic。
func NewInstanceWatcher(discoverer ereg.Discoverer, opts ...WatchOption) *InstanceWatcher {
	if discoverer == nil {
		panic("hertz: NewInstanceWatcher requires non-nil discoverer")
	}
	h := &InstanceWatcher{
		discoverer: discoverer,
		interval:   10 * time.Second,
		cache:      make(map[string][]ereg.Instance),
		stopCh:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Start 启动健康检查器，定期轮询服务实例状态直到上下文取消或调用 Stop。
func (h *InstanceWatcher) Start(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	h.checkAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.checkAll(ctx)
		}
	}
}

// Stop 停止健康检查器。
func (h *InstanceWatcher) Stop() {
	h.stopOnce.Do(func() {
		close(h.stopCh)
	})
}

// GetHealthyInstances 获取指定服务缓存的健康实例列表。
func (h *InstanceWatcher) GetHealthyInstances(serviceName string) []ereg.Instance {
	h.mu.RLock()
	defer h.mu.RUnlock()
	instances, ok := h.cache[serviceName]
	if !ok {
		return nil
	}
	return deepCopyInstances(instances)
}

func (h *InstanceWatcher) checkAll(ctx context.Context) {
	for _, svc := range h.services {
		select {
		case <-ctx.Done():
			return
		default:
		}
		h.checkService(ctx, svc)
	}
}

func (h *InstanceWatcher) checkService(ctx context.Context, serviceName string) {
	current, err := h.discoverer.Discover(ctx, serviceName)
	if err != nil {
		healthLog.Error("health check failed", "service", serviceName, "err", err)
		return
	}

	// 深拷贝实例列表，防止回调或外部代码修改缓存数据
	copied := deepCopyInstances(current)

	h.mu.Lock()
	prev := h.cache[serviceName]
	h.cache[serviceName] = copied
	h.mu.Unlock()

	if instancesChanged(prev, copied) {
		evt := InstanceEvent{
			Type:    EventChanged,
			Service: serviceName,
			// 回调专属深拷贝：copied 同时是 h.cache 的存储值，若直接交给回调，
			// 回调中的修改会污染缓存与下一次 diff 的 prev 基线（review L8）
			Instances: deepCopyInstances(copied),
		}
		if len(prev) == 0 && len(copied) > 0 {
			evt.Type = EventAdded
		} else if len(copied) == 0 && len(prev) > 0 {
			evt.Type = EventRemoved
		}
		h.mu.RLock()
		callbacks := make([]func(InstanceEvent), len(h.callbacks))
		copy(callbacks, h.callbacks)
		h.mu.RUnlock()

		// 上一轮回调尚未跑完（挂死或超过 5s 超时窗口）时跳过本轮派发，
		// 防止挂死回调导致 goroutine 与 channel 每轮 tick 无界累积
		pending, _ := h.inflight.LoadOrStore(serviceName, new(atomic.Int32))
		pendingCount := pending.(*atomic.Int32)
		if pendingCount.Load() > 0 {
			healthLog.Warn("previous callback dispatch still running, skip this round", "service", serviceName)
			return
		}

		// 每个 callback 共用一个超时计时器，循环复用前必须清掉 pending tick，
		// 否则上一次 fire 的 tick 会让下一次 select 立刻命中误报 timeout。
		timeoutTimer := time.NewTimer(5 * time.Second)
		// 第一个 iteration 不需要 drain（timer 刚 NewTimer，timer.C 一定空）。
		first := true
		for _, cb := range callbacks {
			done := make(chan struct{})
			callback := cb
			pendingCount.Add(1)
			async.GoSafe(func() {
				defer func() {
					pendingCount.Add(-1)
					close(done)
				}()
				callback(evt)
			})
			if !first {
				// Stop + drain 模式：上一次循环可能因为 timeout 已 fire，
				// 残留 tick 在 channel 上。必须先 Stop 再消费，否则下一次
				// Reset 不会清掉这个 stale tick。
				if !timeoutTimer.Stop() {
					select {
					case <-timeoutTimer.C:
					default:
					}
				}
			}
			first = false
			timeoutTimer.Reset(5 * time.Second)
			select {
			case <-done:
			case <-timeoutTimer.C:
				healthLog.Warn("callback timeout", "service", serviceName)
			}
		}
		timeoutTimer.Stop()
	}
}

func instancesChanged(a, b []ereg.Instance) bool {
	if len(a) != len(b) {
		return true
	}
	// 排序后比较，避免顺序不同导致误报变更
	sortedA := sortInstances(a)
	sortedB := sortInstances(b)
	for i := range sortedA {
		if sortedA[i].IP != sortedB[i].IP || sortedA[i].Port != sortedB[i].Port || sortedA[i].Weight != sortedB[i].Weight {
			return true
		}
		if !metadataEqual(sortedA[i].Metadata, sortedB[i].Metadata) {
			return true
		}
	}
	return false
}

// sortInstances 按 IP:Port 排序实例列表（返回副本，不修改原 slice）。
func sortInstances(instances []ereg.Instance) []ereg.Instance {
	sorted := make([]ereg.Instance, len(instances))
	copy(sorted, instances)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].IP != sorted[j].IP {
			return sorted[i].IP < sorted[j].IP
		}
		return sorted[i].Port < sorted[j].Port
	})
	return sorted
}

// metadataEqual 比较两个 metadata map 是否相等。
func metadataEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// deepCopyInstances 深拷贝实例列表（含 Metadata map）。
func deepCopyInstances(instances []ereg.Instance) []ereg.Instance {
	if instances == nil {
		return nil
	}
	out := make([]ereg.Instance, len(instances))
	copy(out, instances)
	for i := range out {
		if out[i].Metadata != nil {
			meta := make(map[string]string, len(out[i].Metadata))
			for k, v := range out[i].Metadata {
				meta[k] = v
			}
			out[i].Metadata = meta
		}
	}
	return out
}
