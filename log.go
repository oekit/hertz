package hertz

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/oekit/logx"
)

// hlogAdapter 将 ekit logx.Logger 适配为 Hertz hlog.FullLogger。
//
// 映射关系（对称 kitex/log.go 的 kclogAdapter）：
//   - Trace → Debug（logx 无 Trace 级别）
//   - Notice → Info（logx 无 Notice 级别）
//   - Fatal → Error（ekit 不做 os.Exit）
//   - Ctx* 方法 → logx.Ctx* 方法（自动提取 trace_id）
type hlogAdapter struct {
	logger logx.Logger
}

var _ hlog.FullLogger = (*hlogAdapter)(nil)

// registerBridgeFileOnce 把本文件注册为 logx 的内部帧（仅一次）：
// 适配器方法位于 logx 之外，若不跳过，caller 字段会归属到本文件而非
// Hertz 框架真实调用方（与 kitex/log.go 对称）。
var registerBridgeFileOnce sync.Once

// NewLogger 将 ekit [logx.Logger] 适配为 Hertz [hlog.FullLogger]。
//
// 调用后 Hertz 框架内部日志（网络层、连接异常、路由警告等）将通过 logx 输出，
// 自动携带 trace_id、service name 和结构化字段。
// 本函数会把自身所在文件注册为 logx 内部帧（logx.SkipCallerFiles），
// 使经由适配器的框架日志 caller 归属真实调用方；副作用仅此一项。
//
// 使用方式（必须传 slog 实例，不可传 hlog 后端的 Logger）：
//
//	hlog.SetLogger(hertz.NewLogger(logx.NewSlogLogger("my-http")))
//
// ⚠️ 禁止传入 hlog 后端的 Logger（如 logx.WithService(...)），否则会形成
// adapter → logx → hlog → adapter 的无限递归，导致栈溢出和进程挂死。
// 此函数会在检测到 hlog 后端时 panic 以防止生产环境不可控故障。
func NewLogger(logger logx.Logger) hlog.FullLogger {
	if logger == nil {
		panic("hertz.NewLogger: logger must not be nil")
	}
	if logx.IsHlogBacked(logger) {
		panic("hertz.NewLogger: hlog-backed Logger causes infinite recursion; use logx.NewSlogLogger(\"svc\") or logx.UseSlog(\"svc\")")
	}
	registerBridgeFileOnce.Do(func() {
		if _, file, _, ok := runtime.Caller(0); ok {
			logx.SkipCallerFiles(file)
		}
	})
	return &hlogAdapter{logger: logger}
}

// --- Logger 接口（7 级，variadic） ---

func (a *hlogAdapter) Trace(v ...interface{})  { a.logger.Debug(fmt.Sprint(v...)) }
func (a *hlogAdapter) Debug(v ...interface{})  { a.logger.Debug(fmt.Sprint(v...)) }
func (a *hlogAdapter) Info(v ...interface{})   { a.logger.Info(fmt.Sprint(v...)) }
func (a *hlogAdapter) Notice(v ...interface{}) { a.logger.Info(fmt.Sprint(v...)) }
func (a *hlogAdapter) Warn(v ...interface{})   { a.logger.Warn(fmt.Sprint(v...)) }
func (a *hlogAdapter) Error(v ...interface{})  { a.logger.Error(fmt.Sprint(v...)) }
func (a *hlogAdapter) Fatal(v ...interface{})  { a.logger.Error(fmt.Sprint(v...)) }

// --- FormatLogger 接口（7 级，printf 风格） ---

func (a *hlogAdapter) Tracef(format string, v ...interface{})  { a.logger.Debugf(format, v...) }
func (a *hlogAdapter) Debugf(format string, v ...interface{})  { a.logger.Debugf(format, v...) }
func (a *hlogAdapter) Infof(format string, v ...interface{})   { a.logger.Infof(format, v...) }
func (a *hlogAdapter) Noticef(format string, v ...interface{}) { a.logger.Infof(format, v...) }
func (a *hlogAdapter) Warnf(format string, v ...interface{})   { a.logger.Warnf(format, v...) }
func (a *hlogAdapter) Errorf(format string, v ...interface{})  { a.logger.Errorf(format, v...) }
func (a *hlogAdapter) Fatalf(format string, v ...interface{})  { a.logger.Errorf(format, v...) }

// --- CtxLogger 接口（7 级，context 感知，自动提取 trace_id） ---

func (a *hlogAdapter) CtxTracef(ctx context.Context, format string, v ...interface{}) {
	a.logger.CtxDebugf(ctx, format, v...)
}
func (a *hlogAdapter) CtxDebugf(ctx context.Context, format string, v ...interface{}) {
	a.logger.CtxDebugf(ctx, format, v...)
}
func (a *hlogAdapter) CtxInfof(ctx context.Context, format string, v ...interface{}) {
	a.logger.CtxInfof(ctx, format, v...)
}
func (a *hlogAdapter) CtxNoticef(ctx context.Context, format string, v ...interface{}) {
	a.logger.CtxInfof(ctx, format, v...)
}
func (a *hlogAdapter) CtxWarnf(ctx context.Context, format string, v ...interface{}) {
	a.logger.CtxWarnf(ctx, format, v...)
}
func (a *hlogAdapter) CtxErrorf(ctx context.Context, format string, v ...interface{}) {
	a.logger.CtxErrorf(ctx, format, v...)
}
func (a *hlogAdapter) CtxFatalf(ctx context.Context, format string, v ...interface{}) {
	a.logger.CtxErrorf(ctx, format, v...)
}

// --- Control 接口 ---

func (a *hlogAdapter) SetLevel(lv hlog.Level) {
	switch {
	case lv <= hlog.LevelDebug:
		a.logger.SetLevel(logx.LevelDebug)
	case lv <= hlog.LevelInfo:
		a.logger.SetLevel(logx.LevelInfo)
	case lv <= hlog.LevelNotice:
		// 与方法级映射一致：Notice → Info（CtxNoticef → CtxInfof）
		a.logger.SetLevel(logx.LevelInfo)
	case lv <= hlog.LevelWarn:
		a.logger.SetLevel(logx.LevelWarn)
	default:
		a.logger.SetLevel(logx.LevelError)
	}
}

// SetOutput 无操作：logx.Logger 没有 per-logger 输出 writer 注入语义，
// 输出目的地由 logx 后端（slog/hlog/std）统一管理，此参数被有意忽略。
func (a *hlogAdapter) SetOutput(_ io.Writer) {}
