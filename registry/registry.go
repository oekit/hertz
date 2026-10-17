package registry

import (
	"context"
	"fmt"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	ereg "github.com/oekit/registry"
	"github.com/oekit/registry/sdbridge"
)

// hertzRegistry 将 ekit [ereg.Registrar] 适配为 Hertz [hregistry.Registry]。
type hertzRegistry struct {
	reg           ereg.Registrar
	defaultWeight int
}

var _ hregistry.Registry = (*hertzRegistry)(nil)

// RegistryOption 是 [NewRegistry] 的配置选项函数类型。
type RegistryOption func(*hertzRegistry)

// WithDefaultWeight 设置当 Hertz Info.Weight 为 0 时使用的默认权重。
// 默认值 10（与 Hertz [ereg.DefaultWeight] 一致）。
func WithDefaultWeight(w int) RegistryOption {
	return func(r *hertzRegistry) { r.defaultWeight = w }
}

// NewRegistry 将 ekit [ereg.Registrar] 适配为 Hertz [hregistry.Registry]。
//
// 返回的 Registry 可直接传给 Hertz Server 的 [server.WithRegistry] 选项。
// Hertz 在 server.Spin 启动时自动调用 Register，退出时调用 Deregister。
func NewRegistry(reg ereg.Registrar, opts ...RegistryOption) hregistry.Registry {
	r := &hertzRegistry{
		reg:           reg,
		defaultWeight: 10,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register 将 Hertz 服务实例注册到 ekit 注册中心。
func (r *hertzRegistry) Register(info *hregistry.Info) error {
	inst, err := toInstance(info, r.defaultWeight)
	if err != nil {
		return fmt.Errorf("hertz registry: register: %w", err)
	}
	if err := r.reg.Register(context.Background(), inst); err != nil {
		return fmt.Errorf("hertz registry: register %s: %w", info.ServiceName, err)
	}
	return nil
}

// Deregister 从 ekit 注册中心注销 Hertz 服务实例。
func (r *hertzRegistry) Deregister(info *hregistry.Info) error {
	inst, err := toInstance(info, r.defaultWeight)
	if err != nil {
		return fmt.Errorf("hertz registry: deregister: %w", err)
	}
	if err := r.reg.Deregister(context.Background(), inst); err != nil {
		return fmt.Errorf("hertz registry: deregister %s: %w", info.ServiceName, err)
	}
	return nil
}

// toInstance 将 Hertz registry.Info 转换为 ekit ereg.Instance
// （weight <= 0 时使用 defaultWeight 兜底，委托 [sdbridge.ToInstance]）。
func toInstance(info *hregistry.Info, defaultWeight int) (ereg.Instance, error) {
	if info == nil {
		return ereg.Instance{}, fmt.Errorf("info is nil")
	}
	return sdbridge.ToInstance(info.ServiceName, info.Addr, info.Weight, defaultWeight, info.Tags)
}
