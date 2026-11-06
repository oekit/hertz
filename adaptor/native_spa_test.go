package adaptor

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
)

// spaFS 是 SPA 用例的 fsys 根：index.html / shell.html / robots.txt / assets/app.js。
func spaFS(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(nativeFS, "testdata/spa")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	return sub
}

// newSPAEngine 把 SPA handler 挂到根路由上（prefix 为空 = 挂在 "/"）。
func newSPAEngine(t *testing.T, opts ...NativeStaticOption) *route.Engine {
	t.Helper()
	h := NativeStaticSPAHandler("", spaFS(t), opts...)
	e := newTestEngine()
	e.GET("/", h)
	e.GET("/*filepath", h)
	e.HEAD("/*filepath", h)
	e.POST("/*filepath", h)
	return e
}

func TestNativeStaticSPA_RootServesIndex(t *testing.T) {
	e := newSPAEngine(t)
	code, out := PerformRequest(e, "GET", "/", &ut.Body{})
	if code != 200 {
		t.Fatalf("根路径应回退到 index（code=%d out=%q）", code, out)
	}
	body := string(out)
	if !strings.Contains(body, "spa-index") {
		t.Fatalf("根路径应返回 index 内容: %q", body)
	}
	if !strings.Contains(strings.ToLower(body), "content-type: text/html") {
		t.Fatalf("入口文件必须是 text/html: %q", body)
	}
	// 入口 HTML 缺省 no-cache——长缓存会让发版后用户永远拿到旧 index。
	if !strings.Contains(body, "Cache-Control: no-cache") {
		t.Fatalf("入口 HTML 缺省应为 no-cache: %q", body)
	}
}

func TestNativeStaticSPA_RouteFallsBackToIndex(t *testing.T) {
	e := newSPAEngine(t)
	for _, p := range []string{"/users/42/settings", "/dashboard", "/a/b/c/d"} {
		code, out := PerformRequest(e, "GET", p, &ut.Body{})
		if code != 200 {
			t.Fatalf("%s 应回退到 index，实际 code=%d", p, code)
		}
		if !strings.Contains(string(out), "spa-index") {
			t.Fatalf("%s 回退体不对: %q", p, out)
		}
	}
}

func TestNativeStaticSPA_ExistingAssetServedWithImmutableCache(t *testing.T) {
	e := newSPAEngine(t, WithSPAImmutablePrefix("assets/"))
	code, out := PerformRequest(e, "GET", "/assets/app.js", &ut.Body{})
	if code != 200 {
		t.Fatalf("存在的资源应直出，实际 code=%d", code)
	}
	body := string(out)
	if !strings.Contains(body, "spa-asset") {
		t.Fatalf("资源内容不对: %q", body)
	}
	if !strings.Contains(body, "immutable") {
		t.Fatalf("immutable 前缀下的文件应带长缓存: %q", body)
	}
	if !strings.Contains(strings.ToLower(body), "javascript") {
		t.Fatalf("Content-Type 应由扩展名推导: %q", body)
	}
}

// 这条是 SPA 回退最贵的一个坑：发新版本后浏览器请求**旧 hash 资源**，
// 返回 index.html 200 会让浏览器报 MIME 错，甚至把 HTML 当 JS 缓存。
func TestNativeStaticSPA_MissingAssetMustNotFallBack(t *testing.T) {
	e := newSPAEngine(t, WithSPAImmutablePrefix("assets/"))
	for _, p := range []string{
		"/assets/index-a1b2c3.js", // 旧 hash 资源
		"/missing.css",            // 根下缺失的样式
		"/fonts/inter-var.woff2",  // 嵌套缺失资源
	} {
		code, out := PerformRequest(e, "GET", p, &ut.Body{})
		if code != 404 {
			t.Fatalf("%s 必须 404（带扩展名 = 具体文件），实际 code=%d body=%q", p, code, out)
		}
		if strings.Contains(string(out), "spa-index") {
			t.Fatalf("%s 不得回退成 index.html: %q", p, out)
		}
	}
}

func TestNativeStaticSPA_SkipPrefixNoFallback(t *testing.T) {
	e := newSPAEngine(t, WithSPASkipPrefix("/api/"))
	for _, p := range []string{"/api/users", "/api", "/api/v1/items/1"} {
		code, out := PerformRequest(e, "GET", p, &ut.Body{})
		if code != 404 {
			t.Fatalf("%s 在 skip 前缀下必须 404，实际 code=%d body=%q", p, code, out)
		}
	}
	// 非 skip 前缀照常回退
	if code, _ := PerformRequest(e, "GET", "/users/1", &ut.Body{}); code != 200 {
		t.Fatalf("/users/1 应回退，实际 code=%d", code)
	}
}

