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
//	        health.NamedDBChecker("mysql", db),
//	        redischeck.NamedRedisChecker("redis", redisClient), // Redis 检查器在 health/redischeck
//	    ),
//	)
//
// 响应格式: 使用 ekit 统一响应格式 {code, data, message, trace_id}
//
// /health 的响应体可直接用导出的 health.HealthResponse 解码：
// data.status 是整体状态（health.StatusOK / StatusDegraded / StatusError），
// data.checks 是逐项结果数组（name/status/error）。
// 消费方不必手写信封结构体去猜内层形态——见 HealthResponse 的 godoc，
// 其中列出了消费方极易猜错的 4 点。
//
// 端点说明:
//   - /ping: 存活探针，仅返回 200，不做依赖检查（K8s liveness probe）
//   - /health: 就绪探针，检查所有注册的 Checker（K8s readiness probe）
//   - /buildVersion: 构建版本信息（经 versionh.Handler 注册）
//
// 任一路径传空字符串即跳过该路由注册（如只要 /ping 不要 /buildVersion：
// WithVersionPath("")）。
//
// 内置 Checker:
//   - DBChecker / NamedDBChecker: MySQL PING 检查（stdlib database/sql，本包）
//   - RedisChecker / NamedRedisChecker: Redis PING 检查
//     （在子包 github.com/oekit/hertz/health/redischeck，避免核心包拖入 go-redis）
//   - 自定义 Checker: 实现 HealthChecker 函数类型
//
// 软依赖降级: Checker 可返回 health.Degraded(err) 标记降级——
// CheckResult.Status = health.StatusDegraded、Error 填充，但 HTTP 仍 200（就绪不失败），
// data.status 可为 StatusDegraded 供告警识别，软依赖不可用不会误杀就绪。
package health

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/hertz/versionh"
	"github.com/oekit/logx"
	"github.com/oekit/tracing/traceid"
)

// HealthChecker 健康检查函数类型。
// 返回 nil 表示检查通过；返回 error 表示检查失败；
// 返回 Degraded(err) 表示软依赖降级（HTTP 200，Status = StatusDegraded）。
type HealthChecker func(ctx context.Context) error

// CheckResult 单项检查结果。
//
// 是 HealthData.Checks 的元素类型，name/status/error 三字段与实际 JSON 逐字段对应：
//
//	{"name":"db","status":"ok"}
//	{"name":"mcp-tools","status":"degraded","error":"check degraded"}
//
// 注意单项详情在 error 字段，不在 message（message 是顶层信封消息）。
type CheckResult struct {
	Name   string `json:"name"`
	Status Status `json:"status"` // StatusOK / StatusDegraded / StatusError
	Error  string `json:"error,omitempty"`
}

// DegradedError 表示软依赖降级：不影响就绪（HTTP 200），但
// CheckResult.Status = StatusDegraded、Error 填充，供告警系统识别。
// 与硬失败（Status = StatusError → HTTP 503）区分：软依赖（缓存、旁路服务等）
// 不可用应保持可见，但不应让就绪探针失败导致实例被摘流。
type DegradedError struct {
	// Err 是导致降级的底层错误，经 Unwrap 暴露，可被 errors.Is/As 链识别。
	Err error
}

// Error 实现 error 接口。
func (e *DegradedError) Error() string {
	if e.Err == nil {
		return "degraded"
	}
	return e.Err.Error()
}

// Unwrap 返回底层错误，支持 errors.Is/As 链式判定。
func (e *DegradedError) Unwrap() error {
	return e.Err
}

// Degraded 将错误标记为软依赖降级。
// 在 Checker 中返回 health.Degraded(err)，/health 仍 200，
// 该项 CheckResult.Status = StatusDegraded（data.status = StatusDegraded 供告警）。
//
// 用法:
//
//	health.WithCheckers(health.NamedChecker("cache", func(ctx context.Context) error {
//	    if err := cache.Ping(ctx); err != nil {
//	        return health.Degraded(err) // 软依赖：降级但不摘流
//	    }
//	    return nil
//	}))
func Degraded(err error) error {
	return &DegradedError{Err: err}
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
	PingPath      string
	HealthPath    string
	VersionPath   string
	Checkers      []*Checker
	CheckTimeout  time.Duration
	VerboseErrors bool
}

// HealthOption 配置函数类型
type HealthOption func(*Options)

// WithPingPath 自定义 ping 路径，默认 /ping。
// 传空字符串跳过该路由注册。
func WithPingPath(path string) HealthOption {
	return func(o *Options) {
		o.PingPath = path
	}
}

// WithHealthPath 自定义 health 路径，默认 /health。
// 传空字符串跳过该路由注册。
func WithHealthPath(path string) HealthOption {
	return func(o *Options) {
		o.HealthPath = path
	}
}

// WithVersionPath 自定义 buildVersion 路径，默认 /buildVersion。
// 传空字符串跳过该路由注册。
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

