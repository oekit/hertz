// Package redischeck 提供 Redis 健康检查器（go-redis 适配）。
//
// 从 health 根包拆出：根包只保留核心能力（stdlib + hertz），
// 本子包单独承载 go-redis 依赖——非 Redis 消费者 import health
// 不会传递引入 go-redis。
//
// 用法:
//
//	import (
//	    "github.com/oekit/hertz/health"
//	    "github.com/oekit/hertz/health/redischeck"
//	)
//
//	health.RegisterHealthRoutes(r, health.WithCheckers(
//	    redischeck.NamedRedisChecker("redis", redisClient),
//	    redischeck.RedisChecker(redisClient), // 默认名称 "redis"
//	))
package redischeck

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/oekit/hertz/health"
)

// redisCheckTimeout 是单次 Redis PING 的超时。
const redisCheckTimeout = 1 * time.Second

// NamedRedisChecker 创建带名称的 Redis 健康检查函数。
// 1s 超时的 PING：超时或连接错误返回 error（/health 硬失败路径）。
// 软依赖场景请自行包装为 health.Degraded(err)。
func NamedRedisChecker(name string, client redis.Cmdable) *health.Checker {
	return health.NamedChecker(name, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, redisCheckTimeout)
		defer cancel()
		return client.Ping(ctx).Err()
	})
}

// RedisChecker 创建 Redis 健康检查函数（默认名称 "redis"）。
func RedisChecker(client redis.Cmdable) *health.Checker {
	return NamedRedisChecker("redis", client)
}