func TestNativeStaticSPA_NonGetDoesNotFallBack(t *testing.T) {
	e := newSPAEngine(t)
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		code, out := PerformRequest(e, m, "/users/42", &ut.Body{})
		if code != 404 {
			t.Fatalf("%s 不得回退成 index（否则调用方拿到 200+HTML），实际 code=%d body=%q", m, code, out)
		}
	}
}

func TestNativeStaticSPA_DottedRouteNeedsOptIn(t *testing.T) {
	// 缺省：带点 = 具体文件 ⇒ 404
	e := newSPAEngine(t)
	if code, _ := PerformRequest(e, "GET", "/docs/v1.2", &ut.Body{}); code != 404 {
		t.Fatalf("带点路径缺省应 404，实际 code=%d", code)
	}

	// 显式开启后才回退
	e2 := newSPAEngine(t, WithSPAServeDottedPaths())
	code, out := PerformRequest(e2, "GET", "/docs/v1.2", &ut.Body{})
	if code != 200 || !strings.Contains(string(out), "spa-index") {
		t.Fatalf("WithSPAServeDottedPaths 后应回退，实际 code=%d body=%q", code, out)
	}
}

func TestNativeStaticSPA_HeadReturnsNoBody(t *testing.T) {
	e := newSPAEngine(t)
	code, out := PerformRequest(e, "HEAD", "/users/42", &ut.Body{})
	if code != 200 {
		t.Fatalf("HEAD 回退应 200，实际 %d", code)
	}
	if strings.Contains(string(out), "spa-index") {
		t.Fatalf("HEAD 不得带体: %q", out)
	}
}

func TestNativeStaticSPA_CustomIndex(t *testing.T) {
	e := newSPAEngine(t, WithSPAIndex("shell.html"))
	code, out := PerformRequest(e, "GET", "/anywhere", &ut.Body{})
	if code != 200 || !strings.Contains(string(out), "spa-shell") {
		t.Fatalf("WithSPAIndex(shell.html) 应回退到 shell，实际 code=%d body=%q", code, out)
	}
}

func TestNativeStaticSPA_CacheControlOverride(t *testing.T) {
	e := newSPAEngine(t,
		WithSPAImmutablePrefix("assets/"),
		WithNativeCacheControl("max-age=60"),
		WithSPAImmutableCacheControl("public, max-age=604800"))
	if _, out := PerformRequest(e, "GET", "/assets/app.js", &ut.Body{}); !strings.Contains(string(out), "max-age=604800") {
		t.Fatalf("immutable 档应被覆盖: %q", out)
	}
	if _, out := PerformRequest(e, "GET", "/users/1", &ut.Body{}); !strings.Contains(string(out), "max-age=60") {
		t.Fatalf("entry 档应被 WithNativeCacheControl 覆盖: %q", out)
	}
}

// 穿越判据的落点是「读不到 fsys 之外的文件」，而不是某个具体状态码：
// 路径清理后 ".." 会被折到根，于是清理结果落在 fsys 内、不存在 ⇒ 走正常 miss 分支。
// 这里用 fsys **外面**那个 hello.html 做诱饵（内容 hello-subdir）：任何一点实现
// 松懈都会把它读出来。
func TestNativeStaticSPA_TraversalRejected(t *testing.T) {
	e := newSPAEngine(t)
	for _, p := range []string{"/../hello.html", "/assets/../../hello.html", "/./../hello.html"} {
		code, out := PerformRequest(e, "GET", p, &ut.Body{})
		if strings.Contains(string(out), "hello-subdir") {
			t.Fatalf("%s 读到了 fsys 之外的文件: %q", p, out)
		}
		if code != 404 {
			t.Fatalf("%s 清理后是带扩展名的缺失文件 ⇒ 应 404，实际 %d", p, code)
		}
	}
}

// 回退文件不存在是接线错误（fs.Sub 层级写错），必须在构造期就炸：
// 否则每个前端路由都 404 而日志一个字都没有。
func TestNativeStaticSPA_PanicsWhenIndexMissing(t *testing.T) {
	sub, err := fs.Sub(nativeFS, "testdata/empty")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("回退文件不存在时必须 panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, defaultSPAIndex) {
			t.Fatalf("panic 应点名缺失的回退文件，实际: %v", r)
		}
		if !strings.Contains(msg, "fs.Sub") {
			t.Fatalf("panic 应给出最常见的成因（fs.Sub 层级），实际: %v", r)
		}
		if !strings.Contains(msg, "x.txt") {
			t.Fatalf("panic 应列出 fsys 根下的条目便于定位，实际: %v", r)
		}
	}()
	NativeStaticSPAHandler("", sub)
}

