// Package registry 提供 ekit registry 抽象到 CloudWeGo Hertz 框架的适配器。
//
// 本包将 ekit 的 [ereg.Registrar] 和 [ereg.Discoverer] 接口桥接到
// Hertz 的 [hregistry.Registry] 和 [discovery.Resolver]，使 Hertz Server/Client
// 可以复用 ekit 的服务注册与发现能力（当前支持 Nacos，未来可扩展 etcd/Consul 等）。
//
// # 服务端用法
//
//	namingCli, _ := nacos.NewNamingClient(cfg.Nacos)
//	reg, _ := nacosimpl.NewRegistrar(namingCli)
//	h := server.New(server.WithHostPorts(":8080"),
//	    server.WithRegistry(hertz.NewRegistry(reg), &registry.Info{
//	        ServiceName: "my-service",
//	        Addr:        utils.NewNetAddr("tcp", ":8080"),
//	    }),
//	)
//
// # 客户端用法
//
//	disc, _ := nacosimpl.NewDiscoverer(namingCli)
//	cli, _ := client.NewClient()
//	cli.Use(sd.Discovery(hertz.NewResolver(disc)))
package registry
