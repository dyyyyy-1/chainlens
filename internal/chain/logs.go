package chain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Fetch 按区块区间拉取指定合约的全部日志。
//
// 走 eth_getLogs 范围查询而非逐块 eth_getBlockByNumber（HANDOFF.md ADR-2），
// 签名与第 7 节 LogFetcher 的形状一致，便于 internal/indexer 直接以接口断言复用。
func (c *Client) Fetch(ctx context.Context, from, to uint64, addr common.Address) ([]types.Log, error) {
	if from > to {
		return nil, fmt.Errorf("chain: invalid block range %d..%d: from block is greater than to block", from, to)
	}
	if span := to - from + 1; span > MaxBlockRange {
		return nil, fmt.Errorf("chain: block range %d..%d spans %d blocks, exceeds max %d", from, to, span, MaxBlockRange)
	}

	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: []common.Address{addr},
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
		return nil, fmt.Errorf("chain: logs %d..%d for %s: %w", from, to, addr.Hex(), err)
	}

	return logs, nil
}
