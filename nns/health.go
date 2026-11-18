// InstanceWatcher 导出面（#319）：实例轮询 watcher 的实现自本文件迁入
// github.com/oekit/registry/instancewatcher（oekit/registry#319 迁出单源，
// 类型名与行为不变，hertz 特有日志组件 healthLog/"nns-hertz" 随迁改注册名
// "registry/instancewatcher"），本文件改为薄 re-export——类型别名
// type X = instancewatcher.X + 构造函数薄委托，既有导出符号与调用形态不变；
// 行为语义（轮询 diff、异步派发 #167、inflight 跳过 #133 等）由
// registry/instancewatcher 的测试覆盖。
package nns

import (
	"time"

	ereg "github.com/oekit/registry"
	"github.com/oekit/registry/instancewatcher"
)

// EventType 实例变更事件类型。
type EventType = instancewatcher.EventType

const (
	// EventAdded 实例新增事件。
	EventAdded = instancewatcher.EventAdded
	// EventRemoved 实例移除事件。
	EventRemoved = instancewatcher.EventRemoved
	// EventChanged 实例变更事件。
	EventChanged = instancewatcher.EventChanged
)

// InstanceEvent 实例变更事件，包含事件类型、服务名和实例列表。
type InstanceEvent = instancewatcher.InstanceEvent

// InstanceWatcher 服务健康检查器（实例轮询 watcher），定期轮询服务实例并在
// 变更时触发回调。基于 [ereg.Discoverer] 接口，与具体注册中心解耦。
//
// 实现单源在 [instancewatcher.InstanceWatcher]（#319 迁出），本类型是它的
// 别名 re-export。
type InstanceWatcher = instancewatcher.InstanceWatcher

// WatchOption InstanceWatcher 的配置选项函数类型。
type WatchOption = instancewatcher.WatchOption

// WithWatchInterval 设置健康检查的轮询间隔时间。
//
// d <= 0 时 panic——构造期配置错误启动即暴露（与 [NewInstanceWatcher] 的
// nil discoverer fail-fast 口径一致）。此前无校验：Start 内
// time.NewTicker(h.interval) 对 0/负值直接 panic，而 Start 由业务自行起
// goroutine 调用，panic 未包 recover 即进程退出（#218）。
func WithWatchInterval(d time.Duration) WatchOption {
	return instancewatcher.WithWatchInterval(d)
}

// WithWatchServices 设置需要健康检查的服务列表。
//
// 入口深拷贝落库（hertz#222）——不持有调用方切片（别名写穿），调用方构造后
// 复用/改写同一底层数组不会静默改变运行中 watcher 的服务集合。
func WithWatchServices(services ...string) WatchOption {
	return instancewatcher.WithWatchServices(services...)
}

// WithWatchCallback 添加实例变更事件回调函数。
func WithWatchCallback(fn func(InstanceEvent)) WatchOption {
	return instancewatcher.WithWatchCallback(fn)
}

// NewInstanceWatcher 根据服务发现器和可选参数创建健康检查器实例。
//
// discoverer 不能为 nil，否则 panic（构造期 fail-fast，单源经
// sdbridge.EnsureNotNil，口径同 hertz#219）。薄委托
// [instancewatcher.NewInstanceWatcher]。
func NewInstanceWatcher(discoverer ereg.Discoverer, opts ...WatchOption) *InstanceWatcher {
	return instancewatcher.NewInstanceWatcher(discoverer, opts...)
}
