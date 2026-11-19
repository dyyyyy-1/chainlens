// Command verify 用链上日志回放核对索引结果。
//
// 做法：对同一区间重新发起 eth_getLogs，与库里 chain_log 按 (tx_hash, log_index)
// 逐条比对，报告「链上有库里没有」「库里有链上没有」「字段不一致」三类差异，
// 并用块头校验 block_time。
//
// 它与索引器共用 chain 与 store 两个包，但不共用写入路径：比对的是「链上真相」
// 与「落库结果」，因此能证明索引器忠实记录；它证明不了「我们对合约语义的理解
// 与第三方一致」——后者必须有第三方数据源，Etherscan 需 API key、Blockscout
// 在本机网络不可达，故本项目未做第三方交叉（详见 README 的局限一节）。
//
// 免费端点只保留最近约 8,000 块，工具会自动探测归档边界并只核对可回放的部分，
// 不会因为整批被拒而误报成「数据缺失」。
//
// 退出码：0 无差异，1 存在差异，2 运行失败。
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/dyyyyy-1/chainlens/internal/chain"
	"github.com/dyyyyy-1/chainlens/internal/config"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

const (
	// envFile 是本地配置文件名，已加入 .gitignore。
	envFile = ".env"

	// defaultBatch 是一批核对的区块数。
	//
	// 取 500 而不是 chain.MaxBlockRange（2000）：一批要把链上日志与库里日志
	// 同时装进内存，USDC 每 500 块约 5 万条，两份各约 10 MB；2000 块会让
	// 单批占用接近 100 MB，没有必要。
	defaultBatch = 500

	// archiveMarker 是免费端点在区块超出归档窗口时返回的错误片段。
	archiveMarker = "Archive requests require a personal token"
)

func main() {
	os.Exit(run())
}

func run() int {
	from := flag.Uint64("from", 0, "起始区块，缺省取库内最早区块")
	to := flag.Uint64("to", 0, "结束区块，缺省取库内最新区块")
	batch := flag.Uint64("batch", defaultBatch, "每批核对的区块数，上限 2000")
	contractFlag := flag.String("contract", "", "目标合约地址，缺省取 .env 的 TARGET_CONTRACT")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(envFile)
	if err != nil {
		slog.Error("verify failed", "stage", "config", "err", err)
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	if *batch == 0 || *batch > chain.MaxBlockRange {
		fmt.Fprintf(os.Stderr, "batch 必须在 1..%d 之间，当前 %d\n", chain.MaxBlockRange, *batch)
		return 2
	}

	contract := cfg.TargetContract
	if *contractFlag != "" {
		contract = *contractFlag
	}
	if !common.IsHexAddress(contract) {
		fmt.Fprintf(os.Stderr, "无效的合约地址 %q\n", contract)
		return 2
	}
	address := common.HexToAddress(contract)

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("verify failed", "stage", "database", "err", err)
		return 2
	}
	defer st.Close()

	client, err := chain.New(ctx, cfg.RPCHTTPURL, cfg.ChainID, chain.Options{
		// 与索引器读同一个配置：用 drpc 核对补缺口那段数据时，
		// 块头批量必须降到 3，否则对拍工具自己会先失败。
		HeaderBatchSize: int(cfg.RPCHeaderBatch),
	})
	if err != nil {
		slog.Error("verify failed", "stage", "rpc", "err", err)
		return 2
	}
	defer client.Close()

	coverage, err := st.Coverage(ctx)
	if err != nil {
		slog.Error("verify failed", "stage", "coverage", "err", err)
		return 2
	}
	if coverage.ToBlock == 0 {
		fmt.Fprintln(os.Stderr, "库里没有数据，先跑索引器")
		return 2
	}

	start, end, err := resolveRange(*from, *to, coverage)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	head, err := client.LatestBlockNumber(ctx)
	if err != nil {
		slog.Error("verify failed", "stage", "latest block", "err", err)
		return 2
	}

	fmt.Printf("库内区间 %d..%d，链头 %d\n", start, end, head)

	boundary, err := oldestServable(ctx, client, start, end, address)
	if err != nil {
		slog.Error("verify failed", "stage", "probe archive window", "err", err)
		return 2
	}
	if boundary > start {
		fmt.Printf("归档窗口从 %d 开始，跳过 %d 块（这些块无法回放，不参与判定）\n", boundary, boundary-start)
		start = boundary
	}
	if start > end {
		fmt.Fprintln(os.Stderr, "没有可回放的区间：全部超出免费端点的归档窗口")
		return 2
	}

	fmt.Printf("开始核对 %d..%d，每批 %d 块\n", start, end, *batch)

	result := verifyRange(ctx, st, client, address, start, end, *batch)
	printSummary(result)

	if result.ok() {
		fmt.Println("结果：一致")
		return 0
	}
	fmt.Println("结果：存在差异")
	return 1
}

