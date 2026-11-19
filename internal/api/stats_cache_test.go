package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dyyyyy-1/chainlens/internal/store"
)

func TestStatsCacheReusesWithinTTL(t *testing.T) {
	source := &fakeEvents{stats: store.Stats{TotalLogs: 42}}
	cache := withStatsCache(source, time.Minute).(*statsCache)

	now := time.Date(2026, 11, 19, 13, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }

	for i := range 3 {
		stats, err := cache.Stats(context.Background())
		if err != nil {
			t.Fatalf("第 %d 次调用失败：%v", i+1, err)
		}
		if stats.TotalLogs != 42 {
			t.Fatalf("第 %d 次结果 = %d，want 42", i+1, stats.TotalLogs)
		}
	}

	if source.statsCalls != 1 {
		t.Fatalf("缓存期内底层被调用 %d 次，want 1", source.statsCalls)
	}
}

func TestStatsCacheRefreshesAfterTTL(t *testing.T) {
	source := &fakeEvents{stats: store.Stats{TotalLogs: 1}}
	cache := withStatsCache(source, 10*time.Second).(*statsCache)

	now := time.Date(2026, 11, 19, 13, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }

	if _, err := cache.Stats(context.Background()); err != nil {
		t.Fatalf("首次调用失败：%v", err)
	}

	// 恰好过期：应重新查询。
	now = now.Add(10 * time.Second)
	source.stats = store.Stats{TotalLogs: 2}
	stats, err := cache.Stats(context.Background())
	if err != nil {
		t.Fatalf("过期后调用失败：%v", err)
	}
	if stats.TotalLogs != 2 {
		t.Fatalf("过期后应拿到新值，得到 %d", stats.TotalLogs)
	}
	if source.statsCalls != 2 {
		t.Fatalf("底层调用次数 = %d, want 2", source.statsCalls)
	}
}

func TestStatsCacheDoesNotCacheErrors(t *testing.T) {
	// 一次瞬时错误被缓存住，会放大成持续一个 TTL 的失败。
	source := &fakeEvents{statsErr: errors.New("db down")}
	cache := withStatsCache(source, time.Minute).(*statsCache)

	now := time.Date(2026, 11, 19, 13, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }

	for i := range 2 {
		if _, err := cache.Stats(context.Background()); err == nil {
			t.Fatalf("第 %d 次调用应返回错误", i+1)
		}
	}

	if source.statsCalls != 2 {
		t.Fatalf("错误不应被缓存，底层调用次数 = %d, want 2", source.statsCalls)
	}
}

func TestStatsCacheDisabledWhenTTLZero(t *testing.T) {
	source := &fakeEvents{stats: store.Stats{TotalLogs: 7}}
	cached := withStatsCache(source, 0)

	if _, ok := cached.(*statsCache); ok {
		t.Fatal("ttl<=0 时不应包装缓存")
	}

	for range 2 {
		if _, err := cached.Stats(context.Background()); err != nil {
			t.Fatalf("调用失败：%v", err)
		}
	}
	if source.statsCalls != 2 {
		t.Fatalf("未启用缓存时底层应每次都被调用，实际 %d 次", source.statsCalls)
	}
}
