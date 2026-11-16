// Package chain 封装以太坊 JSON-RPC 访问：连接复用、超时、重试与区间日志拉取。
package chain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	// MaxBlockRange 是单次 eth_getLogs 允许覆盖的最大区块数（HANDOFF.md ADR-2、8.4）。
	MaxBlockRange = 2000

	// rpcErrRateLimited 是 geth 与多数托管节点表达「请求过快」的错误码。
	rpcErrRateLimited = -32005

	// headerBatchSize 是单次 JSON-RPC 批量请求容纳的块头数。
	// 逐块请求在 2000 块批下需要 2000 次往返，批量后降到 20 次。
	headerBatchSize = 100

	// defaultRequestTimeout 是单次 JSON-RPC 调用的超时上限。
	//
	// 取 60s 而非更小的值：USDC 这类高频合约在 2000 块批量下，单次 eth_getLogs
	// 实测耗时 41.6s、响应体 97 MB。超时设成 20s 会让每次请求都踩线并触发重传，
	// 白白多传近 100 MB。
	defaultRequestTimeout = 60 * time.Second
	defaultMaxAttempts    = 3
	defaultRetryBaseDelay = 500 * time.Millisecond
	maxRetryBackoff       = 8 * time.Second
)

// Options 控制单次调用的超时与重试策略，零值字段回落到默认值。
type Options struct {
	// RequestTimeout 是单次 JSON-RPC 调用的超时上限。
	RequestTimeout time.Duration
	// MaxAttempts 是含首次在内的最大尝试次数。
	MaxAttempts int
	// RetryBaseDelay 是指数退避的基准延迟。
	RetryBaseDelay time.Duration
}

func (o Options) withDefaults() Options {
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = defaultRequestTimeout
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = defaultMaxAttempts
	}
	if o.RetryBaseDelay <= 0 {
		o.RetryBaseDelay = defaultRetryBaseDelay
	}
	return o
}

// Client 是 ethclient.Client 的薄封装，只暴露索引器所需的读方法。
//
// 刻意不在此包定义 LogFetcher 之类的接口：接口由使用方（internal/indexer）声明。
type Client struct {
	eth  *ethclient.Client
	opts Options
}

// New 建立连接并校验链 ID，防止主网与 Sepolia 配置混用（HANDOFF.md ADR-5）。
func New(ctx context.Context, httpURL string, wantChainID uint64, opts Options) (*Client, error) {
	if httpURL == "" {
		return nil, errors.New("chain: empty RPC HTTP URL")
	}

	eth, err := ethclient.DialContext(ctx, httpURL)
	if err != nil {
		return nil, fmt.Errorf("chain: dial %s: %w", httpURL, err)
	}

	client := &Client{eth: eth, opts: opts.withDefaults()}

	callCtx, cancel := context.WithTimeout(ctx, client.opts.RequestTimeout)
	defer cancel()

	actualChainID, err := eth.ChainID(callCtx)
	if err != nil {
		eth.Close()
		return nil, fmt.Errorf("chain: query chain id: %w", err)
	}
	if actualChainID.Uint64() != wantChainID {
		eth.Close()
		return nil, fmt.Errorf("chain: chain id mismatch: endpoint reports %s, config expects %d", actualChainID, wantChainID)
	}

	return client, nil
}

// Close 释放底层连接。
func (c *Client) Close() {
	c.eth.Close()
}

