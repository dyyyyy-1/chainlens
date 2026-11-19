package api

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rateLimitWindow 是限流的时间窗口。
const rateLimitWindow = time.Minute

// maxTrackedClients 是限流表的容量上限。
//
// 超限时清理过期条目，避免被随机源地址撑爆内存。这不是精确的 LRU，
// 但足以挡住「大量一次性 IP」这类低成本攻击。
const maxTrackedClients = 4096

// rateLimiter 是按来源 IP 的固定窗口计数器。
//
// 选固定窗口而不是令牌桶：这里的目的不是平滑流量，而是让单个客户端
// 不能把连接池占满。固定窗口实现简单、无状态漂移，够用。
type rateLimiter struct {
	limit  int
	mu     sync.Mutex
	counts map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute <= 0 {
		return nil
	}
	return &rateLimiter{limit: perMinute, counts: make(map[string]*rateWindow)}
}

// allow 判断该来源是否放行，并在拒绝时给出建议的重试等待时间。
func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.counts) >= maxTrackedClients {
		for k, window := range l.counts {
			if now.Sub(window.start) >= rateLimitWindow {
				delete(l.counts, k)
			}
		}
	}

	window, seen := l.counts[key]
	if !seen || now.Sub(window.start) >= rateLimitWindow {
		l.counts[key] = &rateWindow{start: now, count: 1}
		return true, 0
	}

	if window.count >= l.limit {
		return false, rateLimitWindow - now.Sub(window.start)
	}

	window.count++
	return true, 0
}

// rateLimited 把限流器套在路由外层。
func rateLimited(limiter *rateLimiter, next http.Handler) http.Handler {
	if limiter == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed, retryAfter := limiter.allow(clientIP(r), time.Now())
		if !allowed {
			seconds := int(retryAfter.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				"too many requests from this client, retry later")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP 取连接对端地址。
//
// 刻意不读 X-Forwarded-For：本服务没有可信的反向代理，该头可被任意伪造，
// 用它做限流等于把开关交给调用方。将来真放到代理后面，要改成读代理写入的、
// 且只信任已知代理来源的那个头。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
