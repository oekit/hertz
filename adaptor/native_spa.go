package adaptor

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

// NativeStaticSPAHandler 返回单页应用（history 路由）形态的原生静态 handler。
//
// 与 NativeStaticFSHandler 的唯一行为差异是 **miss 之后怎么办**：前者 404，
// 本 handler 回退到入口文件（缺省 index.html，状态码 200）。Vue/React/Router 的
// history 模式下 `/users/42/settings` 这类路径在磁盘上**本来就没有对应文件**，
// 必须由前端路由自己解析——这也是每个 embed 前端此前都要手写一套的原因
// （含自写扩展名→MIME 表：那是本包 mime.TypeByExtension 的手搓降级）。
//
// 示例：
//
//	//go:embed dist/*
//	var dist embed.FS
//	sub, _ := fs.Sub(dist, "dist") // 注意：sub 根下必须真有 index.html
//	adaptor.NativeStaticSPAHandler("", sub,
//	    adaptor.WithSPAImmutablePrefix("assets/"), // 内容哈希目录：长缓存
//	    adaptor.WithSPASkipPrefix("/api/"))        // API 路径不回退，正常 JSON 404
//
// # 回退的四个判据（全部满足才回退）
//
//  1. **方法必须是 GET/HEAD**——POST/PUT 打到一个不存在的路径时回 index.html
//     会让调用方拿到 200 + 一份 HTML，比 404 难查得多；
//  2. **不在 WithSPASkipPrefix 列出的前缀下**（典型 `/api/`）——这些路径的 404
//     是语义的一部分，回退会把「接口不存在」变成「返回一张网页」；
//  3. **末段不带扩展名**（`path.Ext(末段) == ""`）——发新版本后浏览器请求的是**旧
//     hash 资源**（`/assets/index-a1b2c3.js`）：这里必须 404 让前端察觉并刷新，
//     返回 index.html（200 + text/html）只会让浏览器报 MIME 错，甚至把这个 HTML
//     当成 JS 缓存下来。SPA 路由本身带点（如 `/docs/v1.2`）时用
//     WithSPAServeDottedPaths 关掉这条判据；
//  4. 路径合法（不穿越、fs.ValidPath）。
//
// # 缓存双档
//
// SPA 的缓存策略有**安全缺省**，不是「不写死」：入口 HTML 被长缓存是这类服务的
// 经典事故（发版后用户永远拿到旧 index → 引用旧 hash 资源 → 白屏，且刷新无效）。
// 故本变体缺省 **entry = no-cache**（`WithNativeCacheControl` 可覆盖），命中
// `WithSPAImmutablePrefix` 的文件缺省 **immutable 长缓存**，两者可用
// `WithSPAImmutableCacheControl` / `WithNativeCacheControl` 各自覆盖。
//
// # 构造期校验
//
// 回退文件不存在时**构造期 panic**：`fs.Sub` 少写或多写一层目录（`embed dist/*`
// 却 `fs.Sub(dist, "dist/dist")`）是这类接线的头号错误，而它的失效形态是
// 「每个前端路由都 404、日志一个字都没有」。panic 信息会列出 fsys 根下的前几个
// 条目，直接指出该改哪一层。
//
// prefix 语义与 NativeStaticFSHandler 一致：prefix 为空表示挂在根（`"/"` 会回退
// 到入口文件，而不是 404）。
func NativeStaticSPAHandler(prefix string, fsys fs.FS, opts ...NativeStaticOption) app.HandlerFunc {
	cfg := newNativeStaticConfig(prefix, opts...)
	cfg.applySPADefaults()
	ensureSPAIndex(fsys, &cfg)

	return func(ctx context.Context, c *app.RequestContext) {
		name, ok := resolveNativeStaticPath(c, cfg)
		if !ok {
			notFound(c)
			return
		}
		if name != "" {
			if data, err := fs.ReadFile(fsys, name); err == nil {
				serveNativeFile(c, name, data, cfg.cacheControlFor(name), cfg.extraHeaders)
				return
			}
		}
		if !cfg.spaFallbackAllowed(c, name) {
			notFound(c)
			return
		}
		// 构造期已校验过入口文件存在；这里再读一次是刻意的——fsys 可能是
		// os.DirFS（开发期），构造后文件被删掉不该 panic，404 即可。
		data, err := fs.ReadFile(fsys, cfg.spaIndex)
		if err != nil {
			notFound(c)
			return
		}
		serveNativeFile(c, cfg.spaIndex, data, cfg.cacheControlFor(cfg.spaIndex), cfg.extraHeaders)
	}
}

