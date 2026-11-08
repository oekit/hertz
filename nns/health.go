package nns

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/logx"
	ereg "github.com/oekit/registry"
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
	// inflight: serviceName → *atomic.Int32，仍在执行的回调 goroutine 数
	// （回调异步派发，#167）。若回调挂死，没有该计数则每轮 tick 都会新增
	// 一个泄漏 goroutine，无上限。计数 >0 时跳过本轮派发（cache 不动，
	// 排空后重 diff 补发，#133）。
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
	h.mu.Unlock()

	if !instancesChanged(prev, copied) {
		// 无变更：仅刷新缓存内容（与 prev 等价，覆盖元数据等不可比字段）。
		h.mu.Lock()
		h.cache[serviceName] = copied
		h.mu.Unlock()
		return
	}

	// 上一轮回调尚未跑完（慢/挂死回调仍在异步执行，#167）时跳过本轮派发，
	// 防止挂死回调导致 goroutine 每轮 tick 无界累积。
	// ⚠️ 跳过时**不得更新 cache**（#133）：cache 是 diff 的 prev 基线，若先把
	// 本轮变更消费进 cache 再跳过派发，回调恢复后下一轮 instancesChanged 必为
	// false——变更事件被永久吞掉，订阅方（客户端 LB 缓存类）停留在旧实例集且
	// 无任何重试/补发。保持 cache 不动 ⇒ 回调排空后的下一轮会重新发现同一变更
	// 并重新派发（cache 与订阅方一致地「暂时落后」，而非「永久分叉」）。
	pending, _ := h.inflight.LoadOrStore(serviceName, new(atomic.Int32))
	pendingCount := pending.(*atomic.Int32)
	if pendingCount.Load() > 0 {
		healthLog.Warn("previous callback dispatch still running, skip this round (cache untouched, will re-diff)", "service", serviceName)
		return
	}

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

	// 派发决策已通过：此刻才把本轮结果落进 cache（prev 基线推进为本次
	// 派发的内容，cache 始终反映「已派发（或正在派发）的最新实例集」）。
	h.mu.Lock()
	h.cache[serviceName] = copied
	h.mu.Unlock()

	// 回调异步派发（#167）：每个回调独立 goroutine（async.GoSafe，panic
	// 收编对齐仓内并发口径），checkService/ticker 主循环不再同步等待回调——
	// 修复前单回调在主循环内最多同步等 5s（3 回调各挂 5s = 本轮 15s），
	// 期间所有服务的 Discover 与变更检测被压后、ticker tick 被丢弃，单个
	// 挂死消费者把全部服务的实例感知延迟拖到分钟级。异步后主循环只做
	// 发现与派发决策；慢/挂死回调的影响被 inflight 计数（#133）收敛为
	// 「该服务跳过派发且 cache 不动」，排空后下一轮重 diff 补发。
	//
	// 派发决策与 inflight 计数仍在调用方 goroutine 内同步完成（上面的
	// LoadOrStore + 下面的 Add），派发仅执行体异步——「同服务去重」的
	// 判定窗口保持原子。
	for _, cb := range callbacks {
		callback := cb
		// 回调间隔离：并发执行下各回调拿各自的深拷贝。共享同一 evt 时
		// 一个回调改 Instances（含 Metadata map）会与其它回调竞争——
		// 串行派发时代仅靠「回调别改事件」的自觉，并发后必须显式隔离。
		cbEvt := evt
		cbEvt.Instances = deepCopyInstances(evt.Instances)
		pendingCount.Add(1)
		async.GoSafe(func() {
			defer pendingCount.Add(-1)
			callback(cbEvt)
		})
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
