package nns

import (
	"context"
	"fmt"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	nnsclient "github.com/oekit/nns/nns/client"
	"github.com/oekit/registry/registry/sdbridge"
)

// nnsRegistry 包装 nns ServiceClient，实现 Hertz server/registry.Registry。
//
// 与 registry.NewRegistry（薄适配，直接调 registrar）不同，
// 本 adapter 使用 ServiceClient 注册到其 registrar 接口，
// 支持 mock 模式（registrar 为 nil 时静默跳过）。
type nnsRegistry struct {
	client        *nnsclient.ServiceClient
	defaultWeight int
}

var _ hregistry.Registry = (*nnsRegistry)(nil)

// RegistryOption 是 [NewRegistry] 的配置选项函数类型。
type RegistryOption func(*nnsRegistry)

// WithDefaultWeight 设置当 Hertz Info.Weight 为 0 时使用的默认权重。
// 默认值 10（与 Hertz [ereg.DefaultWeight] 一致）。
func WithDefaultWeight(w int) RegistryOption {
	return func(r *nnsRegistry) { r.defaultWeight = w }
}

// NewRegistry 创建基于 NNS ServiceClient 的 Hertz Registry。
//
// 返回的 Registry 可直接传给 Hertz Server 的 [server.WithRegistry] 选项。
// Hertz 在 server.Spin 启动时自动调用 Register，退出时调用 Deregister。
//
// 推荐通过 NNSClient.ServiceClient() 访问底层 client：
//
//	nnshertz.NewRegistry(nnsc.ServiceClient())
func NewRegistry(c *nnsclient.ServiceClient, opts ...RegistryOption) hregistry.Registry {
	r := &nnsRegistry{
		client:        c,
		defaultWeight: 10,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register 将 Hertz 服务实例注册到 NNS 注册中心。
func (r *nnsRegistry) Register(info *hregistry.Info) error {
	if info == nil {
		return fmt.Errorf("nns-hertz registry: info is nil")
	}

	// weight <= 0 时使用 r.defaultWeight 兜底（委托 sdbridge.ToInstance）
	inst, err := sdbridge.ToInstance(info.ServiceName, info.Addr, info.Weight, r.defaultWeight, info.Tags)
	if err != nil {
		return fmt.Errorf("nns-hertz registry: %w", err)
	}

	if r.client.GetRegistrar() == nil {
		// mock 模式是有意跳过；非 mock 降级（启动时 Nacos 不可达）静默放行
		// 会让服务"正常启动但永不可被发现"且无当次调用信号——显式报错
		// 对齐 bootstrap Register 失败即回滚的行为
		if r.client.MockMode() {
			return nil // mock 模式：静默跳过
		}
		return fmt.Errorf("nns-hertz registry: registrar unavailable (degraded, not mock mode); service would be undiscoverable")
	}
	return r.client.GetRegistrar().Register(context.Background(), inst)
}

// Deregister 从 NNS 注册中心注销 Hertz 服务实例。
// 与 Register 对称：保留 tags 并走 defaultWeight 兜底，
// 依赖 metadata/权重匹配的注册中心才能正确注销。
func (r *nnsRegistry) Deregister(info *hregistry.Info) error {
	if info == nil {
		return fmt.Errorf("nns-hertz registry: info is nil")
	}

	inst, err := sdbridge.ToInstance(info.ServiceName, info.Addr, info.Weight, r.defaultWeight, info.Tags)
	if err != nil {
		return fmt.Errorf("nns-hertz registry: %w", err)
	}

	if r.client.GetRegistrar() == nil {
		if r.client.MockMode() {
			return nil // mock 模式：静默跳过
		}
		return fmt.Errorf("nns-hertz registry: registrar unavailable (degraded, not mock mode)")
	}
	return r.client.GetRegistrar().Deregister(context.Background(), inst)
}
