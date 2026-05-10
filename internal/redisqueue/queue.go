package redisqueue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Queue struct {
	client *redis.Client
	prefix string
}

func New(redisURL string, prefix string) (*Queue, error) {
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "envpilot"
	}
	return &Queue{client: redis.NewClient(options), prefix: prefix}, nil
}

func (q *Queue) Ping(ctx context.Context) error {
	if q == nil || q.client == nil {
		return fmt.Errorf("redis queue is not initialized")
	}
	return q.client.Ping(ctx).Err()
}

func (q *Queue) Close() error {
	if q == nil || q.client == nil {
		return nil
	}
	return q.client.Close()
}

func (q *Queue) Enqueue(ctx context.Context, name string, payload []byte) error {
	if q == nil || q.client == nil {
		return fmt.Errorf("redis queue is not initialized")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("queue name is required")
	}
	if len(payload) == 0 {
		return fmt.Errorf("queue payload is required")
	}
	return q.client.RPush(ctx, q.key(name), payload).Err()
}

func (q *Queue) Dequeue(ctx context.Context, name string, timeout time.Duration) ([]byte, error) {
	if q == nil || q.client == nil {
		return nil, fmt.Errorf("redis queue is not initialized")
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("queue name is required")
	}
	result, err := q.client.BLPop(ctx, timeout, q.key(name)).Result()
	if err != nil {
		return nil, err
	}
	if len(result) != 2 {
		return nil, fmt.Errorf("unexpected redis queue response")
	}
	return []byte(result[1]), nil
}

func (q *Queue) DequeueNow(ctx context.Context, name string) ([]byte, error) {
	if q == nil || q.client == nil {
		return nil, fmt.Errorf("redis queue is not initialized")
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("queue name is required")
	}
	return q.client.LPop(ctx, q.key(name)).Bytes()
}

func (q *Queue) Depth(ctx context.Context, name string) (int, error) {
	if q == nil || q.client == nil {
		return 0, fmt.Errorf("redis queue is not initialized")
	}
	if strings.TrimSpace(name) == "" {
		return 0, fmt.Errorf("queue name is required")
	}
	value, err := q.client.LLen(ctx, q.key(name)).Result()
	return int(value), err
}

func (q *Queue) key(name string) string {
	return q.prefix + ":queue:" + strings.TrimSpace(name)
}
