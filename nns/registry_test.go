package nns

import (
	"context"
	"net"
	"sync"
	"testing"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	nnsclient "github.com/oekit/nns/client"
	ereg "github.com/oekit/registry"
	registrymocks "github.com/oekit/registry/registrymocks"
)

// ctxRecordingRegistrar 记录 Register/Deregister 收到的 ctx（钉住 ctx 透传）。
type ctxRecordingRegistrar struct {
	mu            sync.Mutex
	registerCtx   context.Context
	deregisterCtx context.Context
}

func (r *ctxRecordingRegistrar) Register(ctx context.Context, _ ereg.Instance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerCtx = ctx
	return nil
}

func (r *ctxRecordingRegistrar) Deregister(ctx context.Context, _ ereg.Instance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deregisterCtx = ctx
	return nil
}

func (r *ctxRecordingRegistrar) snapshot() (context.Context, context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registerCtx, r.deregisterCtx
}

// TestNnsRegistry_DeregisterKeepsTagsAndWeight 钉住 nns Deregister
// 跟 Register 对称:保留 tags 和 defaultWeight 兜底,跟 nns/kitex 行为一致
// (commit b0a1766 / plan A Task 7)。
//
// 之前 nns.Deregister 传 (addr, 0, 0, nil) — 丢 weight + 兜底 + tags,
// 依赖 metadata/权重匹配的注册中心(nacos 等)无法正确注销。
//
// 实现:直接用 nnsclient.New + WithRegistrar 注入 fake,绕开 nns.New 抽象
// (nns.New 自身 Option 集合不暴露 WithRegistrar;但 NNSClient 内部已用
// nnsclient.New 构造,ServiceClient 可独立测试)。对齐 nns/kitex/kitex_coverage_test.go
// 的 newTestClientWithRegistrar 模式。
func TestNnsRegistry_DeregisterKeepsTagsAndWeight(t *testing.T) {
	fr := &registrymocks.Registrar{}
	nnsc := nnsclient.New(
		nnsclient.WithRegistrar(fr),
	)
	defer nnsc.Close()

	reg := NewRegistry(nnsc, WithDefaultWeight(99))

	info := &hregistry.Info{
		ServiceName: "test-svc",
		Addr:        &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080},
		Weight:      0, // 测试 defaultWeight 兜底
		Tags:        map[string]string{"region": "cn-east", "version": "v1"},
	}

	if err := reg.Register(info); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Deregister(info); err != nil {
		t.Fatalf("Deregister: %v", err)
	}

	if len(fr.Registered) != 1 {
		t.Fatalf("expected 1 Register call, got %d", len(fr.Registered))
	}
	if len(fr.Deregistered) != 1 {
		t.Fatalf("expected 1 Deregister call, got %d", len(fr.Deregistered))
	}

	regInst := fr.Registered[0]
	deregInst := fr.Deregistered[0]

	// 验证 weight(defaultWeight 99 兜底)对称
	if regInst.Weight != 99 {
		t.Errorf("registered weight = %v, want 99 (defaultWeight)", regInst.Weight)
	}
	if deregInst.Weight != 99 {
		t.Errorf("deregistered weight = %v, want 99 (defaultWeight)", deregInst.Weight)
	}

	// 验证 tags 对称保留
	if got := regInst.Metadata; len(got) != 2 || got["region"] != "cn-east" || got["version"] != "v1" {
		t.Errorf("registered tags = %v, want {region:cn-east, version:v1}", got)
	}
	if got := deregInst.Metadata; len(got) != 2 || got["region"] != "cn-east" || got["version"] != "v1" {
		t.Errorf("deregistered tags = %v, want {region:cn-east, version:v1}", got)
	}
}

// TestRegistry_DegradedRegistrar_Fails 回归：非 mock 降级时 Register 曾
// 静默返回 nil（服务不可被发现且无信号）。钉住：降级显式报错。
func TestRegistry_DegradedRegistrar_Fails(t *testing.T) {
	c := nnsclient.New()
	if c.MockMode() {
		t.Fatal("expected non-mock degraded client")
	}
	r := NewRegistry(c)

	addr, _ := net.ResolveTCPAddr("tcp", "10.0.0.5:8080")
	info := &hregistry.Info{ServiceName: "echo-svc", Addr: addr}

	if err := r.Register(info); err == nil {
		t.Error("Register on degraded (non-mock) client must fail loudly, got nil")
	}
	if err := r.Deregister(info); err == nil {
		t.Error("Deregister on degraded (non-mock) client must fail loudly, got nil")
	}
}

// TestNewRegistry_NilClientPanics 钉住：nil client 在**构造期** panic
// （与同包 NewInstanceWatcher 的 nil discoverer 口径一致），不拖到运行期
// 空指针。
func TestNewRegistry_NilClientPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewRegistry(nil) must panic (构造期配置错误)")
		}
	}()
	NewRegistry(nil)
}

// TestRegistry_RegisterContext_PassesCtx 钉住 ctx 透传：此前 Register/
// Deregister 固定用 context.Background()，调用方的取消/超时丢失。
// 迁移位：新增 RegisterContext/DeregisterContext（Hertz 的 registry.Registry
// 接口没有 ctx 形参，故旧签名保留并委托给 ctx 版本）。
func TestRegistry_RegisterContext_PassesCtx(t *testing.T) {
	fr := &ctxRecordingRegistrar{}
	nnsc := nnsclient.New(nnsclient.WithRegistrar(fr))
	defer nnsc.Close()

	reg := NewRegistry(nnsc)

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "v")
	info := &hregistry.Info{
		ServiceName: "ctx-svc",
		Addr:        &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080},
	}

	if err := reg.RegisterContext(ctx, info); err != nil {
		t.Fatalf("RegisterContext: %v", err)
	}
	if err := reg.DeregisterContext(ctx, info); err != nil {
		t.Fatalf("DeregisterContext: %v", err)
	}

	regCtx, deregCtx := fr.snapshot()
	if regCtx == nil {
		t.Fatal("RegisterContext must reach the registrar with a ctx")
	}
	if got := regCtx.Value(ctxKey{}); got != "v" {
		t.Errorf("register ctx value = %v, want \"v\"", got)
	}
	if deregCtx == nil {
		t.Fatal("DeregisterContext must reach the registrar with a ctx")
	}
	if got := deregCtx.Value(ctxKey{}); got != "v" {
		t.Errorf("deregister ctx value = %v, want \"v\"", got)
	}
}

// TestRegistry_ImplementsContextRegistry 静态断言：Registry 同时满足 hertz
// 的 registry.Registry 与本包的 ContextRegistry（带 ctx 的扩展点）。
func TestRegistry_ImplementsContextRegistry(t *testing.T) {
	var _ hregistry.Registry = (*Registry)(nil)
	var _ ContextRegistry = (*Registry)(nil)
}
