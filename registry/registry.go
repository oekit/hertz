package registry

import (
	"context"
	"time"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	"github.com/oekit/pkg/errors"
	ereg "github.com/oekit/registry"
	"github.com/oekit/registry/sdbridge"
)

// defaultRegisterTimeout 是 Register/Deregister 的缺省单次预算（#317，
// kitex/kitex #191 的 hertz 孪生）：hregistry.Registry 接口无 ctx，框架在
// engine.Spin 时自动 Register、优雅关停时 Deregister——裸
// context.Background() 则底层 Registrar 拿到的 ctx 永无截止时间，注册中心
// 不可达时（反）注册退化为 SDK 内部阻塞时长，拖住整个优雅关停序列。
// 缺省 3s，应显著小于 bootstrap 关停 closer 的预算（defaultTimeout），
// 给错误处理与后续 closer 留出余量。
const defaultRegisterTimeout = 3 * time.Second

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
// 构造期 fail-fast（#219，文案口径对齐 nns 包 NewRegistry）。
func NewRegistry(reg ereg.Registrar, opts ...RegistryOption) hregistry.Registry {
	if reg == nil {
		panic("hertz: NewRegistry requires non-nil registrar")
	}
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
		return errors.Wrapf(err, "hertz registry: register")
	}
	// 单次预算（#317，kitex #191 的 hertz 孪生）：接口无 ctx，这里以
	// WithTimeout(Background) 补上，底层 Registrar 的取消/超时语义自此生效。
	ctx, cancel := context.WithTimeout(context.Background(), defaultRegisterTimeout)
	defer cancel()
	if err := r.reg.Register(ctx, inst); err != nil {
		return errors.Wrapf(err, "hertz registry: register %s", info.ServiceName)
	}
	return nil
}

// Deregister 从 ekit 注册中心注销 Hertz 服务实例。
func (r *hertzRegistry) Deregister(info *hregistry.Info) error {
	inst, err := toInstance(info, r.defaultWeight)
	if err != nil {
		return errors.Wrapf(err, "hertz registry: deregister")
	}
	// 单次预算（#317）：server 优雅关停路径的 Deregister 不再有无限预算，
	// 注册中心不可达时最多阻塞 defaultRegisterTimeout。
	ctx, cancel := context.WithTimeout(context.Background(), defaultRegisterTimeout)
	defer cancel()
	if err := r.reg.Deregister(ctx, inst); err != nil {
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