// WithVerboseErrors 开启错误详情暴露（默认关闭）。
//
// 默认（关闭）时 CheckResult.Error 脱敏为 "check failed" / "check degraded"，
// 避免内部连接串、凭据片段等经 /health 暴露给探针调用方。
// 开启后 CheckResult.Error 直接暴露 err.Error()，适合内网调试/开发环境。
// timeout 标记不受影响（本身就是脱敏文案）。
func WithVerboseErrors() HealthOption {
	return func(o *Options) {
		o.VerboseErrors = true
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
//
// 仅 /ping 使用（data 为 null）。/health 使用导出的 HealthResponse，
// 其 data 是强类型的 HealthData（status + checks）——消费方可直接解码，
// 不必手写信封结构体，见 response.go。
type response struct {
	Code    int    `json:"code"`     // 业务状态码：0=成功，非0=失败
	Data    any    `json:"data"`     // 业务数据，失败时为 null
	Message string `json:"message"`  // 描述信息
	TraceID string `json:"trace_id"` // 链路追踪 ID，贯穿请求日志，用于问题排查
}

// traceIDFrom 提取请求上下文中的 trace_id。
// 与 hmw.FromRequestContext 行为一致（trace_id 由
// hmw.Middleware 注入 RequestContext），返回空串表示未注册中间件。
func traceIDFrom(c *app.RequestContext) string {
	if v, exists := c.Get(traceid.Key); exists {
		return fmt.Sprintf("%v", v)
	}
	return ""
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

// checkResultStatus 判定单项检查结果状态与文案。
// 返回 (status, errorText)。errorText 按 verbose 决定脱敏或透出。
// timedOut 仅对硬失败生效：软依赖降级（DegradedError）语义优先，不改判为 timeout。
func checkResultStatus(err error, timedOut, verbose bool) (status Status, errText string) {
	if err == nil {
		return StatusOK, ""
	}
	var deg *DegradedError
	if errors.As(err, &deg) {
		if verbose {
			return StatusDegraded, deg.Error()
		}
		return StatusDegraded, "check degraded"
	}
	if timedOut {
		return StatusError, "timeout"
	}
	if verbose {
		return StatusError, err.Error()
	}
	return StatusError, "check failed"
}

// NewHealthHandler 创建就绪探针 handler。
// 并发执行所有注册的 Checker，结果按注册顺序稳定输出；
// 全部通过返回 200，任一硬失败返回 503，仅降级（无硬失败）返回 200
// 且 data.status = StatusDegraded。适用于 K8s readiness probe。
//
// 检查受 CheckTimeout 总超时约束（默认 3s，可用 WithCheckTimeout 调整）：
// 超时后立即以已完成的检查结果响应，未完成的标记为 timeout。
//
// 错误文案默认脱敏为 "check failed" / "check degraded"，
// WithVerboseErrors() 开启后暴露 err.Error()。
func NewHealthHandler(opts ...HealthOption) app.HandlerFunc {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	checkers := o.Checkers
	checkTimeout := o.CheckTimeout
	verbose := o.VerboseErrors

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
				res := CheckResult{Name: checker.Name}
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
				status, errText := checkResultStatus(err, runCtx.Err() != nil, verbose)
				res.Status, res.Error = status, errText
				if err != nil && status == StatusError {
					logx.CtxError(runCtx, "health check failed", "name", checker.Name, "err", err)
				} else if err != nil {
					logx.CtxWarn(runCtx, "health check degraded", "name", checker.Name, "err", err)
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
						results[i] = CheckResult{Name: checkers[i].Name, Status: StatusError, Error: "timeout"}
					}
				}
				remaining = 0
			}
		}
		// 汇总：任一 error → 失败（503）；仅 degraded → 200 且 status=degraded；
		// 全 ok → 200。degraded 是软依赖降级，就绪不失败，但保持可见供告警。
		status := StatusOK
		for i := range results {
			switch results[i].Status {
			case StatusError:
				status = StatusError
			case StatusDegraded:
				if status == StatusOK {
					status = StatusDegraded
				}
			}
		}

		// 直接用导出的 HealthResponse 写响应：导出面即响应面，
		// 消费方解码 health.HealthResponse 拿到的是同一份结构（见 response.go）。
		data := HealthData{
			Checks: results,
			Status: status,
		}

		if status != StatusError {
			// ok 与 degraded 都保持就绪（HTTP 200）；degraded 时 data.status
			// 供告警系统识别软依赖问题，不触发摘流。
			msg := "success"
			if status == StatusDegraded {
				msg = "service degraded"
			}
			c.JSON(consts.StatusOK, HealthResponse{
				Code: 0, Data: data, Message: msg, TraceID: traceIDFrom(c),
			})
		} else {
			c.JSON(consts.StatusServiceUnavailable, HealthResponse{
				Code: 503, Data: data, Message: "service unhealthy", TraceID: traceIDFrom(c),
			})
			c.Abort()
		}
	}
}

// RegisterHealthRoutes 注册健康检查路由：/ping、/health、/buildVersion。
//
// 任一路径传空字符串即跳过对应路由注册（三个端点相互独立）：
//
//	// 只注册 /ping，不要 /health 和 /buildVersion
//	health.RegisterHealthRoutes(r, health.WithHealthPath(""), health.WithVersionPath(""))
//
// /buildVersion 经 versionh.Handler 直接注册（版本信息端点的唯一实现）。
func RegisterHealthRoutes(r *server.Hertz, opts ...HealthOption) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}

	if o.PingPath != "" {
		r.GET(o.PingPath, PingHandler)
	}
	if o.HealthPath != "" {
		r.GET(o.HealthPath, NewHealthHandler(opts...))
	}
	if o.VersionPath != "" {
		r.GET(o.VersionPath, versionh.Handler)
	}
}

// ==================== 内置 Checker ====================

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
