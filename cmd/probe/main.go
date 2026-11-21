// Command probe 是 W1D1 的临时验证工具：拉取指定合约一段区间的原始日志并打印，
// 用于确认 topic 布局与解码前的事实层结构。索引器落地后即可删除。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/dyyyyy-1/chainlens/internal/chain"
	"github.com/dyyyyy-1/chainlens/internal/config"
)

const (
	// defaultContract 在 TARGET_CONTRACT 为空时使用：主网 USDC，EIP-1967 代理合约，
	// W2 的代理识别同样以它作为验证对象。
	defaultContract = "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"

	// probeSpan 是探测覆盖的区块跨度。刻意不用 BATCH_BLOCK_RANGE（2000）：
	// 公共节点单次 eth_getLogs 返回上限约 1 万条，而 USDC 2000 个区块会产生数万条 Transfer。
	probeSpan = 50

	// maxDetail 限制逐条打印的日志数，total 与 topic0 直方图始终完整输出。
	maxDetail = 20

	envFile = ".env"
)

func main() {
	if err := run(); err != nil {
		slog.Error("probe failed", "err", err)
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

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	if len(cfg.TargetContracts) == 0 {
		return errors.New("probe: TARGET_CONTRACT（或 TARGET_CONTRACTS）is required")
	}
	addr, err := resolveContract(cfg.TargetContracts[0])
	if err != nil {
		return err
	}

	from := cfg.StartBlock
	to := from + probeSpan - 1

	client, err := chain.New(ctx, cfg.RPCHTTPURL, cfg.ChainID, chain.Options{})
	if err != nil {
		return err
	}
	defer client.Close()

	printTarget(cfg.RPCHTTPURL, cfg.ChainID, addr, from, to)

	latest, err := client.LatestBlockNumber(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nlatest block : %d\n", latest)

	header, err := client.HeaderByNumber(ctx, to)
	if err != nil {
		return err
	}
	printHeader(header)

	logs, err := client.Fetch(ctx, from, to, []common.Address{addr})
	if err != nil {
		return err
	}
	printLogs(logs)

	return nil
}

// resolveContract 优先采用 TARGET_CONTRACT，为空时回落到内置的 USDC 地址。
func resolveContract(configured string) (common.Address, error) {
	raw := configured
	if raw == "" {
		raw = defaultContract
	}
	if !common.IsHexAddress(raw) {
		return common.Address{}, fmt.Errorf("probe: invalid contract address %q", raw)
	}
	return common.HexToAddress(raw), nil
}

func printTarget(rpcURL string, chainID uint64, addr common.Address, from, to uint64) {
	fmt.Println("== target ==")
	fmt.Printf("rpc          : %s\n", rpcURL)
	fmt.Printf("chain_id     : %d\n", chainID)
	fmt.Printf("contract     : %s\n", addr.Hex())
	fmt.Printf("block range  : %d..%d (%d blocks)\n", from, to, to-from+1)
}

func printHeader(header *types.Header) {
	fmt.Println("\n== header ==")
	fmt.Printf("number       : %d\n", header.Number)
	fmt.Printf("hash         : %s\n", header.Hash().Hex())
	fmt.Printf("parent_hash  : %s\n", header.ParentHash.Hex())
	fmt.Printf("block_time   : %s\n", time.Unix(int64(header.Time), 0).Format(time.RFC3339))
}

func printLogs(logs []types.Log) {
	fmt.Println("\n== logs ==")
	fmt.Printf("total        : %d\n", len(logs))

	printTopicHistogram(logs)

	detail := min(len(logs), maxDetail)
	fmt.Printf("\n== detail (first %d of %d) ==\n", detail, len(logs))
	for i, log := range logs[:detail] {
		fmt.Printf("%3d blk=%d tx=%s idx=%d topics=%d data=%dB\n",
			i+1, log.BlockNumber, log.TxHash.Hex(), log.Index, len(log.Topics), len(log.Data))
		for j := range 4 {
			fmt.Printf("      topic%d=%s\n", j, formatTopic(log.Topics, j))
		}
	}
	if len(logs) > detail {
		fmt.Printf("... %d more logs omitted\n", len(logs)-detail)
	}
}

// printTopicHistogram 按 topic0 分组计数。同一签名应聚合到同一行，
// 直方图只有一行就说明过滤条件命中了单一事件类型。
func printTopicHistogram(logs []types.Log) {
	counts := make(map[string]int)
	for _, log := range logs {
		counts[formatTopic(log.Topics, 0)]++
	}

	type bucket struct {
		topic0 string
		count  int
	}
	buckets := make([]bucket, 0, len(counts))
	for topic0, count := range counts {
		buckets = append(buckets, bucket{topic0: topic0, count: count})
	}
	slices.SortFunc(buckets, func(a, b bucket) int {
		if a.count != b.count {
			return b.count - a.count
		}
		return strings.Compare(a.topic0, b.topic0)
	})

	fmt.Println("topic0 histogram:")
	for _, item := range buckets {
		fmt.Printf("  %s  x%d\n", item.topic0, item.count)
	}
}

// formatTopic 返回第 index 个 topic 的十六进制形式，缺失时返回 "-"。
func formatTopic(topics []common.Hash, index int) string {
	if index >= len(topics) {
		return "-"
	}
	return topics[index].Hex()
}
