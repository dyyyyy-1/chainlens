// Command indexer 是 ChainLens 的索引器进程入口：按区间拉取日志、落库并推进游标。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/dyyyyy-1/chainlens/internal/chain"
	"github.com/dyyyyy-1/chainlens/internal/config"
	"github.com/dyyyyy-1/chainlens/internal/indexer"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

const (
	// envFile 是本地配置文件名，已加入 .gitignore。
	envFile = ".env"

	// pollInterval 是追平链头后的轮询间隔。第 10 节的变量模板未定义该项，故取常量。
	pollInterval = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("indexer failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(envFile)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	if err := validate(cfg); err != nil {
		return err
	}

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	client, err := chain.New(ctx, cfg.RPCHTTPURL, cfg.ChainID, chain.Options{})
	if err != nil {
		return err
	}
	defer client.Close()

	logger.Info("indexer started",
		"chain_id", cfg.ChainID,
		"contract", cfg.TargetContract,
		"start_block", cfg.StartBlock,
		"confirmations", cfg.Confirmations,
		"batch_blocks", cfg.BatchBlockRange,
	)

	scanner := indexer.New(client, client, st, indexer.Config{
		ChainID:       cfg.ChainID,
		Contract:      common.HexToAddress(cfg.TargetContract),
		StartBlock:    cfg.StartBlock,
		Confirmations: cfg.Confirmations,
		BatchBlocks:   cfg.BatchBlockRange,
		PollInterval:  pollInterval,
	}, logger)

	return scanner.Run(ctx)
}

// validate 校验索引器必需的配置项，失败即退出而不是带病运行。
func validate(cfg config.Config) error {
	if cfg.DatabaseURL == "" {
		return errors.New("indexer: DATABASE_URL is required")
	}
	if cfg.TargetContract == "" {
		return errors.New("indexer: TARGET_CONTRACT is required")
	}
	if !common.IsHexAddress(cfg.TargetContract) {
		return fmt.Errorf("indexer: invalid TARGET_CONTRACT %q", cfg.TargetContract)
	}
	return nil
}
