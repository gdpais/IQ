package redisx

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

func Open(ctx context.Context, addr string, dbString string) (*redis.Client, error) {
	db, err := strconv.Atoi(dbString)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_DB value: %w", err)
	}

	client := redis.NewClient(&redis.Options{
		Addr: addr,
		DB:   db,
	})

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return client, nil
}
