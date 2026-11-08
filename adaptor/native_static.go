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
//
// 前端单页应用（history 路由）用 NativeStaticSPAHandler：它在本 handler 的基础上
// 加「miss → 回退 index.html」与 immutable/no-cache 双档缓存。
func NativeStaticFSHandler(prefix string, fsys fs.FS, opts ...NativeStaticOption) app.HandlerFunc {
	cfg := newNativeStaticConfig(prefix, opts...)
	return func(ctx context.Context, c *app.RequestContext) {
		name, ok := resolveNativeStaticPath(c, cfg)
		if !ok || name == "" {
			notFound(c)
			return
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			notFound(c)
			return
		}
		serveNativeFile(c, name, data, cfg.cacheControl, cfg.extraHeaders)
	}
}

// newNativeStaticConfig 归一化配置：选项按调用顺序套用，nil 选项跳过。
func newNativeStaticConfig(prefix string, opts ...NativeStaticOption) nativeStaticConfig {
	cfg := nativeStaticConfig{prefix: strings.Trim(prefix, "/")}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// resolveNativeStaticPath 把请求路径折算成 fsys 内的相对路径。
//
// prefix 必须落在**路径段边界**上：剥挂载前缀后余串必须以 "/" 开头或为空，
// 否则按 miss 处理（#170）。TrimPrefix 不校验边界会把 "/staticfoo/a.js" 的
// "/static" 剥成 "foo/a.js"——该路径与 "/static/a.js" 解析到同一 fsys 文件，
// 产生未声明的别名路径（缓存键/审计路径口径分裂）。精确路由
// "/static/*filepath" 下不可达，但 prefix 非空 + catch-all 兜底路由（SPA
// 根挂载形态）组合完全可能。
//
// ok=false 表示路径非法（穿越/空段/非规范形态/前缀未落在段边界）；name
// 可能为空串，含义是**fsys 根**——普通静态 handler 视为 miss（根不是文件），
// SPA handler 视为「交给 index」。两者对空串的态度不同，故由调用方判。
func resolveNativeStaticPath(c *app.RequestContext, cfg nativeStaticConfig) (string, bool) {
	p := string(c.Path())
	rest := p
	if cfg.prefix != "" {
		mount := "/" + cfg.prefix
		if !strings.HasPrefix(p, mount) {
			return "", false
		}
		rest = p[len(mount):]
		if rest != "" && !strings.HasPrefix(rest, "/") {
			// 前缀没落在段边界（/staticfoo 相对 /static）：按 miss，
			// 不把半段折算进 fsys（别名路径分裂缓存键与审计口径）
			return "", false
		}
	}
	name := strings.TrimPrefix(rest, "/")
	if name == "" {
		return "", true
	}
	// 与 net/http.FileServer 一致：清理后拒绝穿越
	name = path.Clean("/" + name)
	name = strings.TrimPrefix(name, "/")
	if name == "" || name == "." || !fs.ValidPath(name) {
		return "", false
	}
	return name, true
}

// serveNativeFile 写响应：Content-Type 由扩展名推导，缓存与自定义头由调用方给。
//
// 静态托管最容易漏的是 HEAD——只回头不回体，否则 HEAD 请求会带一整份文件。
func serveNativeFile(c *app.RequestContext, name string, data []byte, cacheControl string, extraHeaders map[string]string) {
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	c.SetContentType(ctype)
	if cacheControl != "" {
		c.Header("Cache-Control", cacheControl)
	}
	for k, v := range extraHeaders {
		c.Header(k, v)
	}
	c.SetStatusCode(http.StatusOK)
	if string(c.Method()) == http.MethodHead {
		return
	}
	// c.Write 直接进响应缓冲，不经 net/http adaptor
	_, _ = c.Write(data)
}

type nativeStaticConfig struct {
	prefix       string
	cacheControl string
	extraHeaders map[string]string

	// SPA 形态专用（NativeStaticFSHandler 不读这些字段）。
	spaIndex          string
	spaImmutable      []string
	spaImmutableCache string
	spaSkip           []string
	spaServeDotted    bool
}

// NativeStaticOption 配置 NativeStaticFSHandler / NativeStaticSPAHandler。
type NativeStaticOption func(*nativeStaticConfig)

// WithNativeCacheControl 设置 Cache-Control 响应头。默认不发送——缓存策略归调用方。
//
// SPA 变体里它是**非 immutable 文件**（含 index.html）那一档的值；未设置时
// SPA 变体缺省 no-cache。
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
	_, _ = c.WriteString("404 page not found\n")
}
