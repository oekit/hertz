// Package versionh 提供版本信息的 HTTP Handler。
//
// 从 version 包拆分，避免 version 包直接依赖 Hertz。
// version 包只提供构建元数据（Metadata/Version 等），本包提供 HTTP 端点。
package versionh

import (
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/oekit/core/version"
	mw "github.com/oekit/middleware"
	"github.com/oekit/middleware/hmw"
)

// BuildVersionResp 版本信息响应结构，用于 /buildVersion 端点。
type BuildVersionResp struct {
	Version     string `json:"version"`
	BuildCommit string `json:"buildCommit"`
	BuildTime   string `json:"buildTime"`
	BuildBranch string `json:"buildBranch"`
	StartTime   string `json:"startTime"`
	Uptime      string `json:"uptime"`
}

// Handler 返回编译版本与运行时信息的 HTTP handler。
// 已包含 code/message/trace_id 外层，不会被 ResponseMiddleware 重复包装。
// 直接在路由中注册：h.GET("/buildVersion", versionh.Handler)
func Handler(ctx context.Context, c *app.RequestContext) {
	uptime := time.Since(version.StartTime).Truncate(time.Second).String()
	// trace_id 取值口径与统一信封一致（hmw.FromRequestContext 读 traceid.Key）。
	traceID := hmw.FromRequestContext(c)
	// 信封用统一的 [mw.Response]（键序 code/data/message/trace_id 固定，
	// 不再手写 map[string]any 的随机键序，调用方无需手写解码）。
	// ⚠️ 对外 JSON 契约保持不变：message 仍为 "ok"（非 mw.SuccessResp 的
	// "success"），trace_id 仍取 traceid.Key（与 hmw.FromRequestContext 同源）。
	c.JSON(consts.StatusOK, mw.Response{
		Code:    0,
		Message: "ok",
		TraceID: traceID,
		Data: BuildVersionResp{
			Version:     version.Version,
			BuildCommit: version.BuildCommit,
			BuildTime:   version.BuildTime,
			BuildBranch: version.BuildBranch,
			StartTime:   version.StartTime.Format(time.RFC3339),
			Uptime:      uptime,
		},
	})
}
