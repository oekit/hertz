package nns

import (
	"net"
	"testing"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	nnsclient "github.com/oekit/nns/client"
	registrymocks "github.com/oekit/registry/registrymocks"
)

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
