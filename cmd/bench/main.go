// Command bench 测量索引器的「拉取 + 解码」两段耗时，不写库。
//
// 存在的理由：索引器日志里的 elapsed_ms 把拉取、解码、写入、块头全部混在一起，
// 无法回答「换端点有没有用」「批量该调多大」这类问题。本工具只做前两段，
// 且完全不碰数据库的写路径，因此可以反复跑而不污染数据。
//
// 用法：
//
//	go run ./cmd/bench -batch 500 -rounds 3
//	go run ./cmd/bench -batch 100 -rpc https://eth.drpc.org -label drpc
//
// 输出 CSV（表头固定），便于直接贴进表格对比。最后一行是汇总。
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/dyyyyy-1/chainlens/internal/chain"
	"github.com/dyyyyy-1/chainlens/internal/config"
	"github.com/dyyyyy-1/chainlens/internal/decoder"
	"github.com/dyyyyy-1/chainlens/internal/store"
)

const (
	// envFile 是本地配置文件名，已加入 .gitignore。
	envFile = ".env"

	// defaultMargin 是相对链头回退的块数。
	//
	// 实测 publicnode 的窗口不足 10,000 块（2026-11-23：26,466,700 已被拒绝，
	// 26,467,700 可用），所以默认退 6,000 块，留出约 3,000 块的缓冲。
	defaultMargin = 6000
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bench failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	from := flag.Uint64("from", 0, "起始块，缺省取链头回退 margin 块")
	margin := flag.Uint64("margin", defaultMargin, "from 缺省时的回退块数；免费端点窗口实测不足 10,000")
	batch := flag.Uint64("batch", 500, "每批块数")
	rounds := flag.Int("rounds", 3, "重复轮数，结果取平均")
	contractFlag := flag.String("contract", "", "目标合约，缺省取 .env")
	rpcFlag := flag.String("rpc", "", "覆盖 RPC 端点，缺省取 .env")
	label := flag.String("label", "", "CSV 里的端点标签，缺省取端点主机名")
	split := flag.Bool("split", false, "只跑一批，把「网络传输」与「JSON 反序列化」分开计时")
	h1only := flag.Bool("h1", false, "禁用 HTTP/2，强制 HTTP/1.1（验证代理对 h2 的处理）")
	nocompress := flag.Bool("nocompress", false, "不请求 gzip（验证代理对压缩流的转发开销）")
	flag.Parse()

	if *batch == 0 || *batch > chain.MaxBlockRange {
		return fmt.Errorf("batch 必须在 1..%d 之间", chain.MaxBlockRange)
	}
	if *rounds <= 0 {
		return fmt.Errorf("rounds 必须为正整数")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(envFile)
	if err != nil {
		return err
	}

	// 只读 contract_meta 拿 ABI，不启动索引器、不写任何表。
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	contract := cfg.TargetContracts[0]
	if *contractFlag != "" {
		contract = *contractFlag
	}
	if !common.IsHexAddress(contract) {
		return fmt.Errorf("无效的合约地址 %q", contract)
	}
	address := common.HexToAddress(contract)

	rpcURL := cfg.RPCHTTPURL
	if *rpcFlag != "" {
		rpcURL = *rpcFlag
	}
	name := *label
	if name == "" {
		name = strings.TrimPrefix(strings.TrimPrefix(rpcURL, "https://"), "http://")
		name = strings.SplitN(name, "/", 2)[0]
	}

	dec, err := loadDecoder(ctx, st, address)
	if err != nil {
		return err
	}

	client, err := chain.New(ctx, rpcURL, cfg.ChainID, chain.Options{})
	if err != nil {
		return err
	}
	defer client.Close()

	head, err := client.LatestBlockNumber(ctx)
	if err != nil {
		return err
	}

	if *split {
		return runSplit(ctx, rpcURL, name, head, *batch, *margin, address, *h1only, *nocompress)
	}

	start := *from
	if start == 0 {
		if head < *margin {
			return fmt.Errorf("链头 %d 太小，无法回退 %d 块", head, *margin)
		}
		start = head - *margin
	}
	if start+(*batch)*uint64(*rounds) > head {
		return fmt.Errorf("起始块 %d + %d×%d 超出链头 %d（减小 batch/rounds 或前移 from）",
			start, *batch, *rounds, head)
	}

	fmt.Fprintf(os.Stderr, "端点 %s，链头 %d，从 %d 开始，%d 批 × %d 轮\n\n", name, head, start, *batch, *rounds)
	fmt.Println("endpoint,batch,round,blocks,logs,fetch_ms,decode_ms,blocks_per_s,logs_per_s,decode_us_per_log")

	var (
		sumBlocks, sumLogs      float64
		sumFetchMs, sumDecodeMs float64
		okRounds                int
		firstErr                error
	)

	for round := 0; round < *rounds; round++ {
		batchFrom := start + uint64(round)*(*batch)
		batchTo := batchFrom + *batch - 1

		fetchStart := time.Now()
		logs, err := client.Fetch(ctx, batchFrom, batchTo, []common.Address{address})
		fetchElapsed := time.Since(fetchStart)

		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// CSV 里不能出现裸逗号：错误信息常常带逗号，会撑出多余列。
			msg := strings.ReplaceAll(err.Error(), ",", ";")
			fmt.Printf("%s,%d,%d,0,0,,,,,%s\n", name, *batch, round, msg)
			continue
		}

		decodeStart := time.Now()
		decoded := 0
		for _, entry := range logs {
			if dec.Decode(entry).IsDecoded() {
				decoded++
			}
		}
		decodeElapsed := time.Since(decodeStart)

		blocks := float64(*batch)
		logCount := float64(len(logs))
		fetchMs := float64(fetchElapsed.Microseconds()) / 1000
		decodeMs := float64(decodeElapsed.Microseconds()) / 1000

		fmt.Printf("%s,%d,%d,%d,%d,%.1f,%.1f,%.2f,%.0f,%.2f\n",
			name, *batch, round, *batch, len(logs),
			fetchMs, decodeMs,
			blocks/(fetchElapsed.Seconds()+decodeElapsed.Seconds()),
			logCount/(fetchElapsed.Seconds()+decodeElapsed.Seconds()),
			decodeMs*1000/max(logCount, 1),
		)

		sumBlocks += blocks
		sumLogs += logCount
		sumFetchMs += fetchMs
		sumDecodeMs += decodeMs
		okRounds++
	}

	if okRounds == 0 {
		return fmt.Errorf("全部轮次失败，首个错误：%w", firstErr)
	}

	totalSeconds := (sumFetchMs + sumDecodeMs) / 1000
	fmt.Printf("\n汇总 %s batch=%d：%d 轮成功，%d 块 / %.0f 条，拉取 %.0f ms，解码 %.0f ms，%.2f 块/秒，%.0f 条/秒，单条解码 %.2f µs\n",
		name, *batch, okRounds, int(sumBlocks), sumLogs,
		sumFetchMs/float64(okRounds), sumDecodeMs/float64(okRounds),
		sumBlocks/totalSeconds, sumLogs/totalSeconds, sumDecodeMs*1000/sumLogs,
	)

	return nil
}

