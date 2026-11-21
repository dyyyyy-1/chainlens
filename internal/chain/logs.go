package chain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Fetch 按区块区间拉取指定合约集合的全部日志。
//
// 走 eth_getLogs 范围查询而非逐块 eth_getBlockByNumber（HANDOFF.md ADR-2），
// 签名与第 7 节 LogFetcher 的形状一致，便于 internal/indexer 直接以接口断言复用。
//
// 一次请求带多个地址，而不是逐合约发请求：eth_getLogs 的 address 本来就是数组，
// 同一区间的块只扫一遍。代价是响应体更大，但请求数下降，对免费端点的限流更友好。
func (c *Client) Fetch(ctx context.Context, from, to uint64, addresses []common.Address) ([]types.Log, error) {
	if from > to {
		return nil, fmt.Errorf("chain: invalid block range %d..%d: from block is greater than to block", from, to)
	}
	if span := to - from + 1; span > MaxBlockRange {
		return nil, fmt.Errorf("chain: block range %d..%d spans %d blocks, exceeds max %d", from, to, span, MaxBlockRange)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("chain: logs %d..%d: empty address list", from, to)
	}

	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: addresses,
	}

	var logs []types.Log
	err := c.withRetry(ctx, "get logs", func(callCtx context.Context) error {
		fetched, err := c.eth.FilterLogs(callCtx, query)
		if err != nil {
			return err
		}
		logs = fetched
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("chain: logs %d..%d for %d contracts: %w", from, to, len(addresses), err)
	}

	return logs, nil
}
