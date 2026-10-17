package registry

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/client/discovery"
	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"
	ereg "github.com/oekit/registry/registry"
	registrymocks "github.com/oekit/registry/registrymocks"
)

func TestNewRegistry(t *testing.T) {
	mock := &registrymocks.Registrar{}
	r := NewRegistry(mock)
	if r == nil {
		t.Fatal("NewRegistry returned nil")
	}
}

func TestRegistry_Register(t *testing.T) {
	mock := &registrymocks.Registrar{}
	r := NewRegistry(mock)

	addr := &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080}
	info := &hregistry.Info{
		ServiceName: "test-service",
		Addr:        addr,
		Weight:      15,
		Tags:        map[string]string{"env": "prod"},
	}

	if err := r.Register(info); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if len(mock.Registered) != 1 {
		t.Fatalf("expected 1 registered, got %d", len(mock.Registered))
	}

	inst := mock.Registered[0]
	if inst.Service != "test-service" {
		t.Errorf("Service = %q, want %q", inst.Service, "test-service")
	}
	if inst.IP != "10.0.0.1" {
		t.Errorf("IP = %q, want %q", inst.IP, "10.0.0.1")
	}
	if inst.Port != 8080 {
		t.Errorf("Port = %d, want %d", inst.Port, 8080)
	}
	if inst.Weight != 15 {
		t.Errorf("Weight = %f, want 15", inst.Weight)
	}
}

func TestRegistry_Register_NilInfo(t *testing.T) {
	mock := &registrymocks.Registrar{}
	r := NewRegistry(mock)

	if err := r.Register(nil); err == nil {
		t.Error("expected error for nil info, got nil")
	}
}

func TestRegistry_Register_RegistrarError(t *testing.T) {
	mock := &registrymocks.Registrar{RegErr: errors.New("register failed")}
	r := NewRegistry(mock)

	addr := &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080}
	info := &hregistry.Info{ServiceName: "svc", Addr: addr}

	if err := r.Register(info); err == nil {
		t.Error("expected error, got nil")
	}
}

func TestRegistry_Deregister(t *testing.T) {
	mock := &registrymocks.Registrar{}
	r := NewRegistry(mock)

	addr := &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080}
	info := &hregistry.Info{ServiceName: "svc", Addr: addr}

	if err := r.Deregister(info); err != nil {
		t.Fatalf("Deregister() error = %v", err)
	}
	if len(mock.Deregistered) != 1 {
		t.Fatalf("expected 1 deregistered, got %d", len(mock.Deregistered))
	}
}

func TestNewResolver(t *testing.T) {
	mock := &registrymocks.Discoverer{}
	r := NewResolver(mock)
	if r == nil {
		t.Fatal("NewResolver returned nil")
	}
}

func TestResolver_Target(t *testing.T) {
	mock := &registrymocks.Discoverer{}
	r := NewResolver(mock)

	// 无 Tags
	got := r.Target(context.Background(), &discovery.TargetInfo{Host: "echo-service"})
	if got != "echo-service" {
		t.Errorf("Target() = %q, want %q", got, "echo-service")
	}

	// 有 Tags
	got = r.Target(context.Background(), &discovery.TargetInfo{
		Host: "echo-service",
		Tags: map[string]string{"env": "prod"},
	})
	// Tags map 遍历顺序不确定，只检查包含关键部分
	if !strings.HasPrefix(got, "echo-service?") {
		t.Errorf("Target() = %q, want prefix %q", got, "echo-service?")
	}
}

func TestResolver_Resolve(t *testing.T) {
	mock := &registrymocks.Discoverer{
		Instances: []ereg.Instance{
			{Service: "echo", IP: "10.0.0.1", Port: 8080, Weight: 10, Metadata: map[string]string{"v": "1"}},
			{Service: "echo", IP: "10.0.0.2", Port: 8081, Weight: 20, Metadata: map[string]string{"v": "2"}},
		},
	}
	r := NewResolver(mock)

	result, err := r.Resolve(context.Background(), "echo")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if result.CacheKey != "echo" {
		t.Errorf("CacheKey = %q, want %q", result.CacheKey, "echo")
	}
	if len(result.Instances) != 2 {
		t.Fatalf("len(Instances) = %d, want 2", len(result.Instances))
	}

	if result.Instances[0].Address().String() != "10.0.0.1:8080" {
		t.Errorf("Instances[0].Address() = %q, want %q", result.Instances[0].Address().String(), "10.0.0.1:8080")
	}
}

func TestResolver_Resolve_Empty(t *testing.T) {
	mock := &registrymocks.Discoverer{Instances: []ereg.Instance{}}
	r := NewResolver(mock)

	_, err := r.Resolve(context.Background(), "echo")
	if err == nil {
		t.Error("expected error for empty instances, got nil")
	}
}

func TestResolver_Resolve_Error(t *testing.T) {
	mock := &registrymocks.Discoverer{Err: errors.New("nacos down")}
	r := NewResolver(mock)

	_, err := r.Resolve(context.Background(), "echo")
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestResolver_Name(t *testing.T) {
	mock := &registrymocks.Discoverer{}
	r := NewResolver(mock)

	if r.Name() != "ekit" {
		t.Errorf("Name() = %q, want %q", r.Name(), "ekit")
	}
}

func TestToInstance(t *testing.T) {
	tests := []struct {
		name          string
		info          *hregistry.Info
		defaultWeight int
		wantService   string
		wantIP        string
		wantPort      int
		wantWeight    float64
		wantErr       bool
	}{
		{
			name:          "normal",
			info:          &hregistry.Info{ServiceName: "svc", Addr: &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 9090}, Weight: 20},
			defaultWeight: 10,
			wantService:   "svc",
			wantIP:        "1.2.3.4",
			wantPort:      9090,
			wantWeight:    20,
		},
		{
			name:          "zero weight uses default",
			info:          &hregistry.Info{ServiceName: "svc", Addr: &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 9090}, Weight: 0},
			defaultWeight: 30,
			wantService:   "svc",
			wantIP:        "1.2.3.4",
			wantPort:      9090,
			wantWeight:    30,
		},
		{
			name:    "nil info",
			info:    nil,
			wantErr: true,
		},
		{
			name:    "nil addr",
			info:    &hregistry.Info{ServiceName: "svc", Addr: nil},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst, err := toInstance(tt.info, tt.defaultWeight)
			if (err != nil) != tt.wantErr {
				t.Fatalf("toInstance() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if inst.Service != tt.wantService {
				t.Errorf("Service = %q, want %q", inst.Service, tt.wantService)
			}
			if inst.IP != tt.wantIP {
				t.Errorf("IP = %q, want %q", inst.IP, tt.wantIP)
			}
			if inst.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", inst.Port, tt.wantPort)
			}
			if inst.Weight != tt.wantWeight {
				t.Errorf("Weight = %f, want %f", inst.Weight, tt.wantWeight)
			}
		})
	}
}
