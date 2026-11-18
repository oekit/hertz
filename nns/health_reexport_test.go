package nns

import (
	"context"
	"testing"
	"time"

	ereg "github.com/oekit/registry"
	"github.com/oekit/registry/instancewatcher"
	registrymocks "github.com/oekit/registry/registrymocks"
)

// #319 re-export 面的钉子：InstanceWatcher 实现迁入 registry/instancewatcher
// 后，本包保留的是类型别名 + 构造函数薄委托。行为语义（轮询 diff、异步派发、
// inflight 跳过、选项校验等）由 registry/instancewatcher 的 watcher_test.go /
// watcher_async_test.go 覆盖（自本仓迁出时的同款用例）；此处只钉 re-export 面
// 本身——别名与单源类型同一（跨包互操作不经转换）、构造委托可达、nil
// fail-fast 语义经壳仍生效——防止别名断裂或委托签名漂移。

// sinkXxx 以函数参数形态钉类型别名同一性（不用 var 声明——staticcheck
// ST1023/QF1011 会要求省略可推断的显式类型，而显式类型正是别名钉子的本体）：
// 单源产物可作实参传入本包参数类型（及反向），可编译即同一类型。
func sinkWatcher(*InstanceWatcher)                         {}
func sinkWatcherSrc(*instancewatcher.InstanceWatcher)      {}
func sinkEvent(instancewatcher.InstanceEvent)              {}
func sinkEventSrc(InstanceEvent)                           {}
func sinkOption(instancewatcher.WatchOption)               {}
func sinkOptionSrc(WatchOption)                            {}
func sinkEventType(instancewatcher.EventType) EventType    { return EventChanged }
func sinkEventTypeSrc(EventType) instancewatcher.EventType { return instancewatcher.EventChanged }

// TestInstanceWatcherReExport_AliasIdentity 钉类型别名同一性：hertz/nns 的
// 导出类型必须**就是** instancewatcher 的类型（type X = X 别名），而非同名
// 新类型——双向赋值可编译即同一。
func TestInstanceWatcherReExport_AliasIdentity(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}

	sinkWatcher(instancewatcher.NewInstanceWatcher(disc))
	sinkWatcherSrc(NewInstanceWatcher(disc))
	sinkEvent(InstanceEvent{})
	sinkEventSrc(instancewatcher.InstanceEvent{})
	sinkOption(WithWatchInterval(time.Second))
	sinkOptionSrc(instancewatcher.WithWatchInterval(time.Second))
	if got := sinkEventType(instancewatcher.EventChanged); got != EventChanged {
		t.Fatalf("sinkEventType = %v, want EventChanged", got)
	}
	if got := sinkEventTypeSrc(EventChanged); got != instancewatcher.EventChanged {
		t.Fatalf("sinkEventTypeSrc = %v, want instancewatcher.EventChanged", got)
	}

	// 常量同一：事件类型值与单源逐字相等。
	if EventAdded != instancewatcher.EventAdded ||
		EventRemoved != instancewatcher.EventRemoved ||
		EventChanged != instancewatcher.EventChanged {
		t.Fatal("event constants diverged from registry/instancewatcher")
	}
}

// TestInstanceWatcherReExport_NilDiscovererPanics 钉构造委托的 fail-fast
// 语义经壳仍生效（单源经 sdbridge.EnsureNotNil，hertz#219 口径）。
func TestInstanceWatcherReExport_NilDiscovererPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewInstanceWatcher(nil) must panic via re-export delegation")
		}
	}()
	NewInstanceWatcher(nil)
}

// TestInstanceWatcherReExport_StartStop 走一遍经壳构造的 Start/Stop 生命周期，
// 钉薄委托产物可正常驱动（行为细节由单源测试覆盖）。
func TestInstanceWatcherReExport_StartStop(t *testing.T) {
	disc := &registrymocks.Discoverer{InstancesMap: make(map[string][]ereg.Instance)}
	w := NewInstanceWatcher(disc, WithWatchInterval(10*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	go w.Start(ctx)
	time.Sleep(30 * time.Millisecond)
	w.Stop() // sync.Once 幂等
	w.Stop()
	cancel()
}