// verifyRange 分批比对，边比边打印。
func verifyRange(
	ctx context.Context,
	st *store.Store,
	client *chain.Client,
	address common.Address,
	start, end, batch uint64,
) result {
	var total result
	timeCheck := true

	for from := start; from <= end; from += batch {
		to := min(from+batch-1, end)

		chainLogs, err := client.Fetch(ctx, from, to, address)
		if err != nil {
			fmt.Fprintf(os.Stderr, "拉取 %d..%d 失败：%v\n", from, to, err)
			total.failed = true
			return total
		}

		dbLogs, err := st.LogsByRange(ctx, from, to)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 %d..%d 失败：%v\n", from, to, err)
			total.failed = true
			return total
		}

		var headers map[uint64]*types.Header
		if timeCheck {
			headers, err = client.HeadersByRange(ctx, distinctBlocks(chainLogs))
			if err != nil {
				if isArchiveLimit(err) {
					fmt.Printf("%d..%d：块头回放被归档限制拒绝，本次跳过 block_time 校验\n", from, to)
					timeCheck = false
				} else {
					fmt.Fprintf(os.Stderr, "拉取 %d..%d 块头失败：%v\n", from, to, err)
					total.failed = true
					return total
				}
			}
		}

		batchResult := diffBatch(chainLogs, dbLogs, address, headers)
		total.add(batchResult, len(chainLogs))
		total.batches++

		fmt.Printf("%d..%d chain=%d db=%d missing=%d extra=%d mismatch=%d time_mismatch=%d\n",
			from, to, len(chainLogs), batchResult.dbCount,
			batchResult.missing, batchResult.extra, batchResult.mismatch, batchResult.timeMismatch)
	}

	return total
}

// result 是一次对拍的累计结果。
type result struct {
	batches      int
	chainCount   int
	dbCount      int
	missing      int
	extra        int
	mismatch     int
	timeMismatch int
	failed       bool
}

func (r *result) add(batch batchResult, chainCount int) {
	r.chainCount += chainCount
	r.dbCount += batch.dbCount
	r.missing += batch.missing
	r.extra += batch.extra
	r.mismatch += batch.mismatch
	r.timeMismatch += batch.timeMismatch
}

func (r *result) ok() bool {
	return !r.failed && r.missing == 0 && r.extra == 0 && r.mismatch == 0 && r.timeMismatch == 0
}

func printSummary(r result) {
	fmt.Printf("\n汇总：%d 批，链上 %d 条，库内 %d 条\n", r.batches, r.chainCount, r.dbCount)
	fmt.Printf("  缺失 %d，多余 %d，字段不一致 %d，block_time 不一致 %d\n",
		r.missing, r.extra, r.mismatch, r.timeMismatch)
}

// batchResult 是单批的差异计数。
type batchResult struct {
	dbCount      int
	missing      int
	extra        int
	mismatch     int
	timeMismatch int
}

// diffBatch 把链上日志与库内日志做集合与字段比对。
//
// 主键是 (tx_hash, log_index)：chain_log 的主键正是这两列，因此用同一把钥匙
// 才能区分「缺失」与「多余」——单看总数相等并不能排除这种错误。
func diffBatch(chainLogs []types.Log, dbLogs []store.LogRecord, address common.Address, headers map[uint64]*types.Header) batchResult {
	dbIndex := make(map[logKey]*store.LogRecord, len(dbLogs))
	for i := range dbLogs {
		// 只核对目标合约：库里若存在其他合约的日志，不算差异。
		if dbLogs[i].Contract != address {
			continue
		}
		dbIndex[logKey{txHash: dbLogs[i].TxHash, index: dbLogs[i].LogIndex}] = &dbLogs[i]
	}

	var out batchResult
	out.dbCount = len(dbIndex)

	seen := make(map[logKey]struct{}, len(chainLogs))
	for _, chainLog := range chainLogs {
		key := logKey{txHash: chainLog.TxHash, index: uint32(chainLog.Index)}
		seen[key] = struct{}{}

		dbLog, found := dbIndex[key]
		if !found {
			out.missing++
			continue
		}
		if !sameLog(chainLog, dbLog) {
			out.mismatch++
		}
		if header, ok := headers[chainLog.BlockNumber]; ok && !dbLog.BlockTime.Equal(time.Unix(int64(header.Time), 0)) {
			out.timeMismatch++
		}
	}

	for key := range dbIndex {
		if _, ok := seen[key]; !ok {
			out.extra++
		}
	}

	return out
}

