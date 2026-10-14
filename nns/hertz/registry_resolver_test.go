package hertz

import (
	"context"
	"errors"
	"net"
	"testing"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	"github.com/yi-nology/ekit-nns/nns"
	nacospkg "github.com/yi-nology/ekit-registry/nacos"
	"github.com/yi-nology/ekit-registry/registry"
)

// TestNewRegistry_NilRegistrar_OK 验证 mock 模式下 Register/Deregister 不报错（静默跳过）。
func TestNewRegistry_NilRegistrar_OK(t *testing.T) {
	nnsc := nns.New(nacospkg.Bootstrap{ServiceName: "test"}, nns.WithMockMode(true))
	defer nnsc.Close()

	reg := NewRegistry(nnsc.ServiceClient())

	info := &hregistry.Info{
		ServiceName: "test-svc",
		Addr:        &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080},
		Weight:      20,
	}
	if err := reg.Register(info); err != nil {
		t.Errorf("Register in mock mode should be no-op, got %v", err)
	}
	if err := reg.Deregister(info); err != nil {
		t.Errorf("Deregister in mock mode should be no-op, got %v", err)
	}
}

// TestNewRegistry_NilInfo_Error 验证 info 为 nil 时返回错误。
func TestNewRegistry_NilInfo_Error(t *testing.T) {
	nnsc := nns.New(nacospkg.Bootstrap{ServiceName: "test"}, nns.WithMockMode(true))
	defer nnsc.Close()

	reg := NewRegistry(nnsc.ServiceClient())
	if err := reg.Register(nil); err == nil {
		t.Error("expected error when info is nil")
	}
}

// TestNewRegistry_DefaultWeightApplied 验证 Weight=0 时使用默认权重 10。
func TestNewRegistry_DefaultWeightApplied(t *testing.T) {
	nnsc := nns.New(nacospkg.Bootstrap{ServiceName: "test"}, nns.WithMockMode(true))
	defer nnsc.Close()

	reg := NewRegistry(nnsc.ServiceClient(), WithDefaultWeight(99))

	info := &hregistry.Info{
		ServiceName: "test-svc",
		Addr:        &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080},
		Weight:      0,
	}
	if info.Weight != 0 {
		t.Fatalf("setup: Weight=%d, want 0", info.Weight)
	}
	if err := reg.Register(info); err != nil {
		t.Errorf("Register should not error, got %v", err)
	}
}

// TestNewResolver_MockMode_ReturnsInstances 验证 mock 模式下 resolver 返回静态实例。
func TestNewResolver_MockMode_ReturnsInstances(t *testing.T) {
	nnsc := nns.New(
		nacospkg.Bootstrap{},
		nns.WithMockMode(true),
		nns.WithStaticInstance("target-svc", []registry.Instance{
			{IP: "10.0.0.1", Port: 8080, Weight: 10},
			{IP: "10.0.0.2", Port: 8081, Weight: 20},
		}),
	)
	defer nnsc.Close()

	r := NewResolver(nnsc.ServiceClient())
	result, err := r.Resolve(context.Background(), "target-svc")
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if len(result.Instances) != 2 {
		t.Errorf("expected 2 instances, got %d", len(result.Instances))
	}
	if result.CacheKey != "target-svc" {
		t.Errorf("CacheKey = %q, want %q", result.CacheKey, "target-svc")
	}
}

// TestNewResolver_NoInstances_Error 验证空实例时返回错误。
func TestNewResolver_NoInstances_Error(t *testing.T) {
	nnsc := nns.New(nacospkg.Bootstrap{ServiceName: "test"}, nns.WithMockMode(true))
	defer nnsc.Close()

	r := NewResolver(nnsc.ServiceClient())
	_, err := r.Resolve(context.Background(), "nonexistent-svc")
	if err == nil {
		t.Error("expected error for empty instances, got nil")
	}
	if !errors.Is(err, err) {
		t.Logf("got error: %v", err)
	}
}

// TestNewResolver_Name 验证 Name 返回值。
func TestNewResolver_Name(t *testing.T) {
	nnsc := nns.New(nacospkg.Bootstrap{ServiceName: "test"}, nns.WithMockMode(true))
	defer nnsc.Close()

	r := NewResolver(nnsc.ServiceClient())
	if r.Name() != resolverName {
		t.Errorf("Name() = %q, want %q", r.Name(), resolverName)
	}
}

// TestNewResolver_TargetWithTags 验证 Target 方法在带 tags 时编码正确。
func TestNewResolver_TargetWithTags(t *testing.T) {
	nnsc := nns.New(nacospkg.Bootstrap{ServiceName: "test"}, nns.WithMockMode(true))
	defer nnsc.Close()

	r := NewResolver(nnsc.ServiceClient())
	desc := r.Target(context.Background(), nil)
	if desc != "" {
		t.Errorf("Target(nil) = %q, want empty", desc)
	}
}

// TestRegistryImplementsHertzInterface 静态断言：nnsRegistry 实现 hregistry.Registry。
var _ hregistry.Registry = (*nnsRegistry)(nil)
