package adaptor

import (
	"embed"
	"io/fs"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
)

//go:embed testdata
var nativeFS embed.FS

func TestNativeStaticFSHandler_Serves(t *testing.T) {
	sub, _ := fs.Sub(nativeFS, "testdata")
	e := newTestEngine()
	e.GET("/static/*filepath", NativeStaticFSHandler("/static", sub, WithNativeCacheControl("no-cache")))
	code, out := PerformRequest(e, "GET", "/static/hello.html", &ut.Body{})
	if code != 200 {
		t.Fatalf("code=%d out=%q", code, out)
	}
	body := string(out)
	if !strings.Contains(body, "hello") && !strings.Contains(body, "Hello") {
		t.Fatalf("body=%q", body)
	}
	if !strings.Contains(body, "Cache-Control: no-cache") {
		t.Fatalf("missing cache-control: %q", body)
	}
	if !strings.Contains(strings.ToLower(body), "content-type") {
		t.Fatalf("missing content-type: %q", body)
	}
}

func TestNativeStaticFSHandler_HeadAnd404(t *testing.T) {
	sub, _ := fs.Sub(nativeFS, "testdata")
	e := newTestEngine()
	e.GET("/static/*filepath", NativeStaticFSHandler("/static", sub))
	e.HEAD("/static/*filepath", NativeStaticFSHandler("/static", sub))

	code, out := PerformRequest(e, "GET", "/static/missing.css", &ut.Body{})
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
	if !strings.Contains(string(out), "nosniff") {
		t.Fatalf("404 should set nosniff: %q", out)
	}

	code, _ = PerformRequest(e, "HEAD", "/static/hello.html", &ut.Body{})
	if code != 200 {
		t.Fatalf("HEAD code=%d", code)
	}
}

func TestNativeStaticFSHandler_RejectsTraversal(t *testing.T) {
	sub, _ := fs.Sub(nativeFS, "testdata")
	e := newTestEngine()
	e.GET("/static/*filepath", NativeStaticFSHandler("/static", sub))
	code, _ := PerformRequest(e, "GET", "/static/../secret", &ut.Body{})
	if code != 404 {
		t.Fatalf("traversal should 404, got %d", code)
	}
}
