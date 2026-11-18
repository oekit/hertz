package registry

import (
	"context"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	"github.com/oekit/pkg/errors"
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
//
// reg 为 nil 时 panic——此前无校验，nil 接口上的 r.reg.Register(...) 会拖到
// engine.Spin 的注册路径（hertz 内部 goroutine）才 panic，症状是「启动期
// 未知来源 panic」；本包与 nns 包两个 Registry 适配器对同一错误形态统一为
// 构造期 fail-fast（#219，文案口径对齐 nns 包 NewRegistry）。nil fail-fast
// 的实现单源自 [sdbridge.EnsureNotNil]（#216 消费，panic 文案逐字不变）。
func NewRegistry(reg ereg.Registrar, opts ...RegistryOption) hregistry.Registry {
	sdbridge.EnsureNotNil(reg, "hertz: NewRegistry", "registrar")
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
//
// 单次预算（#317，kitex #191 的 hertz 孪生）收口单源
// [sdbridge.RegisterWithBudget]（#216 消费，删手写 WithTimeout 拷贝）：
// hregistry.Registry 接口无 ctx，框架在 engine.Spin 时自动 Register——缺省
// 预算 sdbridge.DefaultRegisterBudget（3s）内注册中心不可达则失败返回，不再
// 退化为 SDK 内部阻塞时长拖住启动序列；错误包装与文案不变。
func (r *hertzRegistry) Register(info *hregistry.Info) error {
	inst, err := toInstance(info, r.defaultWeight)
	if err != nil {
		return errors.Wrapf(err, "hertz registry: register")
	}
	if err := sdbridge.RegisterWithBudget(context.Background(), r.reg, inst, sdbridge.DefaultRegisterBudget); err != nil {
		return errors.Wrapf(err, "hertz registry: register %s", info.ServiceName)
	}
	return nil
}

// Deregister 从 ekit 注册中心注销 Hertz 服务实例。
//
// 单次预算（#317）收口单源 [sdbridge.DeregisterWithBudget]（#216 消费）：
// server 优雅关停路径的 Deregister 不再有无限预算，注册中心不可达时最多
// 阻塞缺省预算（3s），不再拖死关停序列。
func (r *hertzRegistry) Deregister(info *hregistry.Info) error {
	inst, err := toInstance(info, r.defaultWeight)
	if err != nil {
		return errors.Wrapf(err, "hertz registry: deregister")
	}
	if err := sdbridge.DeregisterWithBudget(context.Background(), r.reg, inst, sdbridge.DefaultRegisterBudget); err != nil {
		return errors.Wrapf(err, "hertz registry: deregister %s", info.ServiceName)
	}
	return nil
}

// toInstance 将 Hertz registry.Info 转换为 ekit ereg.Instance
// （weight <= 0 时使用 defaultWeight 兜底，委托 [sdbridge.ToInstance]）。
func toInstance(info *hregistry.Info, defaultWeight int) (ereg.Instance, error) {
	if info == nil {
		return ereg.Instance{}, errors.New("info is nil")
	}
	return sdbridge.ToInstance(info.ServiceName, info.Addr, info.Weight, defaultWeight, info.Tags)
}
