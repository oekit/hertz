package adaptor

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
)

// ==================== 挂载前缀段边界（#170） ====================

// TestResolveNativeStaticPath_SegmentBoundary 表驱动钉住：剥挂载前缀必须
// 落在**路径段边界**上——剥后余串以 "/" 开头或为空，否则按 miss。
// TrimPrefix 不校验边界会把 "/staticfoo/a.js" 的 "/static" 剥成 "foo/a.js"：
// 该路径与 "/static/a.js" 解析到同一 fsys 文件，产生未声明的别名路径
// （缓存键/审计路径口径分裂）。
func TestResolveNativeStaticPath_SegmentBoundary(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		reqPath  string
		wantName string
		wantOK   bool
	}{
		// 正常挂载形态不受影响
		{"mounted file", "static", "/static/hello.html", "hello.html", true},
		{"mounted subdir", "static", "/static/many/file1.txt", "many/file1.txt", true},
		{"mounted root", "static", "/static", "", true},
		{"mounted with trailing slash", "static", "/static/", "", true},
		{"multi-segment prefix", "assets/v1", "/assets/v1/a.js", "a.js", true},
		// #170 回归：半段前缀按 miss
		{"half-segment prefix is a miss", "static", "/staticfoo/a.js", "", false},
		{"half-segment exact is a miss", "static", "/staticfoo", "", false},
		{"half-segment multi-segment prefix is a miss", "assets/v1", "/assets/v1x/a.js", "", false},
		// 根挂载（prefix=""，SPA 形态）行为不变
		{"root mount file", "", "/hello.html", "hello.html", true},
		{"root mount deep", "", "/a/b/c.css", "a/b/c.css", true},
		{"root mount root", "", "/", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ut.CreateUtRequestContext("GET", tt.reqPath, &ut.Body{})
			got, ok := resolveNativeStaticPath(c, newNativeStaticConfig(tt.prefix))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (name=%q)", ok, tt.wantOK, got)
			}
			if got != tt.wantName {
				t.Errorf("name = %q, want %q", got, tt.wantName)
			}
		})
	}
}

// TestNativeStaticFSHandler_CatchAllPrefixBoundary 行为级回归 #170：prefix
// 非空 + catch-all 兜底路由（SPA 根挂载形态，精确路由 "/static/*filepath"
// 下不可达）时，"/staticfoo/…" 不得折算成 fsys 的 "foo/…" ——测试 fixture
// testdata/foo/hello.html 真实存在，修复前该请求 200 命中别名文件；修复后
// 按 miss 404。正常挂载路径不受影响。
func TestNativeStaticFSHandler_CatchAllPrefixBoundary(t *testing.T) {
	sub, _ := fs.Sub(nativeFS, "testdata")
	e := newTestEngine()
	e.GET("/*filepath", NativeStaticFSHandler("static", sub))

	// 别名路径必须 miss（fsys 里 foo/hello.html 存在，误剥会 200）
	code, out := PerformRequest(e, "GET", "/staticfoo/hello.html", &ut.Body{})
	if code != 404 {
		t.Fatalf("alias path /staticfoo/hello.html = %d, want 404 (undeclared alias must not serve)", code)
	}
	if len(out) == 0 {
		t.Fatal("404 response should carry the nosniff/plain-text body")
	}

	// 正常挂载不受影响
	code, out = PerformRequest(e, "GET", "/static/hello.html", &ut.Body{})
	if code != 200 {
		t.Fatalf("legit path /static/hello.html = %d, want 200", code)
	}
	if !contains(out, "hello-subdir") {
		t.Errorf("legit path served wrong body: %q", out)
	}
}

// TestNativeStaticSPAHandler_CatchAllPrefixBoundary SPA 变体同口径：半段
// 前缀路径不进回退链（修复前 "foo/…" miss 后会被回退成 index.html，
// 别名以 200 形态泄漏）。
func TestNativeStaticSPAHandler_CatchAllPrefixBoundary(t *testing.T) {
	spaSub, _ := fs.Sub(nativeFS, "testdata/spa")
	e := newTestEngine()
	e.GET("/*filepath", NativeStaticSPAHandler("static", spaSub))

	code, _ := PerformRequest(e, "GET", "/staticfoo/whatever", &ut.Body{})
	if code != 404 {
		t.Errorf("alias path via SPA handler = %d, want 404 (must not fall back to index)", code)
	}
}

func contains(b []byte, substr string) bool {
	return strings.Contains(string(b), substr)
}
