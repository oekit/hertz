// Package health 提供 Hertz 健康检查端点。
//
// 所有 enjoydream 后端服务共享此模块，提供 /ping、/health、/buildVersion 三个基础端点。
//
// 基本用法:
//
//	func customizedRegister(r *server.Hertz) {
//	    health.RegisterHealthRoutes(r)
//	}
//
// 自定义配置:
//
//	health.RegisterHealthRoutes(r,
//	    health.WithPingPath("/livez"),
//	    health.WithHealthPath("/readyz"),
//	    health.WithCheckers(
//	        health.NamedRedisChecker("redis", redisClient),
//	        health.NamedDBChecker("mysql", db),
//	    ),
//	)
//
// 响应格式: 使用 ekit 统一响应格式 {code, data, message, trace_id}
//
// 端点说明:
//   - /ping: 存活探针，仅返回 200，不做依赖检查（K8s liveness probe）
//   - /health: 就绪探针，检查所有注册的 Checker（K8s readiness probe）
//   - /buildVersion: 构建版本信息
//
// 内置 Checker:
//   - RedisChecker / NamedRedisChecker: Redis PING 检查
//   - DBChecker / NamedDBChecker: MySQL PING 检查
//   - 自定义 Checker: 实现 HealthChecker 函数类型
package health

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/redis/go-redis/v9"

	"github.com/yi-nology/ekit-core/concurrency/async"
	"github.com/yi-nology/ekit-core/version"
	"github.com/yi-nology/ekit-observability/middleware/traceid"
	"github.com/yi-nology/ekit-observability/observability/logx"
)

// HealthChecker 健康检查函数类型。
// 返回 nil 表示检查通过，返回 error 表示检查失败。
type HealthChecker func(ctx context.Context) error

// CheckResult 单项检查结果
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ok" 或 "error"
	Error  string `json:"error,omitempty"`
}

// Checker 带名称的健康检查器。
type Checker struct {
	Name    string
	checker HealthChecker
}

// NamedChecker 为 Checker 添加名称，用于检查结果展示。
func NamedChecker(name string, checker HealthChecker) *Checker {
	return &Checker{Name: name, checker: checker}
}

func (n *Checker) check(ctx context.Context) error {
	return n.checker(ctx)
}

// Options 配置选项
type Options struct {
	PingPath     string
	HealthPath   string
	VersionPath  string
	Checkers     []*Checker
	CheckTimeout time.Duration
}

// HealthOption 配置函数类型
type HealthOption func(*Options)

// WithPingPath 自定义 ping 路径，默认 /ping
func WithPingPath(path string) HealthOption {
	return func(o *Options) {
		o.PingPath = path
	}
}

// WithHealthPath 自定义 health 路径，默认 /health
func WithHealthPath(path string) HealthOption {
	return func(o *Options) {
		o.HealthPath = path
	}
}

// WithVersionPath 自定义 buildVersion 路径，默认 /buildVersion
func WithVersionPath(path string) HealthOption {
	return func(o *Options) {
		o.VersionPath = path
	}
}

// WithCheckers 注入自定义健康检查函数
func WithCheckers(checkers ...*Checker) HealthOption {
	return func(o *Options) {
		o.Checkers = append(o.Checkers, checkers...)
	}
}

// WithCheckTimeout 设置就绪检查的总超时（默认 3s）。
// 超时后立即以已完成的检查结果响应，未完成的检查标记为 timeout，
// 保证 /health 响应时间有上界——即使某个 checker 不感知 context 取消。
// 传 0 或负值禁用超时（仅在确信所有 checker 都能自行返回时使用）。
func WithCheckTimeout(d time.Duration) HealthOption {
	return func(o *Options) {
		o.CheckTimeout = d
	}
}

// defaultCheckTimeout 是就绪检查的总超时默认值。
const defaultCheckTimeout = 3 * time.Second

