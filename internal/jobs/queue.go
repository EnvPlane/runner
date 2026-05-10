package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"

	"envpilot/internal/redisqueue"
)

type Queue interface {
	Enqueue(ctx context.Context, id string) error
	Dequeue(ctx context.Context) (string, bool, error)
	Depth(ctx context.Context) (int, error)
}

type MemoryQueue struct {
	mu     sync.Mutex
	items  []string
	queued map[string]bool
}

func NewMemoryQueue() *MemoryQueue {
	return &MemoryQueue{queued: map[string]bool{}}
}

func (q *MemoryQueue) Enqueue(_ context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("job id is required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued[id] {
		return nil
	}
	q.items = append(q.items, id)
	q.queued[id] = true
	return nil
}

func (q *MemoryQueue) Dequeue(_ context.Context) (string, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return "", false, nil
	}
	id := q.items[0]
	q.items = q.items[1:]
	delete(q.queued, id)
	return id, true, nil
}

func (q *MemoryQueue) Depth(_ context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items), nil
}

type RedisQueue struct {
	queue *redisqueue.Queue
	name  string
}

func NewRedisQueue(queue *redisqueue.Queue, name string) RedisQueue {
	if name == "" {
		name = "jobs"
	}
	return RedisQueue{queue: queue, name: name}
}

func (q RedisQueue) Enqueue(ctx context.Context, id string) error {
	return q.queue.Enqueue(ctx, q.name, []byte(id))
}

func (q RedisQueue) Dequeue(ctx context.Context) (string, bool, error) {
	payload, err := q.queue.DequeueNow(ctx, q.name)
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(payload), true, nil
}

func (q RedisQueue) Depth(ctx context.Context) (int, error) {
	return q.queue.Depth(ctx, q.name)
}