// defaultSPAIndex 是回退文件名。改名是配置项（WithSPAIndex），不是硬编码口径。
const defaultSPAIndex = "index.html"

// defaultSPAImmutableCache 是内容哈希目录的缺省缓存值：一年 + immutable。
// 文件名带内容哈希时这是安全的——内容一变文件名就变。
const defaultSPAImmutableCache = "public, max-age=31536000, immutable"

// defaultSPACache 是 SPA 变体对入口/非 immutable 文件的缺省缓存值。
const defaultSPACache = "no-cache"

// applySPADefaults 填 SPA 缺省值并归一化前缀。
func (cfg *nativeStaticConfig) applySPADefaults() {
	if cfg.spaIndex == "" {
		cfg.spaIndex = defaultSPAIndex
	}
	if cfg.spaImmutableCache == "" {
		cfg.spaImmutableCache = defaultSPAImmutableCache
	}
	if cfg.cacheControl == "" {
		cfg.cacheControl = defaultSPACache
	}
	for i, p := range cfg.spaImmutable {
		cfg.spaImmutable[i] = normalizeFSDirPrefix(p)
	}
	for i, p := range cfg.spaSkip {
		cfg.spaSkip[i] = normalizeURLDirPrefix(p)
	}
}

// spaFallbackAllowed 判「这次 miss 该不该回退到入口文件」。
func (cfg *nativeStaticConfig) spaFallbackAllowed(c *app.RequestContext, name string) bool {
	switch string(c.Method()) {
	case http.MethodGet, http.MethodHead:
	default:
		return false
	}
	if matchURLDirPrefix(string(c.Path()), cfg.spaSkip) {
		return false
	}
	// 末段带扩展名 = 请求的是一个**具体文件**：它不存在就该是 404。
	// name 为空表示 fsys 根（请求 "/"），一律按「不是文件」处理——
	// path.Base("") 是 "."，而 path.Ext(".") 恰好返回 "."，不加这道 guard 会把根路径判成带扩展名。
	if base := path.Base(name); base != "." && base != "" && !cfg.spaServeDotted {
		if path.Ext(base) != "" {
			return false
		}
	}
	return true
}

// cacheControlFor 命中 immutable 前缀走长缓存，其余走 entry 档。
func (cfg *nativeStaticConfig) cacheControlFor(name string) string {
	if matchFSDirPrefix(name, cfg.spaImmutable) {
		return cfg.spaImmutableCache
	}
	return cfg.cacheControl
}

// WithSPAIndex 设置 miss 时的回退文件名（缺省 index.html）。
func WithSPAIndex(name string) NativeStaticOption {
	return func(c *nativeStaticConfig) { c.spaIndex = strings.TrimPrefix(name, "/") }
}

// WithSPAImmutablePrefix 声明一个「内容哈希目录」，其下文件走 immutable 长缓存。
//
// 可多次调用累加。前缀相对于 fsys 根（`embed dist/*` + `assets/` ⇒ `assets/`）。
// 缺省无前缀——不声明就全部走 entry 档，宁可少缓存也不要缓存旧资源。
func WithSPAImmutablePrefix(prefix string) NativeStaticOption {
	return func(c *nativeStaticConfig) {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			c.spaImmutable = append(c.spaImmutable, prefix)
		}
	}
}

