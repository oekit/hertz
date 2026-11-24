package registry

import (
	hdiscovery "github.com/cloudwego/hertz/pkg/app/client/discovery"
	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	ereg "github.com/oekit/registry"
	"github.com/oekit/registry/sdbridge"
)

// InstancesToHertz 将 ekit [ereg.Instance] 列表转换为 Hertz [hdiscovery.Instance] 列表。
// 原位于 registry 核心包，因 SPI 不得依赖具体框架迁入本适配包。
//
// 端点与权重兜底收口单源 [sdbridge]（#216 消费）：
//   - 端点经 [sdbridge.InstanceEndpoint]：network 恒为 "tcp"、addr 为 IP:Port
//     （net.JoinHostPort，IPv6 自动加方括号）——与注册侧实例端点同一口径；
//   - Weight <= 0 时经 [sdbridge.WeightOrDefault] 回填
//     [hregistry.DefaultWeight]（与注册侧 toInstance / [sdbridge.ToInstance]
//     的 defaultWeight 兜底同口径；kitex/kitex #92 的 hertz 孪生 #318）：
//     registry.Instance 的零值写法不带 Weight，透传 0 会让零/负权重原样进入
//     发现侧——hertz 自带 instance 在 Weight() 读取时兜底，但该兜底是实现细节，
//     构造期显式回填保证「转换产物权重恒为正」这一契约不依赖具体 Instance
//     实现（kitex 侧同类透传会被 weighted_balancer 判 "invalid weight" 视作
//     无可用实例，fail-closed 503，教训对齐）。
func InstancesToHertz(instances []ereg.Instance) []hdiscovery.Instance {
	result := make([]hdiscovery.Instance, 0, len(instances))
	for _, inst := range instances {
		network, addr := sdbridge.InstanceEndpoint(inst)
		// hertz 权重是 int：先按 int 截断，再以 WeightOrDefault 对 <=0 回填
		// ——若先判定后截断，(0,1) 区间的权重（registry.Instance.Weight 是
		// float64，nacos 可配小数权重）会截成 0 透传（kitex #92 修法，#335）。
		weight := int(inst.Weight)
		weight = int(sdbridge.WeightOrDefault(float64(weight), float64(hregistry.DefaultWeight)))
		result = append(result, hdiscovery.NewInstance(network, addr, weight, inst.Metadata))
	}
	return result
}