// 前缀归一化的边界：前缀写成 "/" 时它归一成空串（防守式跳过），不能变成
// 「所有文件都 immutable」这种放大失效。
func TestNativeStaticSPA_DegeneratePrefixes(t *testing.T) {
	e := newSPAEngine(t, WithSPAImmutablePrefix("/"))
	if _, out := PerformRequest(e, "GET", "/assets/app.js", &ut.Body{}); !strings.Contains(string(out), "no-cache") {
		t.Fatalf("退化前缀不应让所有文件走 immutable，实际: %q", out)
	}

	// skip 前缀写成 "/" 则匹配一切 —— 连根路径都 404（这是调用方要的「全不回退」）
	e2 := newSPAEngine(t, WithSPASkipPrefix("/"))
	for _, p := range []string{"/", "/users/1"} {
		if code, out := PerformRequest(e2, "GET", p, &ut.Body{}); code != 404 {
			t.Fatalf("skip 前缀 \"/\" 下 %s 应 404，实际 code=%d body=%q", p, code, out)
		}
	}
}

func TestNativeStaticSPA_SkipPrefixWithoutSlashes(t *testing.T) {
	// 调用方写 "api" / "api/" / "/api/" 都应等价
	for _, pre := range []string{"api", "api/", "/api/"} {
		e := newSPAEngine(t, WithSPASkipPrefix(pre))
		if code, _ := PerformRequest(e, "GET", "/api/users", &ut.Body{}); code != 404 {
			t.Fatalf("前缀 %q 应生效，实际 code=%d", pre, code)
		}
	}
}

func TestNativeStaticSPA_PanicsOnInvalidIndexPath(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("非法回退文件路径必须 panic")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "非法") {
			t.Fatalf("panic 应指出路径非法，实际: %v", r)
		}
	}()
	NativeStaticSPAHandler("", spaFS(t), WithSPAIndex("../etc/passwd"))
}

// panic 提示里列根目录条目时要截断：几十个文件的根列表会把真正的提示淹掉。
func TestNativeStaticSPA_RootHintTruncates(t *testing.T) {
	sub, err := fs.Sub(nativeFS, "testdata/many")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("缺 index.html 必须 panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "file1.txt") {
			t.Fatalf("提示应列出根下条目，实际: %v", r)
		}
		if strings.Contains(msg, "file7.txt") {
			t.Fatalf("超过 5 个条目应截断，实际: %v", r)
		}
		if !strings.Contains(msg, "…") {
			t.Fatalf("截断应留省略号，实际: %v", r)
		}
	}()
	NativeStaticSPAHandler("", sub)
}

// WithNativeHeader 对两个变体都必须生效（SPA 变体复用了同一段写头逻辑）。
func TestNativeStatic_ExtraHeadersBothVariants(t *testing.T) {
	e := newSPAEngine(t, WithNativeHeader("X-Frame-Options", "DENY"))
	for _, p := range []string{"/users/1", "/assets/app.js"} {
		if _, out := PerformRequest(e, "GET", p, &ut.Body{}); !strings.Contains(string(out), "X-Frame-Options: DENY") {
			t.Fatalf("%s 应带自定义头: %q", p, out)
		}
	}

	base := newTestEngine()
	base.GET("/static/*filepath", NativeStaticFSHandler("/static", spaFS(t), WithNativeHeader("X-Test", "1")))
	if _, out := PerformRequest(base, "GET", "/static/robots.txt", &ut.Body{}); !strings.Contains(string(out), "X-Test: 1") {
		t.Fatalf("基础变体也应带自定义头: %q", out)
	}
}

// 扩展名推不出 MIME 时回退 octet-stream（不是空 Content-Type）。
func TestNativeStaticSPA_UnknownExtensionFallsBackToOctetStream(t *testing.T) {
	e := newSPAEngine(t)
	code, out := PerformRequest(e, "GET", "/LICENSE", &ut.Body{})
	if code != 200 {
		t.Fatalf("LICENSE 存在应直出，实际 %d", code)
	}
	if !strings.Contains(strings.ToLower(string(out)), "content-type: application/octet-stream") {
		t.Fatalf("未知扩展名应回退 octet-stream: %q", out)
	}
}

// 清理后落到根（"/static/."）的请求按非法路径处理，与「根不是文件」同口径。
func TestNativeStaticFS_DotSegmentRejected(t *testing.T) {
	e := newTestEngine()
	e.GET("/static/*filepath", NativeStaticFSHandler("/static", spaFS(t)))
	if code, _ := PerformRequest(e, "GET", "/static/.", &ut.Body{}); code != 404 {
		t.Fatalf("\"/static/.\" 清理后是根 ⇒ 应 404，实际 %d", code)
	}
}
