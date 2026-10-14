package render

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/vmihailenco/msgpack/v5"
)

// renderIsolatedServer 每次测试一个独立 hertz 实例,避免路由表污染。
func renderIsolatedServer(t *testing.T) *server.Hertz {
	t.Helper()
	h := server.New(
		server.WithHostPorts("127.0.0.1:0"),
		server.WithDisablePrintRoute(true),
	)
	return h
}

// TestJSONRPC_SuccessResult 验证 JSONRPC success path: 编码 result 字段,
// 省略 error 字段,带 jsonrpc 协议版本。
func TestJSONRPC_SuccessResult(t *testing.T) {
	h := renderIsolatedServer(t)
	h.GET("/rpc", func(_ context.Context, c *app.RequestContext) {
		c.Render(200, JSONRPC{Result: map[string]any{"pong": true}, ID: 1})
	})
	w := ut.PerformRequest(h.Engine, "GET", "/rpc", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := string(w.Header().ContentType()); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}
	var env struct {
		JSONRPC string         `json:"jsonrpc"`
		Result  map[string]any `json:"result"`
		Error   any            `json:"error"`
		ID      any            `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", env.JSONRPC)
	}
	if env.Result["pong"] != true {
		t.Errorf("Result.pong = %v, want true", env.Result["pong"])
	}
	if env.Error != nil {
		t.Errorf("Error = %v, want nil for success", env.Error)
	}
}

// TestJSONRPC_Error 验证 JSONRPC error path: 编码 error 对象,省略 result。
func TestJSONRPC_Error(t *testing.T) {
	h := renderIsolatedServer(t)
	h.GET("/rpc", func(_ context.Context, c *app.RequestContext) {
		c.Render(200, JSONRPC{
			Error: &JSONRPCError{Code: -32601, Message: "Method not found"},
			ID:    "req-1",
		})
	})
	w := ut.PerformRequest(h.Engine, "GET", "/rpc", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var env struct {
		JSONRPC string        `json:"jsonrpc"`
		Result  any           `json:"result"`
		Error   *JSONRPCError `json:"error"`
		ID      any           `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error == nil {
		t.Fatal("Error = nil, want non-nil")
	}
	if env.Error.Code != -32601 {
		t.Errorf("Error.Code = %d, want -32601", env.Error.Code)
	}
	if env.Result != nil {
		t.Errorf("Result = %v, want nil for error", env.Result)
	}
	if env.ID != "req-1" {
		t.Errorf("ID = %v, want req-1", env.ID)
	}
}

// TestJSONRPC_CustomVersion 验证 Version 字段可显式覆盖(虽然 JSON-RPC
// 2.0 写死,本测试为防御未来 spec 变更或内部协议预留)。
func TestJSONRPC_CustomVersion(t *testing.T) {
	h := renderIsolatedServer(t)
	h.GET("/rpc", func(_ context.Context, c *app.RequestContext) {
		c.Render(200, JSONRPC{Version: "1.0", Result: "ok", ID: 1})
	})
	w := ut.PerformRequest(h.Engine, "GET", "/rpc", nil)
	var env struct {
		JSONRPC string `json:"jsonrpc"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.JSONRPC != "1.0" {
		t.Errorf("jsonrpc = %q, want 1.0", env.JSONRPC)
	}
}

// TestMessagePack_Encoding 验证 MessagePack 二进制编码正确,与 msgpack
// 标准库 round-trip 一致。
func TestMessagePack_Encoding(t *testing.T) {
	h := renderIsolatedServer(t)
	type payload struct {
		Name  string `msgpack:"name"`
		Count int    `msgpack:"count"`
	}
	h.GET("/mp", func(_ context.Context, c *app.RequestContext) {
		c.Render(200, MessagePack{Data: payload{Name: "alice", Count: 42}})
	})
	w := ut.PerformRequest(h.Engine, "GET", "/mp", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := string(w.Header().ContentType())
	if !strings.HasPrefix(ct, "application/x-msgpack") {
		t.Errorf("Content-Type = %q, want application/x-msgpack", ct)
	}
	var got payload
	if err := msgpack.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("msgpack unmarshal: %v", err)
	}
	if got.Name != "alice" || got.Count != 42 {
		t.Errorf("decoded = %+v, want {alice 42}", got)
	}
}

// TestCSV_Basic 验证 CSV 序列化与解码 round-trip。
func TestCSV_Basic(t *testing.T) {
	h := renderIsolatedServer(t)
	rows := [][]string{
		{"name", "age", "active"},
		{"alice", "30", "true"},
		{"bob", "25", "false"},
	}
	h.GET("/csv", func(_ context.Context, c *app.RequestContext) {
		c.Render(200, CSV{Rows: rows})
	})
	w := ut.PerformRequest(h.Engine, "GET", "/csv", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := string(w.Header().ContentType())
	if !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	r := csv.NewReader(bytes.NewReader(w.Body.Bytes()))
	got, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	if got[0][0] != "name" || got[0][1] != "age" || got[0][2] != "active" {
		t.Errorf("header = %v, want [name age active]", got[0])
	}
	if got[1][0] != "alice" || got[1][1] != "30" {
		t.Errorf("row 1 = %v, want [alice 30 ...]", got[1])
	}
}

// TestCSV_Empty 验证空 Rows 仍返回合法 CSV(空行)。
func TestCSV_Empty(t *testing.T) {
	h := renderIsolatedServer(t)
	h.GET("/csv", func(_ context.Context, c *app.RequestContext) {
		c.Render(200, CSV{Rows: nil})
	})
	w := ut.PerformRequest(h.Engine, "GET", "/csv", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("empty CSV should still emit a newline sentinel")
	}
}

// TestCSV_QuoteEscape 验证 CSV 内含逗号/引号被正确转义(RFC 4180)。
func TestCSV_QuoteEscape(t *testing.T) {
	h := renderIsolatedServer(t)
	rows := [][]string{
		{`hello, "world"`},
		{"normal"},
	}
	h.GET("/csv", func(_ context.Context, c *app.RequestContext) {
		c.Render(200, CSV{Rows: rows})
	})
	w := ut.PerformRequest(h.Engine, "GET", "/csv", nil)
	r := csv.NewReader(bytes.NewReader(w.Body.Bytes()))
	got, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv read: %v", err)
	}
	if got[0][0] != `hello, "world"` {
		t.Errorf("escaped value = %q, want %q", got[0][0], `hello, "world"`)
	}
}

// TestNewCSVFromAny_TypeConversion 验证 NewCSVFromAny 转换不同基础类型。
func TestNewCSVFromAny_TypeConversion(t *testing.T) {
	rows := []any{
		[]any{"alice", 30, true, 3.14, nil},
	}
	c := NewCSVFromAny(rows)
	if len(c.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(c.Rows))
	}
	if c.Rows[0][0] != "alice" {
		t.Errorf("col 0 = %q, want alice", c.Rows[0][0])
	}
	if c.Rows[0][1] != "30" {
		t.Errorf("col 1 = %q, want 30", c.Rows[0][1])
	}
	if c.Rows[0][2] != "true" {
		t.Errorf("col 2 = %q, want true", c.Rows[0][2])
	}
	if c.Rows[0][3] != "3.14" {
		t.Errorf("col 3 = %q, want 3.14", c.Rows[0][3])
	}
	if c.Rows[0][4] != "" {
		t.Errorf("col 4 = %q, want empty (nil)", c.Rows[0][4])
	}
}

// TestJSONRPC_VoidResultNull 回归：JSON-RPC 2.0 规范要求成功响应必须携带
// result（void 过程也要有 "result":null，规范禁止 result 与 error 皆无）。
// Result 用 any+omitempty 时 void 响应曾整字段缺失。
func TestJSONRPC_VoidResultNull(t *testing.T) {
	h := renderIsolatedServer(t)
	h.GET("/rpc", func(_ context.Context, c *app.RequestContext) {
		c.Render(200, JSONRPC{ID: 7}) // Result nil → 必须编码为字面 null
	})
	w := ut.PerformRequest(h.Engine, "GET", "/rpc", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"result":null`) {
		t.Errorf(`void success body %s missing "result":null`, w.Body.String())
	}
	var env struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   any    `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.ID != float64(7) {
		t.Errorf("ID = %v, want 7", env.ID)
	}
	if env.Error != nil {
		t.Errorf("Error = %v, want absent for success", env.Error)
	}
}
