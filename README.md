# ekit-hertz

> Hertz 运行时集成——日志桥接、生命周期、SSE/health/metrics 子包、handler、注册发现与 NNS 适配器、OTel 桥接。

本仓库是 [ekit](https://git.enjoye.top/enjoydream/ekit)（`git.enjoye.top/enjoydream/ekit` v0.34.0）
拆分出的独立 Go module，属于 **yi-nology** ekit 子项目矩阵（Round-2 深拆粒度）。

## 安装

```bash
go get github.com/yi-nology/ekit-hertz@v0.2.0
```

## 包清单

- \`hertz/\` — 运行时集成：日志桥接（hlog→logx）、RunServer 生命周期、Shutdown Hook；子包 adaptor/health/metrics/render/sse
- \`handler/\` — crudh（CRUD HTTP handler）、validatorh（参数绑定）、versionh
- \`registry/hertz/\`、\`nns/hertz/\` — Hertz 注册发现适配器
- \`governance/hertz/\` — 动态限流/超时中间件
- \`observability/tracing/instrumentation/hertzotel|hertztracer/\` — OTel / OpenTracing 桥接

> 自研标准中间件栈与 contrib 桥接已迁 [ekit-middleware](https://github.com/yi-nology/ekit-middleware)。

## 依赖关系

子项目矩阵与依赖方向（自底向上，19 仓）：

```
ekit-core            文本/数据工具 + governance/lifecycle/version      （零内部依赖）
ekit-concurrency     并发与流控原语                                     （零内部依赖）
ekit-logx            日志门面 + shutdown                                （零内部依赖）
ekit-istio           Istio Proxyless                                    （零内部依赖）
  ekit-metrics       Prometheus 门面                          → core
  ekit-tracing       OTel + traceid                           → logx
  ekit-registry      注册发现 + 配置                          → core, logx
    ekit-data        缓存抽象 + 定时任务                      → core, logx, concurrency
      ekit-gorm      SQL/GORM 全家                            → core, logx
      ekit-redis     Redis 全家                               → data, core, logx
    ekit-httpx       HTTP 客户端 + 负载均衡                   → core, concurrency, logx, metrics, tracing, registry
      ekit-nns       服务治理客户端                           → httpx, registry, core, logx, tracing, metrics
        ekit-bootstrap 应用引导
    ekit-middleware  Hertz 中间件栈                           → core, concurrency, logx, metrics, tracing, registry
    ekit-hertz       Hertz 运行时/适配/instrumentation        → middleware, gorm, nns, registry, metrics, tracing, logx, core
    ekit-kitex       Kitex 生态                               → nns, registry, core, concurrency, logx, metrics
  ekit-workflow      流程编排                                 → concurrency, logx
```

> 注：`ekit-observability` 已在 Round-2 深拆中解散——logx 迁 [ekit-logx]、metrics 迁 [ekit-metrics]、
> tracing/traceid 迁 [ekit-tracing]、shutdown 迁 [ekit-logx]；原仓库已归档。

依赖：[ekit-middleware](https://github.com/yi-nology/ekit-middleware)、[ekit-gorm](https://github.com/yi-nology/ekit-gorm)（crudh）、[ekit-nns](https://github.com/yi-nology/ekit-nns)、[ekit-registry](https://github.com/yi-nology/ekit-registry)、[ekit-metrics](https://github.com/yi-nology/ekit-metrics)、[ekit-tracing](https://github.com/yi-nology/ekit-tracing)、[ekit-logx](https://github.com/yi-nology/ekit-logx)、[ekit-core](https://github.com/yi-nology/ekit-core)。

## 与 ekit 单体仓库的关系

- 代码与 ekit v0.34.0 等价，module 路径 `git.enjoye.top/enjoydream/ekit/...` → `github.com/yi-nology/ekit-hertz/...`
- 各子仓库独立发版（SemVer）；开发规范沿用 ekit AGENTS.md 约定

## License

见 [LICENSE](LICENSE)
