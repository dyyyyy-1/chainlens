// Package api 是查询服务的 HTTP 层：路由、参数校验、错误映射与响应封装。
//
// 本层不含 SQL、不做聚合。数据来自 internal/store（事件、统计、覆盖区间）
// 与 internal/profile（地址画像），api 只负责搬运与校验。
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/dyyyyy-1/chainlens/internal/profile"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

// requestTimeout 是单个请求的总预算。
//
// 比 profile 内部的 statement_timeout（3s）略长：让数据库先超时，
// 这样能返回「查询范围过大」这类可解释的错误，而不是笼统的请求超时。
const requestTimeout = 5 * time.Second

// EventStore 是事件检索与统计所需的读能力，由 internal/store 满足。
// 接口在使用方声明（HANDOFF.md 第 11 节）。
type EventStore interface {
	ListEvents(ctx context.Context, q store.EventQuery) ([]store.Event, error)
	Stats(ctx context.Context) (store.Stats, error)
	Coverage(ctx context.Context) (store.Coverage, error)
}

// ProfileReader 是地址画像能力，由 internal/profile 满足。
type ProfileReader interface {
	Holdings(ctx context.Context, addr common.Address, limit int) ([]profile.Holding, error)
	Approvals(ctx context.Context, addr common.Address, limit int) ([]profile.Approval, error)
	Interactions(ctx context.Context, addr common.Address, limit int) ([]profile.Interaction, error)
}

// Deps 是路由所需的依赖集合。
type Deps struct {
	Events  EventStore
	Profile ProfileReader
	Logger  *slog.Logger
	// RateLimitPerMinute 是每个来源 IP 每分钟允许的请求数；<=0 表示不限流。
	RateLimitPerMinute int
	// APITokens 非空时要求 Bearer token；为空表示不启用鉴权。
	// 多个口令用于轮换，任意一个匹配即通过。
	APITokens []string
	// Tokens 非空时 /balances 附带链上余额；为空表示未配置 RPC，
	// 响应里 onchain.enabled=false。
	Tokens TokenReader
	// WebDir 非空时在该目录托管前端构建产物（web/dist），并做 SPA 回退。
	WebDir string

	// balance 由 withBalance 注入，未配置 Tokens 时为 nil。
	balance *balanceService
}

// NewRouter 组装路由与中间件。
//
// 路由用标准库 ServeMux 的方法 + 路径参数模式（Go 1.22+），足以覆盖本服务的
// 五条静态路由。HANDOFF.md 第 5 节写的是 chi，这里没有引入：为一个 router
// 增加一条直接依赖不划算，差异已登记在 docs/SESSION-HANDOFF.md §5.1。
func NewRouter(d Deps) http.Handler {
	// 先包缓存，再注册 handler：handler 是绑定在 d 上的方法值，
	// 注册之后再改 d 不会影响已绑定的方法接收者。
	d = d.withCachedStats()
	d = d.withBalance()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/events", d.handleEvents)
	mux.HandleFunc("GET /api/address/{addr}/balances", d.handleBalances)
	mux.HandleFunc("GET /api/address/{addr}/approvals", d.handleApprovals)
	mux.HandleFunc("GET /api/address/{addr}/interactions", d.handleInteractions)
	mux.HandleFunc("GET /api/stats", d.handleStats)

	if d.WebDir != "" {
		mux.Handle("GET /", staticHandler(d.WebDir))
	}

	handler := authRequired(d.APITokens, mux)
	return d.observe(rateLimited(newRateLimiter(d.RateLimitPerMinute), handler))
}

// withCachedStats 返回一个把 /api/stats 计数缓存起来的 Deps。
func (d Deps) withCachedStats() Deps {
	d.Events = withStatsCache(d.Events, statsCacheTTL)
	return d
}

// withBalance 在配置了 RPC 读出能力时构造余额服务。
//
// 未配置就保持 nil，handler 据此回 enabled=false——把「没有这个能力」
// 与「查不到余额」区分开，两者对调用方的含义完全不同。
func (d Deps) withBalance() Deps {
	if d.Tokens == nil {
		return d
	}
	d.balance = newBalanceService(d.Tokens)
	return d
}

// observe 统一处理请求超时、panic 恢复与访问日志。
func (d Deps) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if recovered := recover(); recovered != nil {
				// panic 不能把堆栈或 SQL 片段回给客户端，只落日志。
				d.logger().Error("handler panic", "path", r.URL.Path, "panic", recovered)
				writeError(recorder, http.StatusInternalServerError, "internal", "internal error")
			}

			// token 指纹只标识「哪个口令」，不落明文。没有 Authorization 头时省略该字段。
			attributes := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", recorder.status,
				"elapsed_ms", time.Since(start).Milliseconds(),
			}
			if fingerprint := tokenFingerprint(r); fingerprint != "" {
				attributes = append(attributes, "token", fingerprint)
			}
			d.logger().Info("request", attributes...)
		}()

		next.ServeHTTP(recorder, r.WithContext(ctx))
	})
}

func (d Deps) logger() *slog.Logger {
	if d.Logger == nil {
		return slog.Default()
	}
	return d.Logger
}

// statusRecorder 记录实际写出的状态码，供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
