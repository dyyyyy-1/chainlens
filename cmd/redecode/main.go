// Command redecode 用当前 ABI 重新解码存量的 undecodable 日志。
//
// 与 SESSION-HANDOFF §8 第 1 条描述的「DELETE + 拨游标 + 重跑索引器」不同：
// 那条路要重新从链上拉日志，而存量的未解码日志早已滑出免费端点的归档窗口，
// 必须切归档端点、按 2.5 块/秒重扫数万块。实际上原始日志一直躺在 chain_log 里，
// 缺的只是「用现在的 ABI 再解一次」——不需要 RPC，也不该动游标。
//
// 因此本命令：
//   - 只读 chain_log、只写 decoded_event（覆盖）；
//   - 不碰 index_cursor，不影响索引器续跑；
//   - 按块区间分批提交，中断后可重入（未被覆盖的行仍是 undecodable，下次继续选中）；
//   - 默认 dry-run，必须显式 -apply 才写库。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/dyyyyy-1/chainlens/internal/config"
	"github.com/dyyyyy-1/chainlens/internal/decoder"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

const (
	// envFile 是本地配置文件名，已加入 .gitignore。
	envFile = ".env"

	// defaultBatchBlocks 是每批覆盖的块数。
	// 取值与 cmd/verify 一致：200 块在 USDC 上约一两万条日志，单批提交足够快。
	defaultBatchBlocks = 200

	// maxBatchBlocks 防止误传一个巨大区间把整段历史拉进内存。
	maxBatchBlocks = 2000
)

func main() {
	apply := flag.Bool("apply", false, "真正写库；不加此开关时只统计，不修改任何数据")
	from := flag.Uint64("from", 0, "起始块（含）。为 0 时自动取存量未解码日志的最小块号")
	to := flag.Uint64("to", 0, "结束块（含）。为 0 时自动取存量未解码日志的最大块号")
	batch := flag.Uint64("batch", defaultBatchBlocks, "每批覆盖的块数")
	flag.Parse()

	if err := run(*apply, *from, *to, *batch); err != nil {
		slog.Error("redecode failed", "err", err)
		os.Exit(1)
	}
}

func run(apply bool, from, to, batch uint64) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if batch == 0 || batch > maxBatchBlocks {
		return fmt.Errorf("redecode: batch must be within 1..%d, got %d", maxBatchBlocks, batch)
	}
	if from != 0 && to != 0 && from > to {
		return fmt.Errorf("redecode: from %d must not be greater than to %d", from, to)
	}

	// 用 LoadServer 而不是 Load：本命令不访问链，不应该因为 RPC_HTTP_URL 缺失而拒绝启动。
	cfg, err := config.LoadServer(envFile)
	if err != nil {
		return err
	}
	if cfg.DatabaseURL == "" {
		return errors.New("redecode: DATABASE_URL is required")
	}
	if len(cfg.TargetContracts) == 0 {
		return errors.New("redecode: TARGET_CONTRACTS（或 TARGET_CONTRACT）is required，用它定位 contract_meta 里的 ABI")
	}
	for _, raw := range cfg.TargetContracts {
		if !common.IsHexAddress(raw) {
			return fmt.Errorf("redecode: invalid contract address %q", raw)
		}
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	if !apply {
		fmt.Println("模式：dry-run（不写库）。加 -apply 才会覆盖 decoded_event。")
	} else {
		fmt.Println("模式：apply（覆盖写 decoded_event，不修改 chain_log 与 index_cursor）")
	}
	fmt.Printf("每批 %d 块\n", batch)

	for _, raw := range cfg.TargetContracts {
		if err := redecodeContract(ctx, st, common.HexToAddress(raw), apply, from, to, batch); err != nil {
			return err
		}
	}

	if !apply {
		fmt.Println("dry-run 未写库。确认数字合理后加 -apply 重跑。")
	}
	return nil
}

// loadDecoder 按合约取 contract_meta 里的 ABI 构建解码器。
//
// 不用内置 ERC20 兜底：重解的意义就是「用比当初更完整的 ABI 再解一次」，
// 悄悄退回内置 ABI 会让人以为重解生效了，实际什么也没变。
func loadDecoder(ctx context.Context, st *store.Store, contract common.Address) (*decoder.Decoder, error) {
	meta, ok, err := st.LoadContractMeta(ctx, contract)
	if err != nil {
		return nil, err
	}
	if !ok || len(meta.ABI) == 0 {
		return nil, fmt.Errorf("redecode: contract_meta 里没有 %s 的 ABI，先跑一次索引器再重试", contract.Hex())
	}

	dec, err := decoder.New(meta.ABI)
	if err != nil {
		return nil, fmt.Errorf("redecode: build decoder from %s abi (source=%s): %w", contract.Hex(), meta.ABISource, err)
	}
	return dec, nil
}