// HeaderByNumber 拉取指定高度的区块头。
func (c *Client) HeaderByNumber(ctx context.Context, number uint64) (*types.Header, error) {
	var header *types.Header

	err := c.withRetry(ctx, "get header", func(callCtx context.Context) error {
		fetched, err := c.eth.HeaderByNumber(callCtx, new(big.Int).SetUint64(number))
		if err != nil {
			return err
		}
		header = fetched
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("chain: header %d: %w", number, err)
	}
	if header == nil {
		return nil, fmt.Errorf("chain: header %d: %w", number, ethereum.NotFound)
	}

	return header, nil
}

// LatestBlockNumber 返回链头高度。
func (c *Client) LatestBlockNumber(ctx context.Context) (uint64, error) {
	var number uint64

	err := c.withRetry(ctx, "get latest block", func(callCtx context.Context) error {
		latest, err := c.eth.BlockNumber(callCtx)
		if err != nil {
			return err
		}
		number = latest
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("chain: latest block: %w", err)
	}

	return number, nil
}

// HeadersByRange 拉取一组指定高度的块头，用于补齐 chain_log.block_time。
//
// numbers 由调用方负责去重与升序。内部按 headerBatchSize 打包成 JSON-RPC 批量请求，
// 单批内任一子请求失败即整批重试，避免部分成功的状态被静默接受。
func (c *Client) HeadersByRange(ctx context.Context, numbers []uint64) (map[uint64]*types.Header, error) {
	headers := make(map[uint64]*types.Header, len(numbers))
	if len(numbers) == 0 {
		return headers, nil
	}

	rawClient := c.eth.Client()

	for start := 0; start < len(numbers); start += headerBatchSize {
		chunk := numbers[start:min(start+headerBatchSize, len(numbers))]

		err := c.withRetry(ctx, "get headers", func(callCtx context.Context) error {
			elems := make([]rpc.BatchElem, len(chunk))
			results := make([]*types.Header, len(chunk))

			for i, number := range chunk {
				results[i] = new(types.Header)
				elems[i] = rpc.BatchElem{
					Method: "eth_getBlockByNumber",
					Args:   []any{hexutil.EncodeUint64(number), false},
					Result: results[i],
				}
			}

			if err := rawClient.BatchCallContext(callCtx, elems); err != nil {
				return err
			}

			for i := range elems {
				if elems[i].Error != nil {
					return elems[i].Error
				}
				if results[i].Number == nil {
					return fmt.Errorf("header %d: %w", chunk[i], ethereum.NotFound)
				}
				headers[chunk[i]] = results[i]
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("chain: headers %d..%d: %w", chunk[0], chunk[len(chunk)-1], err)
		}
	}

	return headers, nil
}

// withRetry 以指数退避执行 call，仅对可恢复错误重试。
//
// 每次尝试都派生带超时的子上下文，避免单次慢请求拖垮整体进度；父上下文结束时立即放弃。
func (c *Client) withRetry(ctx context.Context, op string, call func(context.Context) error) error {
	var lastErr error

	for attempt := 1; attempt <= c.opts.MaxAttempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
		err := call(callCtx)
		cancel()

		if err == nil {
			return nil
		}
		lastErr = err

		if attempt == c.opts.MaxAttempts || !isRetryable(ctx, err) {
			break
		}

		timer := time.NewTimer(backoff(c.opts.RetryBaseDelay, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%s: %w", op, ctx.Err())
		case <-timer.C:
		}
	}

	return fmt.Errorf("%s: %w", op, lastErr)
}

// isRetryable 判定错误是否值得重试：限流、网络抖动、5xx 与单次调用超时可重试，
// 参数错误、结果不存在与父上下文取消不可重试。
func isRetryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var httpErr rpc.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= http.StatusInternalServerError
	}

	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		return rpcErr.ErrorCode() == rpcErrRateLimited
	}

	var netErr net.Error
	return errors.As(err, &netErr)
}

// backoff 计算第 attempt 次重试前的等待时长：指数增长并叠加最多 25% 抖动。
func backoff(base time.Duration, attempt int) time.Duration {
	delay := base
	for i := 1; i < attempt; i++ {
		if delay >= maxRetryBackoff {
			break
		}
		delay *= 2
	}
	if delay > maxRetryBackoff {
		delay = maxRetryBackoff
	}

	return delay + time.Duration(rand.Int64N(int64(delay)/4+1))
}
