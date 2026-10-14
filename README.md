# ekit-hertz

> Hertz HTTP 生态——自研标准中间件栈、社区库桥接、CRUD handler、注册发现/NNS 适配器、OTel 桥接。

本仓库是 [ekit](https://git.enjoye.top/enjoydream/ekit)（`git.enjoye.top/enjoydream/ekit` v0.34.0）
按框架耦合分层拆分出的独立 Go module，属于 **yi-nology** ekit 子项目矩阵。

## 安装

```bash
go get github.com/yi-nology/ekit-hertz@v0.1.0
```

## 包清单

- `middleware/` — 标准栈：Recovery/Response/AccessLog/RateLimit/BearerTokenAuth/Prometheus/I18n/Fail（`DefaultMiddleware` 一键注册）
- `middleware/contrib/` — hertz-contrib 桥接：JWT/Cache/Sessions/Swagger/CSRF/pprof/Debug/Lark/ReverseProxy/Sentinel 等
- `middleware/traceid/hmw/` — requestid 集成（hmw）
- `handler/` — crudh（CRUD HTTP handler）、validatorh（参数绑定）、versionh
- `hertz/` — 运行时集成：日志桥接、RunServer 生命周期、Shutdown Hook；子包 metrics/health/sse/render/adaptor
- `registry/hertz/`、`nns/hertz/` — Hertz 注册发现适配器
- `governance/hertz/` — 动态限流/超时中间件
- `observability/tracing/instrumentation/hertzotel|hertztracer/` — OTel / OpenTracing 桥接

## 依赖关系

子项目矩阵与依赖方向（自底向上）：

```
ekit-core                      纯工具层（零内部依赖）
  └─ ekit-observability        日志/追踪/指标门面
       └─ ekit-registry        注册发现 + 配置
            ├─ ekit-data       存储层
            ├─ ekit-network    网络客户端
            │    └─ ekit-nns   服务治理客户端
            │         └─ ekit-bootstrap  应用引导
            ├─ ekit-hertz      Hertz 生态
            ├─ ekit-kitex      Kitex 生态
            └─ ekit-workflow   流程编排
```

依赖：[ekit-core](https://github.com/yi-nology/ekit-core)、[ekit-observability](https://github.com/yi-nology/ekit-observability)、[ekit-registry](https://github.com/yi-nology/ekit-registry)、[ekit-nns](https://github.com/yi-nology/ekit-nns)、[ekit-data](https://github.com/yi-nology/ekit-data)。

## 与 ekit 单体仓库的关系

- 代码与 ekit v0.34.0 等价，仅 module 路径从 `git.enjoye.top/enjoydream/ekit/...` 改为 `github.com/yi-nology/ekit-hertz/...`
- 后续演进以各子仓库独立发版（SemVer）；ekit 单体仓库保留完整历史与架构文档（docs/PLATFORM_CONTRACT.md）
- 开发规范（命名/错误处理/并发安全/日志门面）沿用 ekit AGENTS.md 约定

## License

见 [LICENSE](LICENSE)
