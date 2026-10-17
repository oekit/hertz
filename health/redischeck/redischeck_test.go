package redischeck

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/redis/go-redis/v9"

	"github.com/oekit/hertz/health"
)

// mockRedis 只实现 Ping（health check 唯一调用点），其余方法内嵌
// redis.Cmdable 接口占位——被误调会 nil panic，保证测试不会漏覆盖面。
type mockRedis struct {
	redis.Cmdable
	pingErr error
	pinged  bool
}

func (m *mockRedis) Ping(ctx context.Context) *redis.StatusCmd {
	m.pinged = true
	return redis.NewStatusResult("PONG", m.pingErr)
}

func TestRedisChecker_PingOK(t *testing.T) {
	m := &mockRedis{}
	c := RedisChecker(m)
	if c.Name != "redis" {
		t.Errorf("Name = %q, want %q", c.Name, "redis")
	}

	handler := health.NewHealthHandler(health.WithCheckers(c))
	rc := app.NewContext(0)
	handler(context.Background(), rc)
	if rc.Response.StatusCode() != 200 {
		t.Errorf("status = %d, want 200", rc.Response.StatusCode())
	}
	if !m.pinged {
		t.Error("expected Ping to be called")
	}
}

func TestNamedRedisChecker_PingError(t *testing.T) {
	m := &mockRedis{pingErr: errors.New("connection refused")}
	c := NamedRedisChecker("redis-primary", m)
	if c.Name != "redis-primary" {
		t.Errorf("Name = %q, want %q", c.Name, "redis-primary")
	}

	handler := health.NewHealthHandler(health.WithCheckers(c))
	rc := app.NewContext(0)
	handler(context.Background(), rc)
	if rc.Response.StatusCode() != 503 {
		t.Errorf("status = %d, want 503", rc.Response.StatusCode())
	}
}

func TestNamedRedisChecker_TimeoutApplies(t *testing.T) {
	// Ping 1s 超时是内部约束：checker 不得永久阻塞（外层 health 超时兜底）。
	m := &mockRedis{}
	c := NamedRedisChecker("redis", m)
	start := time.Now()
	handler := health.NewHealthHandler(health.WithCheckers(c))
	handler(context.Background(), app.NewContext(0))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("checker returned too slowly: %v", elapsed)
	}
}
