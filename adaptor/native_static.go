package adaptor

import (
	"context"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

// NativeStaticFSHandler 返回真正「零 adaptor 转换」的 Hertz 原生静态 handler。
//
// 与 StaticFSHandler 的区别：后者内部仍是 adaptor.HertzHandler(http.FileServer)，
// 每次请求都要过 net/http Request⇄RequestContext 适配；本 handler 直接
// fs.ReadFile + c.SetStatusCode/c.Write，热路径上无转换、分配更少。
//
// 语义对齐 net/http.FileServer 的常见子集：
//   - Content-Type 走 mime.TypeByExtension（与 net/http 同表）
//   - 支持 HEAD（只回头不回体）
//   - 文件不存在/路径非法 → 404 + X-Content-Type-Options: nosniff
//   - 路径穿越：拒绝含 ".." / 非法字符的路径（fs.ValidPath）
//
// 缓存策略**不写死**：默认不发 Cache-Control，由调用方用 WithNativeCacheControl
// 注入，或在 Handler 外自行加头。
//
// 示例：
//
//	//go:embed static/*
//	var assets embed.FS
//	sub, _ := fs.Sub(assets, "static")
//	h.GET("/static/*filepath", adaptor.NativeStaticFSHandler("/static", sub,
//	    adaptor.WithNativeCacheControl("no-cache")))
//
// prefix 与路由参数 filepath 拼接后在 fsys 内查找（"/static" + "/css/a.css"
// → 查 "css/a.css"）。若 fsys 根下仍带同名目录，可继续传带目录的 FS。
func NativeStaticFSHandler(prefix string, fsys fs.FS, opts ...NativeStaticOption) app.HandlerFunc {
	cfg := nativeStaticConfig{prefix: strings.Trim(prefix, "/")}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return func(ctx context.Context, c *app.RequestContext) {
		name := strings.TrimPrefix(string(c.Path()), "/"+cfg.prefix)
		name = strings.TrimPrefix(name, "/")
		// *filepath 通配可能拿到 "css/a.css" 或 "/css/a.css"
		if name == "" {
			notFound(c)
			return
		}
		// 与 net/http.FileServer 一致：清理后拒绝穿越
		name = path.Clean("/" + name)
		name = strings.TrimPrefix(name, "/")
		if name == "" || name == "." || !fs.ValidPath(name) {
			notFound(c)
			return
		}

		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			notFound(c)
			return
		}

		ctype := mime.TypeByExtension(path.Ext(name))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		c.SetContentType(ctype)
		if cfg.cacheControl != "" {
			c.Header("Cache-Control", cfg.cacheControl)
		}
		if cfg.extraHeaders != nil {
			for k, v := range cfg.extraHeaders {
				c.Header(k, v)
			}
		}
		if string(c.Method()) == http.MethodHead {
			c.SetStatusCode(http.StatusOK)
			return
		}
		c.SetStatusCode(http.StatusOK)
		// c.Write 直接进响应缓冲，不经 net/http adaptor
		c.Write(data)
	}
}

type nativeStaticConfig struct {
	prefix       string
	cacheControl string
	extraHeaders map[string]string
}

// NativeStaticOption 配置 NativeStaticFSHandler。
type NativeStaticOption func(*nativeStaticConfig)

// WithNativeCacheControl 设置 Cache-Control 响应头。默认不发送——缓存策略归调用方。
func WithNativeCacheControl(v string) NativeStaticOption {
	return func(c *nativeStaticConfig) { c.cacheControl = v }
}

// WithNativeHeader 追加自定义响应头（同名覆盖）。
func WithNativeHeader(k, v string) NativeStaticOption {
	return func(c *nativeStaticConfig) {
		if c.extraHeaders == nil {
			c.extraHeaders = map[string]string{}
		}
		c.extraHeaders[k] = v
	}
}

func notFound(c *app.RequestContext) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.SetStatusCode(http.StatusNotFound)
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Write([]byte("404 page not found\n"))
}