// loadDecoder 优先用 contract_meta 里的 ABI（与索引器一致），没有则退回内置 ERC20。
func loadDecoder(ctx context.Context, st *store.Store, address common.Address) (*decoder.Decoder, error) {
	meta, found, err := st.LoadContractMeta(ctx, address)
	if err != nil {
		return nil, err
	}
	if found && len(meta.ABI) > 0 {
		dec, err := decoder.New(meta.ABI)
		if err == nil {
			return dec, nil
		}
	}

	dec, err := decoder.New([]byte(decoder.ERC20ABI))
	if err != nil {
		return nil, fmt.Errorf("build builtin ERC20 decoder: %w", err)
	}
	return dec, nil
}

// runSplit 把一次 eth_getLogs 拆成「传输」与「反序列化」两段。
//
// 动机：curl 直接拉 1000 块只要约 5 秒（14 MB/s），而走 ethclient 要 19.6 秒。
// 差值必须归因到具体环节，否则调批量、换端点都是在猜。
// rpc.CallContext 拿 json.RawMessage 时不做解码，正好把两段分开。
func runSplit(
	ctx context.Context,
	rpcURL, name string,
	head, batch, margin uint64,
	address common.Address,
	h1only, nocompress bool,
) error {
	if head < margin {
		return fmt.Errorf("链头 %d 太小，无法回退 %d 块", head, margin)
	}
	from := head - margin
	to := from + batch - 1

	// 统一用自定义 transport，才能控制压缩与协议；默认的 ethclient 两条都不暴露。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if nocompress {
		// Go 默认带 Accept-Encoding: gzip。实测经代理时压缩流只有约 1.5 MB/s，
		// 而未压缩的裸流有 13 MB/s——压缩反而更慢。
		transport.DisableCompression = true
	}
	if h1only {
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		// 只广告 http/1.1：仅清 TLSNextProto 不够，ALPN 仍会协商出 h2，
		// 结果是客户端按 h1 解析 h2 帧，报 "malformed HTTP response"。
		transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	}
	client, err := rpc.DialOptions(ctx, rpcURL, rpc.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer client.Close()

	// 预热一次，把 TLS 握手与连接建立排除在计时之外。
	var warm string
	if err := client.CallContext(ctx, &warm, "eth_blockNumber"); err != nil {
		return fmt.Errorf("warmup: %w", err)
	}

	filter := map[string]any{
		"fromBlock": hexutil.EncodeUint64(from),
		"toBlock":   hexutil.EncodeUint64(to),
		"address":   address,
	}

	var raw json.RawMessage
	transferStart := time.Now()
	if err := client.CallContext(ctx, &raw, "eth_getLogs", filter); err != nil {
		return fmt.Errorf("eth_getLogs %d..%d: %w", from, to, err)
	}
	transferElapsed := time.Since(transferStart)

	parseStart := time.Now()
	var logs []types.Log
	if err := json.Unmarshal(raw, &logs); err != nil {
		return fmt.Errorf("unmarshal logs: %w", err)
	}
	parseElapsed := time.Since(parseStart)

	mb := float64(len(raw)) / (1024 * 1024)
	proto := "h2+gzip"
	switch {
	case h1only && nocompress:
		proto = "h1+raw"
	case h1only:
		proto = "h1+gzip"
	case nocompress:
		proto = "h2+raw"
	}
	fmt.Printf("%s [%s] 区间 %d..%d（%d 块）\n", name, proto, from, to, batch)
	fmt.Printf("  响应体 %.1f MB / %d 条日志\n", mb, len(logs))
	fmt.Printf("  传输 %.0f ms（%.1f MB/s，%.0f 块/s）\n",
		float64(transferElapsed.Microseconds())/1000, mb/transferElapsed.Seconds(), float64(batch)/transferElapsed.Seconds())
	fmt.Printf("  反序列化 %.0f ms（%.1f µs/条）\n",
		float64(parseElapsed.Microseconds())/1000, float64(parseElapsed.Microseconds())/float64(max(len(logs), 1)))
	fmt.Printf("  两段合计 %.0f ms，反序列化占 %.0f%%\n",
		float64((transferElapsed+parseElapsed).Microseconds())/1000,
		100*float64(parseElapsed)/float64(transferElapsed+parseElapsed))

	return nil
}
