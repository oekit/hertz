package registry

import (
	"net"
	"strconv"

	hdiscovery "github.com/cloudwego/hertz/pkg/app/client/discovery"
	ereg "github.com/oekit/registry"
)

// InstancesToHertz 将 ekit [ereg.Instance] 列表转换为 Hertz [hdiscovery.Instance] 列表。
// 原位于 registry 核心包，因 SPI 不得依赖具体框架迁入本适配包。
func InstancesToHertz(instances []ereg.Instance) []hdiscovery.Instance {
	result := make([]hdiscovery.Instance, 0, len(instances))
	for _, inst := range instances {
		result = append(result, hdiscovery.NewInstance(
			"tcp",
			net.JoinHostPort(inst.IP, strconv.Itoa(inst.Port)),
			int(inst.Weight),
			inst.Metadata,
		))
	}
	return result
}
