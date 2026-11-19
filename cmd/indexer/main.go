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
	// ABI 拉取失败，退化为内置的 ERC20 事件 ABI。
	abiSourceFallback = "fallback"

	// abiSourceSourcify 表示 ABI 来自 Sourcify 的已验证源码。
	abiSourceSourcify = "sourcify"

	// abiFetchTimeout 是单次 ABI 拉取的超时上限。
	abiFetchTimeout = 15 * time.Second
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

	client, err := chain.New(ctx, cfg.RPCHTTPURL, cfg.ChainID, chain.Options{
		HeaderBatchSize: int(cfg.RPCHeaderBatch),
	})
	if err != nil {
		return err
	}
	defer client.Close()

	contract := common.HexToAddress(cfg.TargetContract)

	// 代理识别 + ABI 拉取只在启动时做一次，结果写入 contract_meta。
	// 任何一步失败都只降级、不中断索引：日志本身不依赖 ABI（HANDOFF.md 8.3）。
	abiJSON, abiSource := resolveContract(ctx, st, client, decoder.NewSourcifyClient(abiFetchTimeout), cfg.ChainID, contract, logger)

	dec, err := decoder.New(abiJSON)
	if err != nil {
		logger.Warn("abi unusable, falling back to builtin ERC20", "abi_source", abiSource, "err", err)
		abiSource = abiSourceFallback
		if dec, err = decoder.New([]byte(decoder.ERC20ABI)); err != nil {
			return fmt.Errorf("indexer: build decoder: %w", err)
		}
	}
	logger.Info("decoder ready", "abi_source", abiSource)

	logger.Info("indexer started",
		"chain_id", cfg.ChainID,
		"contract", cfg.TargetContract,
		"start_block", cfg.StartBlock,
		"confirmations", cfg.Confirmations,
		"batch_blocks", cfg.BatchBlockRange,
		"header_batch", cfg.RPCHeaderBatch,
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

// resolveContract 识别代理实现、拉取 ABI、写入 contract_meta，返回最终使用的 ABI 与来源。
//
// 代理识别范围见 decoder.Detect：EIP-1967 实现槽、EIP-1967 信标槽、ZeppelinOS 遗留槽、
// EIP-1167 最小代理。USDC 属于第三种，用纯 EIP-1967 会漏判（HANDOFF.md 8.2）。
//
// ABI 必须按**实现地址**拉取：代理合约自己验证的源码里只有 AdminChanged / Upgraded，
// 业务事件全在实现上。任何一步失败都记录警告并退化为内置 ABI，绝不阻断索引。
func resolveContract(
	ctx context.Context,
	st *store.Store,
	reader decoder.ChainReader,
	sourcify *decoder.SourcifyClient,
	chainID uint64,
	addr common.Address,
	logger *slog.Logger,
) ([]byte, string) {
	meta := store.ContractMeta{Address: addr, ABISource: abiSourceFallback}
	abiTarget := addr

	info, err := decoder.Detect(ctx, reader, addr)
	if err != nil {
		logger.Warn("proxy detection failed", "address", addr.Hex(), "err", err)
	} else if info.IsProxy() {
		implementation := info.Implementation
		meta.Implementation = &implementation
		abiTarget = implementation
		logger.Info("contract resolved",
			"address", addr.Hex(),
			"proxy_kind", string(info.Kind),
			"implementation", implementation.Hex(),
		)
	} else {
		logger.Info("contract resolved", "address", addr.Hex(), "proxy_kind", string(info.Kind))
	}

	abiJSON, err := sourcify.FetchABI(ctx, chainID, abiTarget)
	if err != nil {
		logger.Warn("abi fetch failed, degrading to builtin ERC20 abi",
			"address", abiTarget.Hex(),
			"err", err,
		)
	} else {
		meta.ABI = abiJSON
		meta.ABISource = abiSourceSourcify
	}

	if err := st.UpsertContractMeta(ctx, meta); err != nil {
		logger.Warn("persist contract meta failed", "address", addr.Hex(), "err", err)
	}

	if len(abiJSON) == 0 {
		return []byte(decoder.ERC20ABI), abiSourceFallback
	}

	return abiJSON, meta.ABISource
}
