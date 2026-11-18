package nns

import (
	"context"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	nnsclient "github.com/oekit/nns/client"
	"github.com/oekit/pkg/errors"
	ereg "github.com/oekit/registry"
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

// degraded 消息正文（#217 收敛前 Register/Deregister 两段各写一份，仅此
// 文案与末行 Register/Deregister 调用不同）：register 路径附「服务不可被
// 发现」说明，deregister 路径为历史短文案。对外错误全文 =
// "nns-hertz registry: " + 正文，逐字保持不变。
const (
	degradedRegisterMsg   = "registrar unavailable (degraded, not mock mode); service would be undiscoverable"
	degradedDeregisterMsg = "registrar unavailable (degraded, not mock mode)"
)

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
// ⚠️ 此处的 nil fail-fast 保留手写 `c == nil` 判定、不改收口
// [sdbridge.EnsureNotNil]（#216）：EnsureNotNil 仅检接口层面的 nil，本参数
// 是具体类型 *nnsclient.ServiceClient——类型化 nil 指针装箱进 any 后
// `dep == nil` 恒为 false（EnsureNotNil 文档明示的范围外），照搬会静默放行
// nil client、把 panic 拖到运行期空指针，恰是 #219 要消灭的形态。参数为
// 接口类型的构造器（hertz/registry.NewRegistry 的 ereg.Registrar）才走
// EnsureNotNil 单源。
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
// 单次预算（#317，kitex #212 的 hertz 孪生）收口单源
// [sdbridge.RegisterWithBudget]（#216 消费，删手写 WithTimeout 拷贝）——
// 注册中心不可达时最多阻塞缺省预算 sdbridge.DefaultRegisterBudget（3s），
// 不再无界阻塞；需要自定义预算/取消的场景用 [Registry.RegisterContext]。
func (r *Registry) Register(info *hregistry.Info) error {
	inst, err := buildInstance(info, r.defaultWeight)
	if err != nil {
		return err
	}
	return r.applyRegistrar(degradedRegisterMsg, func(reg ereg.Registrar) error {
		return sdbridge.RegisterWithBudget(context.Background(), reg, inst, sdbridge.DefaultRegisterBudget)
	})
}

// RegisterContext 与 [Registry.Register] 同语义，但显式传入 ctx
// （可取消/可超时）。
func (r *Registry) RegisterContext(ctx context.Context, info *hregistry.Info) error {
	inst, err := buildInstance(info, r.defaultWeight)
	if err != nil {
		return err
	}
	return r.applyRegistrar(degradedRegisterMsg, func(reg ereg.Registrar) error {
		return reg.Register(ctx, inst)
	})
}

// Deregister 从 NNS 注册中心注销 Hertz 服务实例。
// 与 Register 对称：保留 tags 并走 defaultWeight 兜底，
// 依赖 metadata/权重匹配的注册中心才能正确注销。
// 优雅关停路径的单次预算（#317）收口单源 [sdbridge.DeregisterWithBudget]
// （#216 消费）：注册中心不可达时最多阻塞缺省预算（3s），不再拖死关停序列。
func (r *Registry) Deregister(info *hregistry.Info) error {
	inst, err := buildInstance(info, r.defaultWeight)
	if err != nil {
		return err
	}
	return r.applyRegistrar(degradedDeregisterMsg, func(reg ereg.Registrar) error {
		return sdbridge.DeregisterWithBudget(context.Background(), reg, inst, sdbridge.DefaultRegisterBudget)
	})
}

// DeregisterContext 与 [Registry.Deregister] 同语义，但显式传入 ctx
// （可取消/可超时）。
func (r *Registry) DeregisterContext(ctx context.Context, info *hregistry.Info) error {
	inst, err := buildInstance(info, r.defaultWeight)
	if err != nil {
		return err
	}
	return r.applyRegistrar(degradedDeregisterMsg, func(reg ereg.Registrar) error {
		return reg.Deregister(ctx, inst)
	})
}

// buildInstance 把 Hertz Info 转为 ereg.Instance（#217 收敛 Register/
// DeregisterContext 的共同前段）：info==nil 拒绝；weight<=0 时由 weight
// 兜底参数（defaultWeight）兜底（委托 sdbridge.ToInstance）。
func buildInstance(info *hregistry.Info, weight int) (ereg.Instance, error) {
	if info == nil {
		return ereg.Instance{}, errors.New("nns-hertz registry: info is nil")
	}
	inst, err := sdbridge.ToInstance(info.ServiceName, info.Addr, info.Weight, weight, info.Tags)
	if err != nil {
		return ereg.Instance{}, errors.Wrapf(err, "nns-hertz registry")
	}
	return inst, nil
}

// applyRegistrar 解析当前生效的 registrar 并执行 apply（#217 收敛
// Register/DeregisterContext 的共同后段）：Registrar()==nil 时按 mock
// 口径收敛——mock 模式静默跳过（不调 apply，返回 nil），非 mock 降级
// （启动时 Nacos 不可达）静默放行会让服务"正常启动但永不可被发现"且无
// 当次调用信号——显式报错对齐 bootstrap Register 失败即回滚的行为
// （降级文案由调用方传入，register/deregister 各自保持历史全文）。
//
// 访问器用 Registrar()（nns v0.8.0 #146 双访问器收敛后 GetRegistrar 已
// Deprecated；hertz 适配层随地板抬升一并迁移）。
func (r *Registry) applyRegistrar(degradedMsg string, apply func(reg ereg.Registrar) error) error {
	reg := r.client.Registrar()
	if reg == nil {
		if r.client.MockMode() {
			return nil // mock 模式：静默跳过
		}
		return errors.Newf("nns-hertz registry: %s", degradedMsg)
	}
	return apply(reg)
}