// defaultOptions 返回默认配置
func defaultOptions() *Options {
	return &Options{
		PingPath:     "/ping",
		HealthPath:   "/health",
		VersionPath:  "/buildVersion",
		CheckTimeout: defaultCheckTimeout,
	}
}

// response 是 ekit 平台统一响应格式，字段与 middleware.Response 保持一致。
// 本包属于传输层端点，内联该结构以消除对 middleware 包的反向依赖（review L5），
// 响应体 JSON 形状不变：{"code":0,"data":...,"message":"...","trace_id":"..."}。
type response struct {
	Code    int    `json:"code"`     // 业务状态码：0=成功，非0=失败
	Data    any    `json:"data"`     // 业务数据，失败时为 null
	Message string `json:"message"`  // 描述信息
	TraceID string `json:"trace_id"` // 链路追踪 ID，贯穿请求日志，用于问题排查
}

// traceIDFrom 提取请求上下文中的 trace_id。
// 与 middleware/traceid/hmw.FromRequestContext 行为一致（trace_id 由
// traceid/hmw.Middleware 注入 RequestContext），返回空串表示未注册中间件。
func traceIDFrom(c *app.RequestContext) string {
	if v, exists := c.Get(traceid.Key); exists {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// buildVersionResp 版本信息响应结构，字段与 handler/versionh.BuildVersionResp 一致。
type buildVersionResp struct {
	Version     string `json:"version"`
	BuildCommit string `json:"buildCommit"`
	BuildTime   string `json:"buildTime"`
	BuildBranch string `json:"buildBranch"`
	StartTime   string `json:"startTime"`
	Uptime      string `json:"uptime"`
}

// PingHandler 返回存活探针 handler。
// 仅返回 200 表示进程存活，不做任何依赖检查。
// 适用于 K8s liveness probe。
func PingHandler(ctx context.Context, c *app.RequestContext) {
	c.JSON(consts.StatusOK, response{
		Code: 0, Data: nil, Message: "pong", TraceID: traceIDFrom(c),
	})
}

// indexedResult 是单个检查的索引化结果：检查并发执行，
// 通过 channel 汇聚（而非共享 slice），保证 handler 超时返回后
// 迟到的 goroutine 仍能安全退出且不产生数据竞争。
type indexedResult struct {
	idx int
	res CheckResult
}

// NewHealthHandler 创建就绪探针 handler。
// 并发执行所有注册的 Checker，结果按注册顺序稳定输出；
// 全部通过返回 200，任一失败返回 503。适用于 K8s readiness probe。
//
// 检查受 CheckTimeout 总超时约束（默认 3s，可用 WithCheckTimeout 调整）：
// 超时后立即以已完成的检查结果响应，未完成的标记为 timeout。
func NewHealthHandler(opts ...HealthOption) app.HandlerFunc {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	checkers := o.Checkers
	checkTimeout := o.CheckTimeout

	return func(ctx context.Context, c *app.RequestContext) {
		runCtx := ctx
		if checkTimeout > 0 {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithTimeout(ctx, checkTimeout)
			defer cancel()
		}

		results := make([]CheckResult, len(checkers))
		filled := make([]bool, len(checkers))
		ch := make(chan indexedResult, len(checkers)) // 容量=len：迟到发送永不阻塞

		var wg sync.WaitGroup
		for i, checker := range checkers {
			wg.Add(1)
			async.GoSafe(func() {
				defer wg.Done()
				res := CheckResult{Name: checker.Name, Status: "ok"}
				// checker panic 就地收编为 error 结果：checkTimeout<=0 路径
				// 依赖"每个 checker 恰好投递一次"，panic 跳过投递会让
				// 下面的 <-ch 永久阻塞（/health 挂死）。
				err := func() (err error) {
					defer func() {
						if r := recover(); r != nil {
							err = fmt.Errorf("checker panic: %v", r)
						}
					}()
					return checker.check(runCtx)
				}()
				if err != nil {
					logx.CtxError(runCtx, "health check failed", "name", checker.Name, "err", err)
					res.Status = "error"
					res.Error = "check failed"
					if runCtx.Err() != nil {
						res.Error = "timeout"
					}
				}
				ch <- indexedResult{idx: i, res: res}
			})
		}

		// 收集结果。禁用超时（checkTimeout<=0）时等全部检查退出后再读取；
		// 启用超时则以硬截止收割：即使 checker 不感知 context 取消，
		// /health 的响应时间也有上界。
		var timer *time.Timer
		if checkTimeout <= 0 {
			wg.Wait()
		} else {
			timer = time.NewTimer(checkTimeout + 100*time.Millisecond)
			defer timer.Stop()
		}

		allOK := true
		for remaining := len(checkers); remaining > 0; {
			if checkTimeout <= 0 {
				r := <-ch
				results[r.idx], filled[r.idx] = r.res, true
				remaining--
				continue
			}
			select {
			case r := <-ch:
				results[r.idx], filled[r.idx] = r.res, true
				remaining--
			case <-timer.C:
				for i := range results {
					if !filled[i] {
						results[i] = CheckResult{Name: checkers[i].Name, Status: "error", Error: "timeout"}
					}
				}
				remaining = 0
			}
		}
		for i := range results {
			if results[i].Status != "ok" {
				allOK = false
				break
			}
		}

		data := map[string]any{
			"status": "ok",
			"checks": results,
		}

		if allOK {
			c.JSON(consts.StatusOK, response{
				Code: 0, Data: data, Message: "success", TraceID: traceIDFrom(c),
			})
		} else {
			data["status"] = "error"
			c.JSON(consts.StatusServiceUnavailable, response{
				Code: 503, Data: data, Message: "service unhealthy", TraceID: traceIDFrom(c),
			})
			c.Abort()
		}
	}
}

// buildVersionHandler 返回编译版本与运行时信息的 HTTP handler。
// 内联自 handler/versionh.Handler，路由路径与响应结构保持一致
// （响应已含 code/message/trace_id 外层，不会被 ResponseMiddleware 重复包装）。
func buildVersionHandler(ctx context.Context, c *app.RequestContext) {
	uptime := time.Since(version.StartTime).Truncate(time.Second).String()
	c.JSON(consts.StatusOK, map[string]any{
		"code":     0,
		"message":  "ok",
		"trace_id": traceIDFrom(c),
		"data": buildVersionResp{
			Version:     version.Version,
			BuildCommit: version.BuildCommit,
			BuildTime:   version.BuildTime,
			BuildBranch: version.BuildBranch,
			StartTime:   version.StartTime.Format(time.RFC3339),
			Uptime:      uptime,
		},
	})
}

// RegisterHealthRoutes 注册健康检查路由：/ping、/health、/buildVersion。
func RegisterHealthRoutes(r *server.Hertz, opts ...HealthOption) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}

	r.GET(o.PingPath, PingHandler)
	r.GET(o.HealthPath, NewHealthHandler(opts...))
	r.GET(o.VersionPath, buildVersionHandler)
}

// ==================== 内置 Checker ====================

// NamedRedisChecker 创建带名称的 Redis 健康检查函数。
func NamedRedisChecker(name string, client redis.Cmdable) *Checker {
	return NamedChecker(name, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()
		return client.Ping(ctx).Err()
	})
}

// RedisChecker 创建 Redis 健康检查函数（默认名称 "redis"）。
func RedisChecker(client redis.Cmdable) *Checker {
	return NamedRedisChecker("redis", client)
}

// NamedDBChecker 创建带名称的 MySQL 健康检查函数。
func NamedDBChecker(name string, db *sql.DB) *Checker {
	return NamedChecker(name, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()
		return db.PingContext(ctx)
	})
}

// DBChecker 创建 MySQL 健康检查函数（默认名称 "mysql"）。
func DBChecker(db *sql.DB) *Checker {
	return NamedDBChecker("mysql", db)
}
