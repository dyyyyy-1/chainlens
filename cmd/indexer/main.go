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
	"github.com/dyyyyy-1/chainlens/internal/decoder"
	"github.com/dyyyyy-1/chainlens/internal/indexer"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

const (
	// envFile 是本地配置文件名，已加入 .gitignore。
	envFile = ".env"

	// pollInterval 是追平链头后的轮询间隔。第 10 节的变量模板未定义该项，故取常量。
	pollInterval = 5 * time.Second

	// abiSourceFallback 对应 contract_meta.abi_source 的 fallback 取值：
	// 当前用的是内置 ERC20 ABI，尚未从 Etherscan 获取（W3 接入）。
	abiSourceFallback = "fallback"
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

	// W1D3 阶段使用内置 ERC20 ABI；Etherscan 拉取与本地 ABI 文件加载在 W3 接入。
	dec, err := decoder.New([]byte(decoder.ERC20ABI))
	if err != nil {
		return fmt.Errorf("indexer: build decoder: %w", err)
	}

	contract := common.HexToAddress(cfg.TargetContract)

	// 代理识别只在启动时做一次，结果写入 contract_meta。
	// 失败不阻塞索引：日志本身不依赖 ABI，识别不到只会让解码退化（HANDOFF.md 8.3）。
	if err := resolveContract(ctx, st, client, contract, logger); err != nil {
		logger.Warn("contract resolution skipped", "address", contract.Hex(), "err", err)
	}

	logger.Info("indexer started",
		"chain_id", cfg.ChainID,
		"contract", cfg.TargetContract,
		"start_block", cfg.StartBlock,
		"confirmations", cfg.Confirmations,
		"batch_blocks", cfg.BatchBlockRange,
	)

	scanner := indexer.New(client, client, st, dec, indexer.Config{
		ChainID:       cfg.ChainID,
		Contract:      contract,
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

// resolveContract 识别目标合约的代理实现并写入 contract_meta。
//
// 识别范围见 decoder.Detect：EIP-1967 实现槽、EIP-1967 信标槽、ZeppelinOS 遗留槽、
// EIP-1167 最小代理。USDC 属于第三种，用纯 EIP-1967 会漏判（HANDOFF.md 8.2）。
func resolveContract(ctx context.Context, st *store.Store, reader decoder.ChainReader, addr common.Address, logger *slog.Logger) error {
	info, err := decoder.Detect(ctx, reader, addr)
	if err != nil {
		return err
	}

	meta := store.ContractMeta{Address: addr, ABISource: abiSourceFallback}
	if info.IsProxy() {
		implementation := info.Implementation
		meta.Implementation = &implementation
	}
	if err := st.UpsertContractMeta(ctx, meta); err != nil {
		return err
	}

	logger.Info("contract resolved",
		"address", addr.Hex(),
		"proxy_kind", string(info.Kind),
		"implementation", info.Implementation.Hex(),
	)

	return nil
}
