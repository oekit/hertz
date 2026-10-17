# Changelog — oekit/hertz

所有 notable 变更记录如下。格式参考 [Keep a Changelog](https://keepachangelog.com/)。

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
