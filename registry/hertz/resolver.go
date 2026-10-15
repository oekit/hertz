package hertz

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app/client/discovery"
	"github.com/oekit/registry/registry"
	"github.com/oekit/registry/registry/sdbridge"
)

const resolverName = "ekit"

// resolverErrPrefix 是 Resolve 错误消息前缀。
const resolverErrPrefix = "hertz resolver"

// hertzResolver 将 ekit [registry.Discoverer] 适配为 Hertz [discovery.Resolver]。
//
// 拉取 + 校验 + 错误包装的公共骨架委托给 [sdbridge.ResolverCore]，
// 本类型只负责 Hertz 服务描述解析、实例类型转换与 discovery.Result 组装。
type hertzResolver struct {
	core *sdbridge.ResolverCore
}

var _ discovery.Resolver = (*hertzResolver)(nil)

// ResolverOption 是 [NewResolver] 的配置选项函数类型。
type ResolverOption func(*hertzResolver)

// NewResolver 将 ekit [registry.Discoverer] 适配为 Hertz [discovery.Resolver]。
//
// 返回的 Resolver 可通过 [sd.Discovery] 中间件挂载到 Hertz Client：
//
//	cli.Use(sd.Discovery(hertz.NewResolver(disc)))
func NewResolver(disc registry.Discoverer, opts ...ResolverOption) discovery.Resolver {
	fetch := func(ctx context.Context, service string) ([]registry.Instance, error) {
		return disc.Discover(ctx, service)
	}
	r := &hertzResolver{
		core: sdbridge.NewResolverCore(fetch, resolverName, resolverErrPrefix,
			sdbridge.WithFetchErr(sdbridge.DiscoverFetchErr)),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Target 返回用于缓存 key 的服务描述。
// Hertz 的 TargetInfo 包含 Host 和 Tags，这里将 Tags 编码到 key 中。
// Tags 按 key 排序以保证确定性（委托给 registry.EncodeTargetTags）。
func (r *hertzResolver) Target(_ context.Context, target *discovery.TargetInfo) string {
	if target == nil {
		return ""
	}
	return registry.EncodeTargetTags(target.Host, target.Tags)
}

// Resolve 根据服务描述从 ekit 注册中心拉取实例列表。
func (r *hertzResolver) Resolve(ctx context.Context, desc string) (discovery.Result, error) {
	// 从 desc 中提取服务名（去掉 ?tags 部分）
	insts, err := r.core.Fetch(ctx, sdbridge.ServiceNameFromDesc(desc))
	if err != nil {
		return discovery.Result{}, err
	}
	return discovery.Result{
		CacheKey:  desc,
		Instances: InstancesToHertz(insts),
	}, nil
}

// Name 返回 resolver 名称。
func (r *hertzResolver) Name() string {
	return r.core.Name()
}
