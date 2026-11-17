package registry

import (
	"net"
	"strconv"

	hdiscovery "github.com/cloudwego/hertz/pkg/app/client/discovery"
	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	ereg "github.com/oekit/registry"
)

// InstancesToHertz 将 ekit [ereg.Instance] 列表转换为 Hertz [hdiscovery.Instance] 列表。
// 原位于 registry 核心包，因 SPI 不得依赖具体框架迁入本适配包。
//
// Weight <= 0 时回填 [hregistry.DefaultWeight]（与注册侧 toInstance /
// [sdbridge.ToInstance] 的 defaultWeight 兜底同口径；kitex/kitex #92 的
// hertz 孪生 #318）：registry.Instance 的零值写法不带 Weight，透传 0 会让
// 零/负权重原样进入发现侧——hertz 自带 instance 在 Weight() 读取时兜底，
// 但该兜底是实现细节，构造期显式回填保证「转换产物权重恒为正」这一契约
// 不依赖具体 Instance 实现（kitex 侧同类透传会被 weighted_balancer 判
// "invalid weight" 视作无可用实例，fail-closed 503，教训对齐）。
func InstancesToHertz(instances []ereg.Instance) []hdiscovery.Instance {
	result := make([]hdiscovery.Instance, 0, len(instances))
	for _, inst := range instances {
		weight := int(inst.Weight)
		if weight <= 0 {
			weight = hregistry.DefaultWeight
		}
		result = append(result, hdiscovery.NewInstance(
			"tcp",
			net.JoinHostPort(inst.IP, strconv.Itoa(inst.Port)),
			weight,
			inst.Metadata,
		))
	}
	return result
}
