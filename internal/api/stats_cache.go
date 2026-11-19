package api

import (
	"context"
	"sync"
	"time"

	"github.com/dyyyyy-1/chainlens/internal/store"
)

// statsCacheTTL 是统计结果的缓存时长。
//
// 10 秒的取舍：`/api/stats` 的计数是 count(*) 全表聚合，代价随表线性增长；
// 而统计数据本身不需要秒级精确。缓存把「反复刷新面板」的代价从每次一次全表
// 扫描降到每 10 秒一次，同时保证滞后有界。
const statsCacheTTL = 10 * time.Second

// statsCache 给 EventStore 的 Stats 加一层进程内缓存。
//
// 只包 Stats，不包 ListEvents 与 Coverage：事件检索必须返回最新数据，
// 覆盖区间本身就极便宜（两端索引查找），没有缓存的必要。
//
// 没有选择「计数表」是因为它要在批提交事务与 reorg 回滚两处同时维护计数，
// 任何一处漏改都会让统计静默失真，而且没有便宜的办法发现漂移。
// 缓存的失效方式是「过期」，不会累积错误。
type statsCache struct {
	EventStore
	ttl time.Duration

	// now 可在测试中替换，生产环境为零值时用 time.Now。
	now func() time.Time

	mu      sync.Mutex
	stats   store.Stats
	expires time.Time
	primed  bool
}

func withStatsCache(source EventStore, ttl time.Duration) EventStore {
	if source == nil || ttl <= 0 {
		return source
	}
	return &statsCache{EventStore: source, ttl: ttl}
}

// Stats 在缓存有效期内直接返回上次结果。
//
// 整个查询过程持有互斥锁：这不是为了正确性，而是顺带做单飞——统计是重查询，
// 并发打进来时让一个请求去查、其余等待复用结果，比多个请求同时做全表聚合要好。
func (c *statsCache) Stats(ctx context.Context) (store.Stats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.timeNow()
	if c.primed && now.Before(c.expires) {
		return c.stats, nil
	}

	stats, err := c.EventStore.Stats(ctx)
	if err != nil {
		// 失败不进缓存，否则一次瞬时错误会被放大成 10 秒的持续失败。
		return store.Stats{}, err
	}

	c.stats = stats
	c.expires = now.Add(c.ttl)
	c.primed = true

	return stats, nil
}

func (c *statsCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
