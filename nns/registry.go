package nns

import (
	"context"
	"fmt"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	nnsclient "github.com/oekit/nns/client"
	"github.com/oekit/registry/sdbridge"
)

// Registry 包装 nns ServiceClient，实现 Hertz server/registry.Registry。
//
// 与 registry.NewRegistry（薄适配，直接调 registrar）不同，
// 本 adapter 使用 ServiceClient 注册到其 registrar 接口，
// 支持 mock 模式（registrar 为 nil 时静默跳过）。
//
// 导出为具体类型（而非只返回接口）：Hertz 的 [hregistry.Registry] 接口
// 没有 ctx 形参，本类型额外提供带 ctx 的
// [Registry.RegisterContext] / [Registry.DeregisterContext]。
type Registry struct {
	client        *nnsclient.ServiceClient
	defaultWeight int
}

var _ hregistry.Registry = (*Registry)(nil)

// ContextRegistry 是带 ctx 的注册扩展点：Hertz 的 [hregistry.Registry]
// 接口没有 ctx 形参（engine 在 Spin/Shutdown 时调用 Register/Deregister），
// 需要取消/超时的场景用本接口显式传 ctx。
type ContextRegistry interface {
	// RegisterContext 带 ctx 注册服务实例。
	RegisterContext(ctx context.Context, info *hregistry.Info) error
	// DeregisterContext 带 ctx 注销服务实例。
	DeregisterContext(ctx context.Context, info *hregistry.Info) error
}

var _ ContextRegistry = (*Registry)(nil)

// RegistryOption 是 [NewRegistry] 的配置选项函数类型。
type RegistryOption func(*Registry)

// WithDefaultWeight 设置当 Hertz Info.Weight 为 0 时使用的默认权重。
// 默认值 10（与 Hertz [ereg.DefaultWeight] 一致）。
func WithDefaultWeight(w int) RegistryOption {
	return func(r *Registry) { r.defaultWeight = w }
}

// NewRegistry 创建基于 NNS ServiceClient 的 Hertz Registry。
//
// 返回的 Registry 可直接传给 Hertz Server 的 [server.WithRegistry] 选项。
// Hertz 在 server.Spin 启动时自动调用 Register，退出时调用 Deregister。
//
// 推荐通过 NNSClient.ServiceClient() 访问底层 client：
//
//	nnshertz.NewRegistry(nnsc.ServiceClient())
//
// client 为 nil 时 panic——与同包 [NewInstanceWatcher] 的 nil discoverer
// 口径一致（构造期配置错误，启动即暴露），避免运行期空指针。
//
// ⚠️ 返回类型由 [hregistry.Registry] 接口改为具体类型 *Registry（仍实现该
// 接口，赋值/传参不受影响）：为的是让带 ctx 的注册方法可直接调用。
func NewRegistry(c *nnsclient.ServiceClient, opts ...RegistryOption) *Registry {
	if c == nil {
		panic("hertz: NewRegistry requires non-nil client")
	}
	r := &Registry{
		client:        c,
		defaultWeight: 10,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register 将 Hertz 服务实例注册到 NNS 注册中心。
//
// Hertz 的 [hregistry.Registry] 接口没有 ctx 形参（engine 在 Spin 时调用），
// 故此处用 context.Background()；需要 ctx 的场景用 [Registry.RegisterContext]。
func (r *Registry) Register(info *hregistry.Info) error {
	return r.RegisterContext(context.Background(), info)
}

// RegisterContext 与 [Registry.Register] 同语义，但显式传入 ctx
// （可取消/可超时）。
func (r *Registry) RegisterContext(ctx context.Context, info *hregistry.Info) error {
	if info == nil {
		return fmt.Errorf("nns-hertz registry: info is nil")
	}

	// weight <= 0 时使用 r.defaultWeight 兜底（委托 sdbridge.ToInstance）
	inst, err := sdbridge.ToInstance(info.ServiceName, info.Addr, info.Weight, r.defaultWeight, info.Tags)
	if err != nil {
		return fmt.Errorf("nns-hertz registry: %w", err)
	}

	if r.client.Registrar() == nil {
		// mock 模式是有意跳过；非 mock 降级（启动时 Nacos 不可达）静默放行
		// 会让服务"正常启动但永不可被发现"且无当次调用信号——显式报错
		// 对齐 bootstrap Register 失败即回滚的行为
		if r.client.MockMode() {
			return nil // mock 模式：静默跳过
		}
		return fmt.Errorf("nns-hertz registry: registrar unavailable (degraded, not mock mode); service would be undiscoverable")
	}
	return r.client.Registrar().Register(ctx, inst)
}

// Deregister 从 NNS 注册中心注销 Hertz 服务实例。
// 与 Register 对称：保留 tags 并走 defaultWeight 兜底，
// 依赖 metadata/权重匹配的注册中心才能正确注销。
func (r *Registry) Deregister(info *hregistry.Info) error {
	return r.DeregisterContext(context.Background(), info)
}

// DeregisterContext 与 [Registry.Deregister] 同语义，但显式传入 ctx
// （可取消/可超时）。
func (r *Registry) DeregisterContext(ctx context.Context, info *hregistry.Info) error {
	if info == nil {
		return fmt.Errorf("nns-hertz registry: info is nil")
	}

	inst, err := sdbridge.ToInstance(info.ServiceName, info.Addr, info.Weight, r.defaultWeight, info.Tags)
	if err != nil {
		return fmt.Errorf("nns-hertz registry: %w", err)
	}

	if r.client.Registrar() == nil {
		if r.client.MockMode() {
			return nil // mock 模式：静默跳过
		}
		return fmt.Errorf("nns-hertz registry: registrar unavailable (degraded, not mock mode)")
	}
	return r.client.Registrar().Deregister(ctx, inst)
}
