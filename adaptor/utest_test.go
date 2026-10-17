package adaptor

import (
	"embed"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
)

// newTestEngine 用 server.New 构造但不 Spin，取其 Engine 做单测。
func newTestEngine() *route.Engine {
	return server.New(server.WithDisablePrintRoute(true)).Engine
}

//go:embed testdata
var utestFS embed.FS

// 本文件回归 hertz#9：adaptor 4 个导出在 ut.PerformRequest 下因
// rc.GetWriter()==nil 全部 panic。MockConn/PerformRequest 补上 conn 后可单测。

func TestPerformRequest_HertzHandler(t *testing.T) {
	e := newTestEngine()
	e.GET("/hi", HertzHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte("hello"))
	}))
	code, out := PerformRequest(e, "GET", "/hi", nil)
	if code != 201 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(string(out), "hello") {
		t.Fatalf("body not on conn: %q", out)
	}
}

func TestPerformRequest_HertzHandlerFunc(t *testing.T) {
	e := newTestEngine()
	e.GET("/x", HertzHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok-func"))
	})))
	_, out := PerformRequest(e, "GET", "/x", nil)
	if !strings.Contains(string(out), "ok-func") {
		t.Fatalf("out=%q", out)
	}
}

func TestPerformRequest_StaticFSHandler(t *testing.T) {
	e := newTestEngine()
	e.GET("/testdata/*filepath", StaticFSHandler("/testdata", utestFS))
	code, out := PerformRequest(e, "GET", "/testdata/hello.html", &ut.Body{})
	if code != 200 {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if !strings.Contains(string(out), "Hello") && !strings.Contains(string(out), "hello") {
		t.Fatalf("static body missing: %q", out)
	}
}

func TestPerformRequest_StaticDirHandler(t *testing.T) {
	e := newTestEngine()
	e.GET("/files/*filepath", StaticDirHandler("/files", "testdata"))
	code, out := PerformRequest(e, "GET", "/files/hello.html", &ut.Body{})
	if code != 200 {
		t.Fatalf("code=%d out=%q", code, out)
	}
}
