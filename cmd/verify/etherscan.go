package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/dyyyyy-1/chainlens/internal/config"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

const (
	// etherscanBaseURL 是 API V2 的入口。V1 已于 2025-08-15 弃用。
	etherscanBaseURL = "https://api.etherscan.io/v2/api"

	// etherscanFreePageLimit 是免费档单次响应的记录上限（2026-07-01 起从 10000 降到 1000）。
	//
	// 达到这个数**不能**当成「刚好这么多条」：更可能是被截断。
	// 免费档不会为此报错，所以必须由我们主动判定。
	etherscanFreePageLimit = 1000

	// etherscanMinInterval 对应免费档 3 次/秒，留约 20% 余量。
	etherscanMinInterval = 350 * time.Millisecond
)

// errEtherscanTruncated 表示单次响应触到记录上限，结果可能不完整。
var errEtherscanTruncated = errors.New("etherscan: response hit the 1000-record limit, narrow the block range")

// indexUnknown 标记「第三方没有给 log_index」的日志。
//
// 实测 Etherscan V2 会对个别日志返回 logIndex="0x"（2026-11-20，块 26433620 与
// 26433621 各一条），topics 与 data 却是完整的。它不是数据缺失，但也不能当成 0，
// 否则会和真正的 index 0 混淆。
const indexUnknown = ^uint(0)

// etherscanClient 是 Etherscan getLogs 的最小客户端。
type etherscanClient struct {
	apiKey   string
	chainID  uint64
	http     *http.Client
	lastCall time.Time
}

// newEtherscanClient 构造客户端。proxyURL 为空时沿用标准库的环境变量行为。
func newEtherscanClient(apiKey string, chainID uint64, proxyURL string) (*etherscanClient, error) {
	if apiKey == "" {
		return nil, errors.New("etherscan: API key is empty")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("etherscan: parse proxy %q: %w", proxyURL, err)
		}
		transport.Proxy = http.ProxyURL(parsed)
	}

	return &etherscanClient{
		apiKey:  apiKey,
		chainID: chainID,
		http:    &http.Client{Timeout: 30 * time.Second, Transport: transport},
	}, nil
}

// FetchLogs 拉取闭区间内某合约的全部日志，同时返回出现过的块时间。
//
// 块时间来自每条日志的 timeStamp，不需要额外请求块头——这正是不再走 RPC 的好处：
// Etherscan 的日志响应自带时间戳。
func (c *etherscanClient) FetchLogs(ctx context.Context, from, to uint64, address common.Address) ([]types.Log, map[uint64]time.Time, error) {
	entries, err := c.getLogs(ctx, from, to, address)
	if err != nil {
		return nil, nil, err
	}

	logs := make([]types.Log, 0, len(entries))
	blockTimes := make(map[uint64]time.Time, 16)

	for _, entry := range entries {
		parsed, blockTime, err := entry.toTypesLog()
		if err != nil {
			return nil, nil, fmt.Errorf("etherscan: log %s#%s: %w", entry.TransactionHash, entry.LogIndex, err)
		}
		logs = append(logs, parsed)
		blockTimes[parsed.BlockNumber] = blockTime
	}

	return logs, blockTimes, nil
}

