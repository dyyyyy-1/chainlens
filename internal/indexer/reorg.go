package indexer

import (
	"context"
	"fmt"
)

// maxReorgDepth 是允许自动回溯的最大深度。
//
// 超过这个深度多半不是普通重组（可能是换了链，或本地数据被污染），
// 继续自动删数据的风险高于收益，直接报错让人介入。
const maxReorgDepth = 128

// checkReorg 校验本地 tip 与链上同一高度是否仍然是同一个块。
//
// 比对对象是本地 tip 本身，而不是「下一个待写区块的父哈希」：追平时 next_block == head+1，
// 那个块尚不存在，无法作为比对基准；而重组恰恰最容易发生在追平之后。
//
// 返回 (fork, true, nil) 表示发生重组，fork 是最后一个仍然有效的块号；
// 返回 (0, false, nil) 表示无需处理。
func (s *Scanner) checkReorg(ctx context.Context) (uint64, bool, error) {
	tip, hasTip, err := s.store.LocalTip(ctx)
	if err != nil {
		return 0, false, err
	}
	if !hasTip {
		return 0, false, nil
	}

	headers, err := s.headers.HeadersByRange(ctx, []uint64{tip.Number})
	if err != nil {
		return 0, false, err
	}
	chainTip, hasChain := headers[tip.Number]
	if !hasChain {
		return 0, false, fmt.Errorf("indexer: local tip %d missing from header response", tip.Number)
	}

	if chainTip.Hash() == tip.Hash {
		return 0, false, nil
	}

	fork, err := s.findForkPoint(ctx, tip.Number)
	if err != nil {
		return 0, false, err
	}

	return fork, true, nil
}

// findForkPoint 从本地 tip 往回逐块比对链上哈希，返回最后一个仍然有效的块号。
//
// 一次批量取回整个回溯窗口的链上块头，避免逐块往返。
func (s *Scanner) findForkPoint(ctx context.Context, tipNumber uint64) (uint64, error) {
	lowest := uint64(0)
	if tipNumber > maxReorgDepth {
		lowest = tipNumber - maxReorgDepth
	}

	numbers := blockNumbers(lowest, tipNumber)
	chainHeaders, err := s.headers.HeadersByRange(ctx, numbers)
	if err != nil {
		return 0, err
	}

	// 从 tip 往回找，第一个与链上一致的块就是分叉点。
	for i := len(numbers) - 1; i >= 0; i-- {
		number := numbers[i]

		localHash, hasLocal, err := s.store.HeaderHashAt(ctx, number)
		if err != nil {
			return 0, err
		}
		if !hasLocal {
			continue
		}

		chainHeader, hasChain := chainHeaders[number]
		if !hasChain {
			return 0, fmt.Errorf("indexer: block %d missing from header response", number)
		}
		if chainHeader.Hash() == localHash {
			return number, nil
		}
	}

	return 0, fmt.Errorf("indexer: no common ancestor within %d blocks below %d", maxReorgDepth, tipNumber)
}

// applyReorg 在一个事务内删除分叉点之后的数据，并把游标退回分叉点。
func (s *Scanner) applyReorg(ctx context.Context, fork, finalizedBlock uint64) error {
	tx, err := s.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	deleted, err := s.store.DeleteFrom(ctx, tx, fork+1)
	if err != nil {
		return err
	}

	// 回滚之后 finalized_block 不可能高于分叉点，取两者较小值。
	newFinalized := min(finalizedBlock, fork)
	if err := s.store.Advance(ctx, tx, fork+1, newFinalized); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("indexer: commit reorg rollback at %d: %w", fork, err)
	}

	s.logger.Warn("reorg rolled back",
		"fork_block", fork,
		"headers_deleted", deleted,
		"finalized_block", newFinalized,
	)

	return nil
}
