// Package versionh 提供版本信息的 HTTP Handler。
//
// 从 version 包拆分，避免 version 包直接依赖 Hertz。
// version 包只提供构建元数据（Metadata/Version 等），本包提供 HTTP 端点。
package versionh

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/yi-nology/ekit-core/version"
	"github.com/yi-nology/ekit-tracing/traceid"
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
	traceID := ""
	if v, exists := c.Get(traceid.Key); exists {
		traceID = fmt.Sprintf("%v", v)
	}
	c.JSON(consts.StatusOK, map[string]any{
		"code":     0,
		"message":  "ok",
		"trace_id": traceID,
		"data": BuildVersionResp{
			Version:     version.Version,
			BuildCommit: version.BuildCommit,
			BuildTime:   version.BuildTime,
			BuildBranch: version.BuildBranch,
			StartTime:   version.StartTime.Format(time.RFC3339),
			Uptime:      uptime,
		},
	})
}
