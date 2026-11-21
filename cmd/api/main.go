// Command api 是 ChainLens 的只读查询服务：把已索引的数据暴露为 HTTP 接口。
//
// 它只读数据库，不访问链、不写数据。索引与查询是两个进程，
// 因此查询服务可以在索引器停机时继续服务（数据会停在最后一次索引的水位）。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dyyyyy-1/chainlens/internal/api"
	"github.com/dyyyyy-1/chainlens/internal/chain"
	"github.com/dyyyyy-1/chainlens/internal/config"
	"github.com/dyyyyy-1/chainlens/internal/profile"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

const (
	// envFile 是本地配置文件名，已加入 .gitignore。
	envFile = ".env"

	// readHeaderTimeout 防慢速请求头占住连接。
	readHeaderTimeout = 10 * time.Second

	// idleTimeout 是 keep-alive 空闲连接的回收时间。
	idleTimeout = 60 * time.Second

	// shutdownTimeout 是优雅退出时等待在途请求的上限。
	shutdownTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("api failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadServer(envFile)
	if err != nil {
		return err
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	if cfg.DatabaseURL == "" {
		return errors.New("api: DATABASE_URL is required")
	}

	// 全进程只建一个连接池。
	//
	// profile 通过 st.Pool() 复用同一个池：若让它自己建池，一个进程里会出现
	// 两套连接上限，数据库侧看到的总连接数是两者之和，且两边的超时与回收
	// 互不知情，排查连接数异常时会误导判断。
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	// 链上余额是可选能力：RPC 与链 ID 都配置了才启用。
	//
	// 连不上也不能让查询服务起不来——画像的主数据（持仓、授权、交互）全在本地库，
	// 余额只是锦上添花。因此这里失败只降级，不返回错误。
	var tokens api.TokenReader
	if cfg.RPCHTTPURL != "" && cfg.ChainID != 0 {
		client, err := chain.New(ctx, cfg.RPCHTTPURL, cfg.ChainID, chain.Options{
			// 启动时只做一次链 ID 校验，用短预算：RPC 不通时不该把启动卡住几分钟。
			RequestTimeout: 5 * time.Second,
			MaxAttempts:    1,
		})
		if err != nil {
			slog.Warn("onchain balance disabled: rpc unavailable", "url", cfg.RPCHTTPURL, "err", err)
		} else {
			defer client.Close()
			tokens = client
		}
	} else {
		slog.Info("onchain balance disabled", "hint", "配置 RPC_HTTP_URL 与 CHAIN_ID 后启用")
	}

	handler := api.NewRouter(api.Deps{
		Events:             st,
		Profile:            profile.New(st.Pool()),
		Logger:             slog.Default(),
		RateLimitPerMinute: int(cfg.HTTPRateLimit),
		APITokens:          cfg.APITokens,
		Tokens:             tokens,
		Metas:              st,
		WebDir:             cfg.WebDir,
	})

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

	if cfg.WebDir == "" {
		slog.Info("frontend not served", "hint", "npm run build 后重试，或用 WEB_DIR 指定目录")
	} else {
		slog.Info("frontend served", "dir", cfg.WebDir)
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", cfg.HTTPAddr)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("api: listen on %s: %w", cfg.HTTPAddr, err)
	case <-ctx.Done():
		slog.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("api: shutdown: %w", err)
	}

	return nil
}
