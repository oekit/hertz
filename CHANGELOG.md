# Changelog — oekit/hertz

所有 notable 变更记录如下。格式参考 [Keep a Changelog](https://keepachangelog.com/)。

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