// redecodeContract 重解单个合约的存量未解码日志。
//
// 按合约分开是必须的：每个合约的 ABI 不同，拿 USDC 的 ABI 去解另一个代币的日志，
// 结果只会是「全部 undecodable」，看起来像重解失败。
func redecodeContract(
	ctx context.Context,
	st *store.Store,
	contract common.Address,
	apply bool,
	from, to, batch uint64,
) error {
	summary, err := st.UndecodableSummary(ctx, contract)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s：存量未解码 %d 条，涉及 %d 个块，区间 %d..%d\n",
		contract.Hex(), summary.Rows, summary.Blocks, summary.MinBlock, summary.MaxBlock)
	if summary.Rows == 0 {
		return nil
	}

	if from == 0 {
		from = summary.MinBlock
	}
	if to == 0 {
		to = summary.MaxBlock
	}

	dec, err := loadDecoder(ctx, st, contract)
	if err != nil {
		return err
	}

	stats, err := redecodeRange(ctx, st, dec, contract, from, to, batch, apply)
	if err != nil {
		return err
	}

	fmt.Printf("%s 汇总：扫描 %d 条，可解 %d 条，仍不可解 %d 条\n",
		contract.Hex(), stats.Scanned, stats.Decoded, stats.StillUndecodable)

	if !apply {
		return nil
	}

	after, err := st.UndecodableSummary(ctx, contract)
	if err != nil {
		return err
	}
	fmt.Printf("%s 写库后存量未解码：%d 条（原 %d 条）\n", contract.Hex(), after.Rows, summary.Rows)
	return nil
}

// redecodeStats 汇总一次重解的计数。
type redecodeStats struct {
	Scanned          int
	Decoded          int
	StillUndecodable int
}

// redecodeRange 按块区间分批重解。
//
// 每批独立事务：中途失败时已提交的批次保留，重跑会自然跳过（已解出的行不再被选中），
// 因此不需要额外的断点文件。
func redecodeRange(
	ctx context.Context,
	st *store.Store,
	dec *decoder.Decoder,
	contract common.Address,
	from, to, batch uint64,
	apply bool,
) (redecodeStats, error) {
	var stats redecodeStats

	for start := from; start <= to; start += batch {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		end := min(start+batch-1, to)

		records, err := st.UndecodableLogs(ctx, start, end, contract)
		if err != nil {
			return stats, err
		}
		if len(records) == 0 {
			continue
		}

		decoded := make([]store.DecodedRecord, 0, len(records))
		batchDecoded, batchStuck := 0, 0

		for _, record := range records {
			event := dec.Decode(toTypesLog(record))
			stats.Scanned++

			if !event.IsDecoded() {
				batchStuck++
				stats.StillUndecodable++
				continue
			}

			batchDecoded++
			stats.Decoded++
			decoded = append(decoded, store.DecodedRecord{
				TxHash:    event.TxHash,
				LogIndex:  event.LogIndex,
				EventName: event.Name,
				Status:    event.Status,
				Payload:   event.Payload,
			})
		}

		if apply && len(decoded) > 0 {
			if err := applyBatch(ctx, st, decoded); err != nil {
				return stats, fmt.Errorf("redecode: blocks %d..%d: %w", start, end, err)
			}
		}

		fmt.Printf("块 %d..%d：未解码 %d 条，其中可解 %d 条，仍不可解 %d 条%s\n",
			start, end, len(records), batchDecoded, batchStuck, appliedSuffix(apply))
	}

	return stats, nil
}

// applyBatch 在一个事务里覆盖写一批解码结果。
func applyBatch(ctx context.Context, st *store.Store, records []store.DecodedRecord) error {
	tx, err := st.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := st.UpsertDecoded(ctx, tx, records); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appliedSuffix(apply bool) string {
	if apply {
		return "，已写入"
	}
	return ""
}

// toTypesLog 把库内的行还原成 decoder 需要的 types.Log。
//
// topics 必须保持前缀连续：链上日志不会出现「有 topic2 却没有 topic1」的情况，
// 遇到空位就应停止，而不是继续往后拼接。
func toTypesLog(record store.LogRecord) types.Log {
	entry := types.Log{
		Address:     record.Contract,
		Data:        record.Data,
		BlockNumber: record.BlockNumber,
		TxHash:      record.TxHash,
		Index:       uint(record.LogIndex),
	}

	for _, topic := range []*common.Hash{record.Topic0, record.Topic1, record.Topic2, record.Topic3} {
		if topic == nil {
			break
		}
		entry.Topics = append(entry.Topics, *topic)
	}

	return entry
}
