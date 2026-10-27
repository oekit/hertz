# Changelog — oekit/hertz

所有 notable 变更记录如下。格式参考 [Keep a Changelog](https://keepachangelog.com/)。

## [v0.8.6] — 2026-10-28

- build(deps): 依赖地板对齐 gormx v0.6.0 → v0.6.2，crudh 消费方拿不到 gormx v0.6.2 的 IsUniqueViolation sqlite 分支

## [v0.8.5] — 2026-10-25

- feat(clientip): 跟随 pkg v0.26.0——**默认不采信 X-Real-IP**（BREAKING）：X-Real-IP 是单跳头，HTTP 头无法区分「代理写的」与「客户端自带的」，网关默认不写该头但原样透传客户端自带的 ⇒ 客户端可任意改写自己的客户端 IP（限流/锁定计数桶被绕过、审计与访问日志来源 IP 可改写，oekit/pkg#106）；GetClientIP / GetClientIPWithDefaultProxies 不再采信该头，确需采信改用新增的 GetClientIPWithXRI / GetClientIPWithDefaultProxiesAndXRI（前置条件：可信代理必须覆写该头）；新增 GetClientIPWithSet 走预解析可信代理集合，请求期不再逐条 ParseCIDR 整份清单（oekit/pkg#107 热路径形态）；基座地板 pkg v0.25.2 → v0.26.0

## [v0.8.4] — 2026-10-25

- feat(clientip): Hertz 侧客户端 IP 解析适配——Hertz 自带的 c.ClientIP() 默认信任任意来源的 X-Forwarded-For，直连方可随意伪造，于是限流/审计/访问控制三处的 IP 口径只能各写一遍；新增 clientip 包，把采信判定交给 pkg/convert.ResolveClientIP（与 net/http 侧同一份实现），Hertz 侧只负责取对端地址与两个代理头；GetClientIP(c, trustedProxies) 按可信代理列表解析、GetClientIPWithDefaultProxies(c) 用 pkg/convert 的默认可信网段；基座依赖地板跟到 pkg v0.25.2（#95）

## [v0.8.3] — 2026-10-24

- fix(bootstrap): RunServer 就绪探活被正常关停打断时误报 ERROR「ready probe failed」——关停与探活失败两种语义未区分；探活侧改为返回可判别哨兵 errProbeAborted，调用侧 errors.Is 命中后只记一条 INFO、不再请求关机（进程本就在关停，再请求是多余动作）；并让关停优先于超时判定——探活超时仍按启动失败处理，既有语义不变（#48）

## [v0.8.2] — 2026-10-24

- fix(bootstrap): RunServer 就绪探活与 h.Run() 并发——此前串行先探活必然 connection refused、探活必超时，h.Run() 根本不被调用、server 从不监听；探活改为并发执行，且只在 engine running 后发起、探活通过后复核 engine 仍在运行（端口被他人占用时不得回调 onReady，避免把不属于自己的实例注册进注册中心）（#43）

## [v0.8.1] — 2026-10-22

- build(deps): logx/hlog 地板 v0.1.1 → v0.1.4（父模块地板与 logx v0.7.1 对齐；零代码改动）

## [v0.8.0] — 2026-10-22

- fix(health): `/health` 响应形态提供导出面——消费方不必手写信封结构体（[#26](https://github.com/oekit/hertz/issues/26)）
  - 新增 `health.HealthResponse`（信封 `{code,data,message,trace_id}`，`data` 强类型）+ `health.HealthData`（`{checks,status}`），消费方可直接 `json.Unmarshal` 到导出类型
  - 新增 `health.Status` 类型与 `StatusOK` / `StatusDegraded` / `StatusError` 常量——比较状态不再硬编码字面量
  - **BREAKING**: `CheckResult.Status` 由 `string` 改为 `health.Status`（底层仍是 `string`）。与字面量比较的调用点不受影响；把 `CheckResult.Status` 赋给 `string` 变量的调用点需显式 `string(...)` 转换
  - 响应体字节不变：`data` 由 `map[string]any` 改为强类型结构，键序与取值与既有响应完全一致
- build: 测试改用嵌套模块 registry/nacos——registry v0.5.0 起不再提供该子包
  - 依赖地板：registry v0.4.1 → **v0.5.0**（新增 require `github.com/oekit/registry/nacos v0.1.0`，中接 `registry/nacosimpl v0.1.0`）；logx v0.7.0 → v0.7.1
  - registry v0.5.0 把 Nacos 客户端门面拆成独立发版的嵌套模块，import 路径不变但「由哪个 module 提供」变了——不补 require 则 `go mod tidy` 认不出提供者

## [v0.7.1] — 2026-10-19

- build: 矩阵内依赖地板对齐（align_floors 不动点）：bootstrap v0.6.1 → v0.6.2、concurrency v0.5.4 → v0.17.0、core v0.7.0 → v0.9.2、kitx v0.1.1 → v0.2.0、logx v0.6.0 → v0.7.0、middleware v0.7.2 → v0.12.0、nns v0.4.1 → v0.4.2、pkg v0.11.0 → v0.15.1

## [v0.7.0] — 2026-10-19

**BREAKING**: crudh 主键类型泛型化（跟随 gormx v0.6.0 `[T, ID comparable]`，gormx#7 问题 1）

- `Handler[T, C, U]` → `Handler[T, ID comparable, C, U]`；`HandlerOption`/`WithSortableColumns`/
  `NewHandler` 签名跟随（`crud.Service[T, ID, C, U]`）
- 新增 `WithIDParser(fn func(string) (ID, error))`：路径参数 → ID 解析注入
- 缺省解析（不注入时）：int 系主键 `strconv.ParseInt`（宽度随 ID 类型，int64 与 v0.6 之前
  行为一致）、string 主键直通；其余 comparable 类型必须显式注入，缺省解析失败按 400 拒绝
  （fail-closed）
- 存量 int64 调用点机械迁移：`NewHandler[Item, C, U]` → `NewHandler[Item, int64, C, U]`；
  文本主键项目传恒等解析 `WithIDParser[Doc, string, C, U](func(s string) (string, error) { return s, nil })`
- gormx 地板 v0.5.5 → **v0.6.0**
- 新增文本主键端到端测试（hex 串路径参数经 WithIDParser 直通 + Get/Delete/404 链）与
  `defaultParseID` 单测（int64/string 双形态）

## [v0.6.4] — 2026-10-18

- docs(log): NewLogger 注释改按判据描述 hlog 递归风险（IsHlogBacked 实测值 + 常见触发路径：先 hlog.SetLogger 改全局背端再取 Default/WithService），panic message 同步带上实测判据（[#10](https://github.com/oekit/hertz/issues/10)）

## [v0.6.3] — 2026-10-18

- fix(deps): middleware v0.7.1 → v0.7.2 地板对齐（零代码改动）

## [v0.6.2] — 2026-10-18

- fix(deps): gormx v0.5.4 → v0.5.5、metrics v0.1.6 → v0.1.7、tracing v0.2.1 → v0.2.2 地板对齐（零代码改动）

## [v0.6.1] — 2026-10-18

- fix(deps): 全部 oekit 依赖地板对齐最新（bootstrap v0.5.3 → v0.6.1、logx v0.5.0 → v0.6.0、middleware v0.7.0 → v0.7.1、nns v0.4.0 → v0.4.1、registry v0.4.0 → v0.4.1，零代码改动）

## [v0.6.0] — 2026-10-17

### Added（go-kratos 对齐）
- `transportx` 新子包：Hertz RequestContext → core/transport 适配
  （`NewServerTransport`/`NewServerContext`；Kind=http、Endpoint=Host、Operation=PATH，
  header 读写经协议无关 `transport.Header`）
- `chainmw` 新子包：core/chain 中间件栈接入 Hertz——`Wrap`（自动注入 server Transporter，
  错误出口默认接 `mw.Fail` 统一响应格式，可 `WithOnError` 接管）、`FromHertz` 混编桥
- `RunServer` 变参扩展：`WithOnReady`（TCP 探活通过后回调，典型用途 `app.Ready()`
  触发 bootstrap/nacos v0.3.0 延迟注册）、`WithReadyProbeTimeout`（默认 5s；探活失败
  按启动失败触发关机）。hertz OnRun 钩子先于 listenAndServe 执行，无原生就绪信号，
  探活是对真实就绪的近似；未设置 OnReady 时行为与 v0.5.x 一致

### Added（adaptor）
- **feat(adaptor)**：`MockConn` + `PerformRequest` 测试 helper——补上
  `rc.GetWriter()` 所需的 network.Conn，4 个导出 handler 可用 ut 风格单测
  （原先 `ut.PerformRequest` 直接 nil deref）。响应体在 `MockConn.Out()`，
  不在 `ctx.Response.Body()`（HijackWriter 直写 conn，godoc 已注明）
  （[#9](https://github.com/oekit/hertz/issues/9)）
- **feat(adaptor)**：`NativeStaticFSHandler`——真正零 adaptor 转换的原生静态
  handler（fs.ReadFile + c.Write）。Content-Type 走 mime 表、HEAD、404+nosniff、
  路径穿越拒绝；**不写死 Cache-Control**（`WithNativeCacheControl` 可选）。
  `StaticFSHandler` godoc 点明内部仍走 adaptor
  （[#8](https://github.com/oekit/hertz/issues/8)）

### Changed
- 依赖升级：registry v0.4.0 / nns v0.4.0 / core v0.7.0 / middleware v0.7.0
  （registry/nns 包提升至模块根，导入路径变更）

## [v0.5.10] — 2026-10-17

- bootstrap 地板 v0.5.2 → v0.5.3；nns 地板 v0.3.4 → v0.3.5

## [v0.5.9] — 2026-10-17

- kitx 地板 v0.1.0 → v0.1.1

## [v0.5.8] — 2026-10-17

- pkg 地板 v0.11.0；concurrency v0.5.4、gormx v0.5.4、middleware v0.6.2 地板跟随

## [v0.5.7] — 2026-10-17

- fix(deps): 地板跟随——middleware v0.5.1 → v0.6.1、gormx v0.5.2 → v0.5.3、concurrency v0.5.2 → v0.5.3（pkg/kitx 已在 v0.10.0/v0.1.0）

## [v0.5.6] — 2026-10-17

- refactor(validatorh): 底层校验迁移至 [kitx](https://github.com/oekit/kitx)/validator（pkg v0.10.0 拆分承接）——import 机械替换，Bind*/Fail 400 错误映射行为不变；deps: pkg v0.9.3 → v0.10.0、新增 kitx v0.1.0

## [v0.5.5] — 2026-10-17

- fix(lifecycle): bootstrap_test 迁移 logx/shutdown → core/lifecycle（SA1019）；fix(deps): bootstrap 地板 v0.5.1 → v0.5.2

## [v0.5.4] — 2026-10-17

- **fix(lint)**：`health_test.go` 循环内 `resp` 改复用外层声明（govet shadow）——
  v0.5.2 tag 曾因 CI 存量红被重指向 shadow 修复提交，导致 tag 树与
  sum.golang.org 首次记录的校验和不一致（默认 GOSUMDB 用户拉取报
  checksum mismatch，[#7](https://github.com/oekit/hertz/issues/7)）。
  **处置**：v0.5.2 tag 已恢复至与 sumdb 记录一致的原提交（默认 GOSUMDB 可正常拉取）；
  shadow 修复自 v0.5.4 起可用（v0.5.3 曾按本 issue 请求发布，后随事故版本批次撤删，
  勿引用），受影响用户请使用 v0.5.4 或更高版本

## [v0.5.2] — 2026-10-17

### Fixed

- bootstrap 结构重置对齐（v0.5.0 起包提升至模块根）：`bootstrap_test.go` 的 import 由旧路径 `github.com/oekit/bootstrap/bootstrap` 改为模块根
- 依赖地板升重置新基线：bootstrap v0.4.1 → v0.5.0、gormx v0.3.0 → v0.5.1（旧版本 tag 已随结构重置清空，原地板会导致 `go mod tidy` 与直连拉取失败；测试外代码不涉旧路径，build/vet 不受影响）

## [v0.5.1] — 2026-10-17

仓库结构重置后的首个版本（历史与旧版本号 v0.1.0–v0.5.0 已清空重做，从 v0.5.1 起算）。

### 目录结构

```
github.com/oekit/hertz                  # package hertz — RunServer / hlog 桥接 / Hook（模块根）
├── adaptor/                            # package adaptor — net/http 互转
├── health/                             # package health — K8s 探针 + Degraded 降级
│   └── redischeck/                     # package redischeck — go-redis PING 检查器
├── metrics/                            # package metrics — Prometheus /metrics
├── render/                             # package render — 业务 Render 实现
├── sse/                                # package sse — Server-Sent Events
├── crudh/                              # package crudh — 泛型 CRUD Handler
├── validatorh/                         # package validatorh — 参数绑定校验
├── versionh/                           # package versionh — 版本端点
├── governance/                         # package govhertz — 动态限流/超时中间件
├── nns/                                # package nns — NNS → Hertz 注册发现适配
├── registry/                           # package registry — ekit registry → Hertz 适配
├── hertzotel/                          # package hertzotel — OpenTelemetry 链路
└── hertztracer/                        # package hertztracer — OpenTracing 链路
```

- **核心包 `hertz` 提升到模块根**：import 从 `github.com/oekit/hertz/hertz`
  改为 `github.com/oekit/hertz`（包名仍为 `hertz`），消除双层目录
- **子包收成一级**：`hertz/health` → `health`、`hertz/sse` → `sse`、
  `hertz/metrics` → `metrics`、`hertz/render` → `render`、`hertz/adaptor` → `adaptor`
- **适配器扁平化**：
  `governance/hertz` → `governance`（包名 govhertz 不变，避免与 core/governance 冲突）、
  `registry/hertz` → `registry`（包名 hertz → registry）、
  `nns/hertz` → `nns`（包名 hertz → nns）
- **可观测路径收拢**：`observability/tracing/instrumentation/hertzotel` → `hertzotel`、
  `observability/tracing/instrumentation/hertztracer` → `hertztracer`
- **middleware 依赖对齐 v0.5.0**：`mw` import 路径
  `github.com/oekit/middleware/middleware` → `github.com/oekit/middleware`

### 能力面（自 ekit 拆分保留）

- `RunServer` — 接 bootstrap 生命周期，失败自动 RequestShutdown，容忍双路 Shutdown 竞态
- `NewLogger` / `hlog` 桥接 — Hertz 框架日志归一到 logx，caller 归属已修正
- `AddShutdownHook` — 关停前刷数据钩子
- `health.RegisterHealthRoutes` — /ping、/health、/buildVersion；`Degraded(err)` 软依赖降级；
  `WithVerboseErrors()` 可配置脱敏；空路径跳过注册
- `sse` — 官方 protocol/sse 并发安全封装 + LastEventID 续传
- `metrics.InitAndRegister` — Prometheus /metrics（gzip/OpenMetrics 协商透传）
- `crudh` — 泛型 CRUD Handler，直接消费 gormx `crud.Service`
- `validatorh` — Hertz 参数绑定+校验，ValidationError 自动 400
- `versionh` — 版本信息端点
- `govhertz.NewLimiterMiddleware` / `NewTimeoutMiddleware` — 治理动态限流/超时
- `registry.NewRegistry` / `NewResolver` / `InstancesToHertz` — ekit registry 适配
- `nns.NewRegistry` / `NewResolver` / `NewInstanceWatcher` — NNS 适配（含降级链）
- `hertzotel` / `hertztracer` — server/client 全量 tracing 集成

### 迁移（从旧路径）

| 旧 | 新 |
|---|---|
| `github.com/oekit/hertz/hertz` | `github.com/oekit/hertz` |
| `github.com/oekit/hertz/hertz/{health,metrics,sse,render,adaptor}` | `github.com/oekit/hertz/{health,metrics,sse,render,adaptor}` |
| `github.com/oekit/hertz/hertz/health/redischeck` | `github.com/oekit/hertz/health/redischeck` |
| `github.com/oekit/hertz/governance/hertz` | `github.com/oekit/hertz/governance` |
| `github.com/oekit/hertz/registry/hertz` | `github.com/oekit/hertz/registry` |
| `github.com/oekit/hertz/nns/hertz` | `github.com/oekit/hertz/nns` |
| `github.com/oekit/hertz/observability/tracing/instrumentation/hertzotel` | `github.com/oekit/hertz/hertzotel` |
| `github.com/oekit/hertz/observability/tracing/instrumentation/hertztracer` | `github.com/oekit/hertz/hertztracer` |
| `github.com/oekit/middleware/middleware`（本仓 crudh 依赖） | `github.com/oekit/middleware` |

包名变化：`registry/hertz` 与 `nns/hertz` 的包名由 `hertz` 分别改为 `registry`、`nns`。
