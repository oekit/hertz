// Package hertz 提供 nns 与 CloudWeGo Hertz 集成的适配器。
//
// 本包包含：
//   - [NewRegistry] — 将 NNS ServiceClient 适配为 Hertz [registry.Registry]，支持 mock 模式。
//   - [NewResolver] — 将 NNS ServiceClient 适配为 Hertz [discovery.Resolver]，继承完整降级链。
//   - [InstanceWatcher] — 以定时轮询的方式通过 [registry.Discoverer] 监控服务实例变化，
//     并通过回调机制通知订阅者。注意本类型关注的是"实例列表变更"，与根 health 包的
//     HealthChecker（依赖探活，func(ctx) error）语义不同。
package hertz
