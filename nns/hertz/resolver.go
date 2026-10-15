package hertz

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app/client/discovery"
	registryhertz "github.com/oekit/hertz/registry/hertz"
	nnsclient "github.com/oekit/nns/nns/client"
	"github.com/oekit/registry/registry"
	"github.com/oekit/registry/registry/sdbridge"
)

const resolverName = "nns-hertz"

// resolverErrPrefix 是 Resolve 错误消息前缀。
const resolverErrPrefix = "nns-hertz resolver"

// nnsResolver 包装 nns ServiceClient，实现 Hertz client/discovery.Resolver。
//
// 与 registry/hertz.NewResolver（薄适配，直接调 discoverer.Discover）不同，
// 本 resolver 调用 ServiceClient.GetInstances，继承完整的降级链：
// 静态 → mock → 缓存 → discoverer(singleflight) → 降级缓存。
//
// 拉取 + 校验 + 错误包装的公共骨架委托给 [sdbridge.ResolverCore]，
// 本类型只负责 Hertz 服务描述解析、实例类型转换与 discovery.Result 组装。
type nnsResolver struct {
	core *sdbridge.ResolverCore
}

var _ discovery.Resolver = (*nnsResolver)(nil)

// NewResolver 创建基于 NNS ServiceClient 的 Hertz Resolver。
//
// 返回的 Resolver 可通过 [sd.Discovery] 中间件挂载到 Hertz Client：
//
//	cli.Use(sd.Discovery(nnshertz.NewResolver(nnsc.ServiceClient())))
//
// 推荐通过 NNSClient.ServiceClient() 访问底层 client：
//
//	nnshertz.NewResolver(nnsc.ServiceClient())
func NewResolver(c *nnsclient.ServiceClient) discovery.Resolver {
	return &nnsResolver{
		core: sdbridge.NewResolverCore(c.GetInstances, resolverName, resolverErrPrefix),
	}
}

// Target 返回用于缓存 key 的服务描述。
// Hertz 的 TargetInfo 包含 Host 和 Tags，这里将 Tags 编码到 key 中。
// Tags 按 key 排序以保证确定性（委托给 registry.EncodeTargetTags）。
func (r *nnsResolver) Target(_ context.Context, target *discovery.TargetInfo) string {
	if target == nil {
		return ""
	}
	return registry.EncodeTargetTags(target.Host, target.Tags)
}

// Resolve 根据服务名从 NNS 获取实例列表（含 mock/静态/缓存/降级）。
func (r *nnsResolver) Resolve(ctx context.Context, desc string) (discovery.Result, error) {
	// 从 desc 中提取服务名（去掉 ?tags 部分）
	instances, err := r.core.Fetch(ctx, sdbridge.ServiceNameFromDesc(desc))
	if err != nil {
		return discovery.Result{}, err
	}
	return discovery.Result{
		CacheKey:  desc,
		Instances: registryhertz.InstancesToHertz(instances),
	}, nil
}

// Name 返回 resolver 名称。
func (r *nnsResolver) Name() string {
	return r.core.Name()
}
