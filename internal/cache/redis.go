package cache

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// redisStore adapts go-redis v9 to the Store interface.
type redisStore struct {
	client *goredis.Client
	ttl    time.Duration
}

func newRedis(cfg Config) (Store, error) {
	client := goredis.NewClient(&goredis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cache: redis %s unreachable: %w", cfg.Addr, err)
	}
	return &redisStore{client: client, ttl: cfg.TTL}, nil
}

func (s *redisStore) Get(ctx context.Context, key string) (string, bool, error) {
	value, err := s.client.Get(ctx, key).Result()
	if err == goredis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (s *redisStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = s.ttl
	}
	return s.client.Set(ctx, key, value, ttl).Err()
}

func (s *redisStore) Delete(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}

func (s *redisStore) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = s.ttl
	}
	return s.client.SetNX(ctx, key, value, ttl).Result()
}

func (s *redisStore) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }
func (s *redisStore) Close() error                   { return s.client.Close() }
