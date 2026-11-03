package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	ereg "github.com/oekit/registry"
	registrymocks "github.com/oekit/registry/registrymocks"
	"github.com/oekit/registry/sdbridge"
)

// TestResolver_WithFetchErr_CustomErrorFormat 钉住 ResolverOption 不再是死
// 扩展点：WithFetchErr 真的改变拉取失败的错误文案（此前 opts 循环只作用于
// 无可配置字段的 hertzResolver ⇒ 传什么都不生效）。
func TestResolver_WithFetchErr_CustomErrorFormat(t *testing.T) {
	mock := &registrymocks.Discoverer{Err: errors.New("boom")}

	// 自定义格式：PlainFetchErr → "hertz resolver: boom"
	r := NewResolver(mock, WithFetchErr(sdbridge.PlainFetchErr))
	_, err := r.Resolve(context.Background(), "echo")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "hertz resolver: boom") {
		t.Errorf("error = %q, want custom format \"hertz resolver: boom\"", err.Error())
	}

	// 缺省格式（DiscoverFetchErr）带服务名
	r2 := NewResolver(mock)
	_, err2 := r2.Resolve(context.Background(), "echo")
	if err2 == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err2.Error(), "discover") || !strings.Contains(err2.Error(), "echo") {
		t.Errorf("default error = %q, want discover-formatted error", err2.Error())
	}
}

// TestResolver_WithFetchErr_NilIgnored 传 nil 时保持缺省（不 panic）。
func TestResolver_WithFetchErr_NilIgnored(t *testing.T) {
	mock := &registrymocks.Discoverer{Err: errors.New("boom")}
	r := NewResolver(mock, WithFetchErr(nil))
	_, err := r.Resolve(context.Background(), "echo")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "discover") {
		t.Errorf("error = %q, want default format", err.Error())
	}
}

// TestResolver_Resolve_InstancesUnchanged 加了选项不得影响正常解析路径。
func TestResolver_Resolve_InstancesUnchanged(t *testing.T) {
	mock := &registrymocks.Discoverer{
		Instances: []ereg.Instance{
			{Service: "echo", IP: "10.0.0.1", Port: 8080, Weight: 10},
		},
	}
	r := NewResolver(mock, WithFetchErr(sdbridge.PlainFetchErr))
	res, err := r.Resolve(context.Background(), "echo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(res.Instances))
	}
}
