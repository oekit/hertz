package hertz

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
)

// TestAddShutdownHook_AppendsToOnShutdown 验证 hook 正确追加到
// h.OnShutdown 切片。
func TestAddShutdownHook_AppendsToOnShutdown(t *testing.T) {
	h := server.New(server.WithHostPorts(":0"))

	before := len(h.OnShutdown)
	AddShutdownHook(h, func(_ context.Context) error { return nil })
	AddShutdownHook(h, func(_ context.Context) error { return nil })

	after := len(h.OnShutdown)
	if after-before != 2 {
		t.Fatalf("expected 2 hooks appended, got %d (before=%d, after=%d)", after-before, before, after)
	}
}

// TestAddShutdownHook_HookInvokedManually 验证直接调用 OnShutdown[i](ctx)
// 时 fn 被调起并接收 ctx。
func TestAddShutdownHook_HookInvokedManually(t *testing.T) {
	h := server.New(server.WithHostPorts(":0"))

	var (
		mu     sync.Mutex
		called bool
		gotCtx context.Context
	)
	AddShutdownHook(h, func(ctx context.Context) error {
		mu.Lock()
		called = true
		gotCtx = ctx
		mu.Unlock()
		return nil
	})

	if len(h.OnShutdown) == 0 {
		t.Fatal("expected at least 1 OnShutdown hook")
	}
	ctx := context.WithValue(context.Background(), keyType("req-id"), "abc-123")
	h.OnShutdown[0](ctx)

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("registered hook was not called")
	}
	if gotCtx.Value(keyType("req-id")) != "abc-123" {
		t.Fatal("hook did not receive the ctx we passed")
	}
}

// TestAddShutdownHook_ErrorLogged 验证 fn 返回 error 时 hook 不 panic,
// 由 bootstrapLog.Warn 记录(error 通过 fmt.Errorf 形式传给 log 字段,
// 验证的关键是 fn 仍被调用,而不是错误传播到 caller)。
func TestAddShutdownHook_ErrorLogged(t *testing.T) {
	h := server.New(server.WithHostPorts(":0"))

	var (
		mu     sync.Mutex
		called bool
	)
	AddShutdownHook(h, func(_ context.Context) error {
		mu.Lock()
		called = true
		mu.Unlock()
		return errors.New("simulated hook error")
	})

	// 直接调 hook 不会 panic(我们只关心"error 不向上传播"语义)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("hook invocation panicked on error return: %v", r)
		}
	}()
	h.OnShutdown[0](context.Background())

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("hook was not called")
	}
}

// TestAddShutdownHook_PreservesOrder 验证多 hook 按 append 顺序执行。
func TestAddShutdownHook_PreservesOrder(t *testing.T) {
	h := server.New(server.WithHostPorts(":0"))

	var (
		mu    sync.Mutex
		order []int
	)

	AddShutdownHook(h, func(_ context.Context) error {
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
		return nil
	})
	AddShutdownHook(h, func(_ context.Context) error {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		return nil
	})
	AddShutdownHook(h, func(_ context.Context) error {
		mu.Lock()
		order = append(order, 3)
		mu.Unlock()
		return nil
	})

	for _, hook := range h.OnShutdown {
		hook(context.Background())
	}

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 3}
	if len(order) != len(want) {
		t.Fatalf("got %d calls, want %d", len(order), len(want))
	}
	for i, v := range want {
		if order[i] != v {
			t.Errorf("order[%d] = %d, want %d", i, order[i], v)
		}
	}
}

// TestAddShutdownHook_RunShutdownTriggers 集成场景说明:本测试未做 e2e 启动
// + Shutdown 流程,因为 hertz 在 engine 未 Run 时直接返回 errEngineNotRunning
// 并跳过 OnShutdown(commit c7cf63f 注释已记);真实 Run + Shutdown 触发路径由
// bootstrap/app_test.go + examples 中的 e2e 覆盖(本测试不替代)。
// helper 本身的行为(append + 手动调 + 多 hook 顺序 + error 不传播)已由前
// 四个 test 充分覆盖。

// keyType 用于 context.Value 的私有类型,避免与其他包冲突。
type keyType string