func (c *etherscanClient) getLogs(ctx context.Context, from, to uint64, address common.Address) ([]etherscanLog, error) {
	if err := c.waitTurn(ctx); err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Set("chainid", strconv.FormatUint(c.chainID, 10))
	query.Set("module", "logs")
	query.Set("action", "getLogs")
	query.Set("fromBlock", strconv.FormatUint(from, 10))
	query.Set("toBlock", strconv.FormatUint(to, 10))
	query.Set("address", address.Hex())
	query.Set("apikey", c.apiKey)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, etherscanBaseURL+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("etherscan: build request: %w", err)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("etherscan: request %d..%d: %w (需要用代理时加 -proxy)", from, to, err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("etherscan: read response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("etherscan: HTTP %d: %s", response.StatusCode, truncate(string(body), 200))
	}

	var envelope struct {
		Status  string          `json:"status"`
		Message string          `json:"message"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("etherscan: decode response: %w (body=%s)", err, truncate(string(body), 200))
	}

	// 空区间返回 status=0 + message="No records found"，这是正常结果而不是失败。
	if envelope.Status != "1" {
		if strings.Contains(envelope.Message, "No records found") {
			return nil, nil
		}
		return nil, fmt.Errorf("etherscan: status=%s message=%s result=%s",
			envelope.Status, envelope.Message, truncate(string(envelope.Result), 200))
	}

	var entries []etherscanLog
	if err := json.Unmarshal(envelope.Result, &entries); err != nil {
		return nil, fmt.Errorf("etherscan: decode logs: %w", err)
	}
	if len(entries) >= etherscanFreePageLimit {
		return nil, fmt.Errorf("%w（%d..%d 返回 %d 条）", errEtherscanTruncated, from, to, len(entries))
	}

	return entries, nil
}

// waitTurn 保证两次请求间隔不小于免费档允许的频率。
func (c *etherscanClient) waitTurn(ctx context.Context) error {
	if !c.lastCall.IsZero() {
		if elapsed := time.Since(c.lastCall); elapsed < etherscanMinInterval {
			timer := time.NewTimer(etherscanMinInterval - elapsed)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	c.lastCall = time.Now()
	return nil
}

// etherscanLog 是 getLogs 的单条记录。
//
// 数值字段是十六进制字符串（Ethereum 主网），但解析时兼容十进制，
// 避免不同链或将来格式变化让整个对拍失效。
type etherscanLog struct {
	Address         string   `json:"address"`
	Topics          []string `json:"topics"`
	Data            string   `json:"data"`
	BlockNumber     string   `json:"blockNumber"`
	TimeStamp       string   `json:"timeStamp"`
	LogIndex        string   `json:"logIndex"`
	TransactionHash string   `json:"transactionHash"`
}

func (e etherscanLog) toTypesLog() (types.Log, time.Time, error) {
	blockNumber, err := parseQuantity(e.BlockNumber)
	if err != nil {
		return types.Log{}, time.Time{}, fmt.Errorf("blockNumber: %w", err)
	}
	logIndex := indexUnknown
	if !isBlankQuantity(e.LogIndex) {
		parsed, err := parseQuantity(e.LogIndex)
		if err != nil {
			return types.Log{}, time.Time{}, fmt.Errorf("logIndex: %w", err)
		}
		logIndex = uint(parsed)
	}
	timestamp, err := parseQuantity(e.TimeStamp)
	if err != nil {
		return types.Log{}, time.Time{}, fmt.Errorf("timeStamp: %w", err)
	}
	if !common.IsHexAddress(e.Address) {
		return types.Log{}, time.Time{}, fmt.Errorf("invalid address %q", e.Address)
	}

	data, err := hexutil.Decode(e.Data)
	if err != nil {
		return types.Log{}, time.Time{}, fmt.Errorf("data: %w", err)
	}

	topics := make([]common.Hash, 0, len(e.Topics))
	for _, topic := range e.Topics {
		topics = append(topics, common.HexToHash(topic))
	}

	entry := types.Log{
		Address:     common.HexToAddress(e.Address),
		Topics:      topics,
		Data:        data,
		BlockNumber: blockNumber,
		TxHash:      common.HexToHash(e.TransactionHash),
		Index:       logIndex,
	}

	return entry, time.Unix(int64(timestamp), 0).UTC(), nil
}

// isBlankQuantity 判断第三方是否给了空数量（"0x" / "0X" / 空串）。
func isBlankQuantity(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	return trimmed == "" || trimmed == "0x" || trimmed == "0X"
}

// parseQuantity 解析十六进制（0x 前缀）或十进制数量字符串。
func parseQuantity(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("empty quantity")
	}
	if strings.HasPrefix(raw, "0x") || strings.HasPrefix(raw, "0X") {
		return hexutil.DecodeUint64(raw)
	}
	return strconv.ParseUint(raw, 10, 64)
}

// sampleRanges 在 [start, end] 内均匀取 segments 段，每段宽 width 块。
//
// 抽样而不是全量是有意的：全量同源回放已由 -source rpc 覆盖，
// 这里要验证的是「第三方数据源看到的日志与我们记录的是否一致」，
// 均匀抽样足以暴露系统性偏差，而且不烧免费额度（10 万次/天）。
func sampleRanges(start, end, segments, width uint64) [][2]uint64 {
	if start > end || segments == 0 || width == 0 {
		return nil
	}

	span := end - start + 1
	if span <= segments*width {
		return [][2]uint64{{start, end}}
	}

	step := span / segments
	ranges := make([][2]uint64, 0, segments)
	for i := uint64(0); i < segments; i++ {
		from := start + i*step
		to := min(from+width-1, end)
		ranges = append(ranges, [2]uint64{from, to})
	}
	return ranges
}

// etherscanOptions 是 -source etherscan 的运行参数。
type etherscanOptions struct {
	segments uint64
	width    uint64
}

// runEtherscan 用 Etherscan 的 getLogs 作为第三方数据源做对拍。
func runEtherscan(
	ctx context.Context,
	cfg config.Config,
	st *store.Store,
	address common.Address,
	start, end uint64,
	proxyURL string,
	opts etherscanOptions,
) int {
	if cfg.EtherscanAPIKey == "" {
		fmt.Fprintln(os.Stderr, "ETHERSCAN_API_KEY 为空：先到 https://etherscan.io/myapikey 创建并填入 .env")
		return 2
	}

	client, err := newEtherscanClient(cfg.EtherscanAPIKey, cfg.ChainID, proxyURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	ranges := sampleRanges(start, end, opts.segments, opts.width)
	if len(ranges) == 0 {
		fmt.Fprintln(os.Stderr, "没有可核对的区间")
		return 2
	}

	fmt.Printf("数据源：Etherscan V2（chainid=%d），抽样 %d 段 × %d 块，覆盖 %d..%d\n",
		cfg.ChainID, len(ranges), opts.width, start, end)

	var total result
	for _, blockRange := range ranges {
		from, to := blockRange[0], blockRange[1]

		chainLogs, blockTimes, err := client.FetchLogs(ctx, from, to, address)
		if err != nil {
			fmt.Fprintf(os.Stderr, "拉取 %d..%d 失败：%v\n", from, to, err)
			total.failed = true
			return 2
		}
		if len(chainLogs) == 0 && blockTimes == nil {
			fmt.Printf("%d..%d etherscan=0（该区间无日志）\n", from, to)
			continue
		}

		dbLogs, err := st.LogsByRange(ctx, from, to)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 %d..%d 失败：%v\n", from, to, err)
			total.failed = true
			return 2
		}

		chainLogs, dbLogs, indexlessMatched, indexlessMissing, err := reconcileIndexless(chainLogs, dbLogs, address)
		if err != nil {
			fmt.Fprintf(os.Stderr, "核对 %d..%d 失败：%v\n", from, to, err)
			total.failed = true
			return 2
		}

		// diffBatch 只用 header.Time 做 block_time 校验，这里按 Etherscan 的
		// timeStamp 造一个最小 header；其余字段留零值，不要当成完整块头使用。
		headers := make(map[uint64]*types.Header, len(blockTimes))
		for number, blockTime := range blockTimes {
			headers[number] = &types.Header{Time: uint64(blockTime.Unix())}
		}

		batchResult := diffBatch(chainLogs, dbLogs, address, headers)
		// 第三方没给 log_index、但按 (tx, topics, data) 唯一匹配上的条目，
		// 匹配成功的不算差异；匹配不上的计入 missing（不能被「跳过」吞掉）。
		batchResult.missing += indexlessMissing

		total.add(batchResult, len(chainLogs))
		total.batches++

		fmt.Printf("%d..%d etherscan=%d db=%d missing=%d extra=%d mismatch=%d time_mismatch=%d indexless=%d\n",
			from, to, len(chainLogs), batchResult.dbCount,
			batchResult.missing, batchResult.extra, batchResult.mismatch, batchResult.timeMismatch,
			indexlessMatched)
	}

	printSummary(total)
	if total.ok() {
		fmt.Println("结果：一致（第三方数据源视角）")
		return 0
	}
	fmt.Println("结果：存在差异")
	return 1
}

// reconcileIndexless 处理第三方未给 log_index 的记录。
//
// 直接丢弃会掩盖「库里真的缺这条」的情况，所以按 (tx_hash, topics, data) 在库内
// 做唯一匹配：唯一候选视为同一条并从两侧摘除；零候选计入 missing（暴露为差异）；
// 多候选无法判定，报错而不是猜。
func reconcileIndexless(chainLogs []types.Log, dbLogs []store.LogRecord, address common.Address) (
	kept []types.Log, remaining []store.LogRecord, matched int, missing int, err error,
) {
	if !hasIndexless(chainLogs) {
		return chainLogs, dbLogs, 0, 0, nil
	}

	used := make([]bool, len(dbLogs))
	kept = make([]types.Log, 0, len(chainLogs))

	for _, chainLog := range chainLogs {
		if chainLog.Index != indexUnknown {
			kept = append(kept, chainLog)
			continue
		}

		candidates := make([]int, 0, 1)
		for i := range dbLogs {
			if used[i] || dbLogs[i].Contract != address || dbLogs[i].TxHash != chainLog.TxHash {
				continue
			}
			if !sameTopicsAndData(&dbLogs[i], chainLog) {
				continue
			}
			candidates = append(candidates, i)
		}

		switch len(candidates) {
		case 1:
			used[candidates[0]] = true
			matched++
		case 0:
			missing++
		default:
			return nil, nil, 0, 0, fmt.Errorf(
				"交易 %s 有 %d 条主题与数据相同的日志，无法判断哪条对应第三方的无索引记录",
				chainLog.TxHash.Hex(), len(candidates))
		}
	}

	remaining = make([]store.LogRecord, 0, len(dbLogs)-matched)
	for i := range dbLogs {
		if !used[i] {
			remaining = append(remaining, dbLogs[i])
		}
	}

	return kept, remaining, matched, missing, nil
}

func hasIndexless(logs []types.Log) bool {
	for _, entry := range logs {
		if entry.Index == indexUnknown {
			return true
		}
	}
	return false
}

// sameTopicsAndData 按位置比较主题与数据；库内 topic 可为 NULL，因此逐项对齐。
func sameTopicsAndData(dbLog *store.LogRecord, chainLog types.Log) bool {
	dbT := dbTopics(dbLog)
	if len(dbT) != len(chainLog.Topics) {
		return false
	}
	for i := range dbT {
		if dbT[i] != chainLog.Topics[i] {
			return false
		}
	}
	return bytes.Equal(dbLog.Data, chainLog.Data)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
