// Package adaptor 提供 hertz-contrib/adaptor 常用 helper,让 ekit 业务
// 用 1 行代码把 net/http 生态(http.FileServer / embed.FS /
// gorilla/websocket)接入 hertz。
//
// 官方依据:https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/http-adaptor/
// (2025-05-21 新教程)。背景:
//   - 淘汰长期没有良好维护的 hertz app.FS
//   - 通过官方 net/http 生态提供兼容性更好、性能也不差的 fs
//   - 允许在 hertz 用 http.FileServer / embed.FS / gorilla/websocket
//
// 设计:ekit 包装 4 个最常见场景,1 行挂载。
package adaptor

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/adaptor"
)

// HertzHandler 透传 hertz-contrib adaptor.HertzHandler(http.Handler),
// 让业务不用记 import 长路径。直接传 hertz 注册用。
//
// 官方依据:https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/http-adaptor/
// "adaptor.HertzHandler 允许你将现有的 http.HandlerFunc 方法直接转为 HertzHandler"
func HertzHandler(h http.Handler) app.HandlerFunc {
	return adaptor.HertzHandler(h)
}

// HertzHandlerFunc 是 HertzHandler 的便捷写法,接 http.HandlerFunc 而不是
// http.Handler。
func HertzHandlerFunc(h http.HandlerFunc) app.HandlerFunc {
	return adaptor.HertzHandler(h)
}

// StaticFSHandler 用 embed.FS 提供静态资源服务(prefix = URL 前缀,
// "/static/*filepath")。封装 net/http.FileServer(http.FS(efs)) +
// adaptor.HertzHandler + http.StripPrefix,业务 1 行挂载:
//
//	//go:embed static/*
//	var staticFS embed.FS
//	h.GET("/static/*filepath", hertzadaptor.StaticFSHandler("/static", staticFS))
//
// 前缀解析规则(2026-09-05 复审修复:原实现 fs.Sub(efs, "/static") 被
// fs.ValidPath 拒绝前导斜杠 → 永远 404;且无 StripPrefix,子 FS 双重前缀):
//   - embed 根下存在同名目录(trim 斜杠后,如 "static"):以该目录为根;
//   - 否则以 embed 根为根(官方 http-adaptor 教程模式:FileServer 直接
//     解析完整路径,前缀目录在 embed 内部)。
//
// 两种情况都做 StripPrefix:FileServer 只看到前缀后的子路径。
//
// 官方依据:https://www.cloudwego.io/zh/docs/hertz/tutorials/basic-feature/http-adaptor/
// 例子:
//
//	staticFS := adaptor.HertzHandler(http.FileServer(http.FS(staticFiles)))
//	h.GET("/static/*filepath", staticFS)
func StaticFSHandler(prefix string, efs embed.FS) app.HandlerFunc {
	root := strings.Trim(prefix, "/")
	var core http.Handler
	if root != "" {
		// fs.Sub 不检查目录是否存在(只查 ValidPath,缺失时静默包一个空
		// subFS → 全部 404),必须显式 Stat 确认同名目录真实存在
		if st, serr := fs.Stat(efs, root); serr == nil && st.IsDir() {
			if sub, err := fs.Sub(efs, root); err == nil {
				// embed 根下存在同名目录:以该目录为根
				core = http.FileServer(http.FS(sub))
			}
		}
	}
	if core == nil {
		// 官方教程模式:embed 根即服务根,完整路径(含前缀目录)由
		// FileServer 解析
		core = http.FileServer(http.FS(efs))
	}
	if root != "" {
		core = http.StripPrefix("/"+root, core)
	}
	return adaptor.HertzHandler(core)
}

// StaticDirHandler 用本地目录提供静态资源服务。开发模式(dev)用,
// 生产推荐 StaticFSHandler(embed.FS 不依赖运行时目录)。
//
// 注意:dir 路径是相对 server 启动时的工作目录;Docker 镜像里需要
// 把目录 COPY 进容器。
func StaticDirHandler(prefix, dir string) app.HandlerFunc {
	return adaptor.HertzHandler(http.StripPrefix(prefix, http.FileServer(http.Dir(dir))))
}
