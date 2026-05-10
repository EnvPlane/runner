package redisqueue

import "testing"

func TestNewQueueDefaultsPrefixAndBuildsStableKeys(t *testing.T) {
	queue, err := New("redis://localhost:6379/3", "")
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}
	t.Cleanup(func() {
		_ = queue.Close()
	})

	if got := queue.key(" jobs "); got != "envpilot:queue:jobs" {
		t.Fatalf("queue key = %q", got)
	}
}

func TestNewQueueRejectsInvalidRedisURL(t *testing.T) {
	if _, err := New("://bad", "envpilot"); err == nil {
		t.Fatal("expected invalid redis url error")
	}
}