// logKey 对应 chain_log 的主键 (tx_hash, log_index)。
type logKey struct {
	txHash common.Hash
	index  uint32
}

// sameLog 逐字段比较：块号、合约、全部 topic、data。
func sameLog(chainLog types.Log, dbLog *store.LogRecord) bool {
	if chainLog.BlockNumber != dbLog.BlockNumber {
		return false
	}
	if chainLog.Address != dbLog.Contract {
		return false
	}

	topics := dbTopics(dbLog)
	if len(chainLog.Topics) != len(topics) {
		return false
	}
	for i := range topics {
		if chainLog.Topics[i] != topics[i] {
			return false
		}
	}

	return bytes.Equal(chainLog.Data, dbLog.Data)
}

func dbTopics(dbLog *store.LogRecord) []common.Hash {
	topics := make([]common.Hash, 0, 4)
	for _, topic := range []*common.Hash{dbLog.Topic0, dbLog.Topic1, dbLog.Topic2, dbLog.Topic3} {
		if topic != nil {
			topics = append(topics, *topic)
		}
	}
	return topics
}

// distinctBlocks 返回批内出现过的块号，升序去重。
func distinctBlocks(logs []types.Log) []uint64 {
	if len(logs) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{}, len(logs)/8+1)
	blocks := make([]uint64, 0, len(seen))
	for _, log := range logs {
		if _, ok := seen[log.BlockNumber]; ok {
			continue
		}
		seen[log.BlockNumber] = struct{}{}
		blocks = append(blocks, log.BlockNumber)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	return blocks
}

// resolveRange 把命令行参数收敛到库内区间。
func resolveRange(from, to uint64, coverage store.Coverage) (uint64, uint64, error) {
	start := coverage.FromBlock
	if from != 0 {
		start = from
	}
	end := coverage.ToBlock
	if to != 0 {
		end = to
	}

	if start > end {
		return 0, 0, fmt.Errorf("起始块 %d 大于结束块 %d", start, end)
	}
	if start < coverage.FromBlock || end > coverage.ToBlock {
		return 0, 0, fmt.Errorf("请求区间 %d..%d 超出库内区间 %d..%d",
			start, end, coverage.FromBlock, coverage.ToBlock)
	}
	return start, end, nil
}

// oldestServable 找出免费端点还能回放的最早区块。
//
// 免费端点的归档窗口是滚动的，起点会随时间上移。硬编码一个窗口宽度迟早失效，
// 所以用二分探测：先用单块请求确认两端，再二分收敛。每个探针只有一次轻量请求。
func oldestServable(ctx context.Context, client *chain.Client, from, to uint64, address common.Address) (uint64, error) {
	probe := func(height uint64) (bool, error) {
		_, err := client.Fetch(ctx, height, height, address)
		if err == nil {
			return true, nil
		}
		if isArchiveLimit(err) {
			return false, nil
		}
		return false, fmt.Errorf("探测区块 %d 失败：%w", height, err)
	}

	servable, err := probe(from)
	if err != nil {
		return 0, err
	}
	if servable {
		return from, nil
	}

	servable, err = probe(to)
	if err != nil {
		return 0, err
	}
	if !servable {
		return 0, errors.New("结束块也无法回放：整个区间都超出归档窗口")
	}

	low, high := from, to // 不变量：low 不可回放，high 可回放
	for low+1 < high {
		mid := low + (high-low)/2
		servable, err := probe(mid)
		if err != nil {
			return 0, err
		}
		if servable {
			high = mid
		} else {
			low = mid
		}
	}

	return high, nil
}

func isArchiveLimit(err error) bool {
	return err != nil && strings.Contains(err.Error(), archiveMarker)
}