// WithSPAImmutableCacheControl 覆盖 immutable 档的 Cache-Control 值。
func WithSPAImmutableCacheControl(v string) NativeStaticOption {
	return func(c *nativeStaticConfig) { c.spaImmutableCache = v }
}

// WithSPASkipPrefix 声明「不回退到入口文件」的请求路径前缀，典型是 API 前缀
// （`/api/`）——那里的 404 是语义的一部分，被回退成一张网页会把「接口不存在」
// 变成「调用方拿到一段 HTML」。
//
// 可多次调用累加；前缀按 URL 路径匹配（前导 `/` 与尾随 `/` 会自动补齐）。
func WithSPASkipPrefix(prefix string) NativeStaticOption {
	return func(c *nativeStaticConfig) {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			c.spaSkip = append(c.spaSkip, prefix)
		}
	}
}

// WithSPAServeDottedPaths 关闭「末段带扩展名就不回退」的缺省判据。
//
// 只在 SPA 路由本身带点时才需要（如 `/docs/v1.2`）。开启后**旧 hash 资源**
// （`/assets/index-a1b2c3.js`）也会回退成 index.html 200——前端将拿不到 404、
// 无法察觉发版并刷新，浏览器还会报 MIME 错。开启前请确认没有这类资源，
// 或把资源目录登记进 WithSPASkipPrefix。
func WithSPAServeDottedPaths() NativeStaticOption {
	return func(c *nativeStaticConfig) { c.spaServeDotted = true }
}

// ensureSPAIndex 构造期校验回退文件：它不存在时**每个前端路由都会 404**，
// 而日志里一个字都没有。
func ensureSPAIndex(fsys fs.FS, cfg *nativeStaticConfig) {
	if !fs.ValidPath(cfg.spaIndex) {
		panic(fmt.Sprintf("adaptor: NativeStaticSPAHandler 的回退文件路径非法: %q"+
			"（应为 fsys 内的相对路径，如 index.html）", cfg.spaIndex))
	}
	if _, err := fs.Stat(fsys, cfg.spaIndex); err == nil {
		return
	}
	panic(fmt.Sprintf("adaptor: NativeStaticSPAHandler 在 fsys 根下找不到回退文件 %q%s"+
		"——常见原因是 fs.Sub 的目录层级多写/少写了一层（embed dist/* 却 fs.Sub(dist, \"dist/dist\")）",
		cfg.spaIndex, spaRootHint(fsys)))
}

// spaRootHint 列出 fsys 根下的前几个条目，供上面那条 panic 直接指出该改哪一层。
func spaRootHint(fsys fs.FS) string {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil || len(entries) == 0 {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	const maxShown = 5
	suffix := ""
	if len(names) > maxShown {
		names, suffix = names[:maxShown], " …"
	}
	return fmt.Sprintf("（fsys 根下是：%s%s）", strings.Join(names, ", "), suffix)
}

// normalizeFSDirPrefix 把前缀归一成 fsys 相对形态：无前导 `/`、有尾随 `/`。
func normalizeFSDirPrefix(p string) string {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// normalizeURLDirPrefix 把前缀归一成 URL 形态：有前导 `/`、有尾随 `/`。
func normalizeURLDirPrefix(p string) string {
	p = "/" + strings.Trim(strings.TrimSpace(p), "/")
	if p == "/" {
		return p
	}
	return p + "/"
}

// matchFSDirPrefix 判 fsys 相对路径是否落在某个目录前缀下（含前缀本身）。
func matchFSDirPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if name == strings.TrimSuffix(p, "/") || strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// matchURLDirPrefix 判请求路径是否落在某个目录前缀下（含前缀本身）。
func matchURLDirPrefix(reqPath string, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if reqPath == strings.TrimSuffix(p, "/") || strings.HasPrefix(reqPath, p) {
			return true
		}
	}
	return false
}
