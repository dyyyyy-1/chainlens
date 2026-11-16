package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

const (
	loadTipSQL = `SELECT number, hash FROM block_header ORDER BY number DESC LIMIT 1`

	loadHeaderHashSQL = `SELECT hash FROM block_header WHERE number = $1`

	promoteFinalizedSQL = `
UPDATE block_header
SET is_finalized = true
WHERE number > $1 AND number <= $2 AND is_finalized = false`

	// 删除顺序不能颠倒：decoded_event 没有 block_number 列，只能通过 chain_log 反查，
	// 所以必须先按子查询删掉它，再删 chain_log 本身。
	deleteDecodedFromSQL = `
DELETE FROM decoded_event
WHERE (tx_hash, log_index) IN (SELECT tx_hash, log_index FROM chain_log WHERE block_number >= $1)`

	deleteLogsFromSQL    = `DELETE FROM chain_log WHERE block_number >= $1`
	deleteHeadersFromSQL = `DELETE FROM block_header WHERE number >= $1`
)

// LocalTip 返回本地已写入的最高块头；表为空时 found 为 false。
func (s *Store) LocalTip(ctx context.Context) (HeaderTip, bool, error) {
	var (
		tip       HeaderTip
		number    int64
		hashBytes []byte
	)

	err := s.pool.QueryRow(ctx, loadTipSQL).Scan(&number, &hashBytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HeaderTip{}, false, nil
		}
		return HeaderTip{}, false, fmt.Errorf("store: load local tip: %w", err)
	}

	tip.Number = uint64(number)
	tip.Hash = common.BytesToHash(hashBytes)

	return tip, true, nil
}

// HeaderHashAt 返回本地记录的第 number 块的哈希，用于回溯分叉点。
func (s *Store) HeaderHashAt(ctx context.Context, number uint64) (common.Hash, bool, error) {
	var hashBytes []byte

	err := s.pool.QueryRow(ctx, loadHeaderHashSQL, int64(number)).Scan(&hashBytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return common.Hash{}, false, nil
		}
		return common.Hash{}, false, fmt.Errorf("store: load header hash %d: %w", number, err)
	}

	return common.BytesToHash(hashBytes), true, nil
}

// PromoteFinalized 把 (fromExclusive, toInclusive] 区间内尚未标记的块头置为已确认，
// 返回被更新的行数。区间端点由调用方按确认数水位算出。
func (s *Store) PromoteFinalized(ctx context.Context, tx pgx.Tx, fromExclusive, toInclusive uint64) (int64, error) {
	if toInclusive <= fromExclusive {
		return 0, nil
	}

	tag, err := tx.Exec(ctx, promoteFinalizedSQL, int64(fromExclusive), int64(toInclusive))
	if err != nil {
		return 0, fmt.Errorf("store: promote finalized blocks %d..%d: %w", fromExclusive+1, toInclusive, err)
	}

	return tag.RowsAffected(), nil
}

// DeleteFrom 删除 number 及之后的全部区块数据，返回删除的块头行数。
//
// 顺序固定：decoded_event → chain_log → block_header。反过来会丢失关联关系，
// 导致 decoded_event 里留下无法归属的孤儿行。
func (s *Store) DeleteFrom(ctx context.Context, tx pgx.Tx, number uint64) (int64, error) {
	if _, err := tx.Exec(ctx, deleteDecodedFromSQL, int64(number)); err != nil {
		return 0, fmt.Errorf("store: delete decoded events from block %d: %w", number, err)
	}
	if _, err := tx.Exec(ctx, deleteLogsFromSQL, int64(number)); err != nil {
		return 0, fmt.Errorf("store: delete logs from block %d: %w", number, err)
	}

	tag, err := tx.Exec(ctx, deleteHeadersFromSQL, int64(number))
	if err != nil {
		return 0, fmt.Errorf("store: delete headers from block %d: %w", number, err)
	}

	return tag.RowsAffected(), nil
}

// insertHeadersSQL 用数组参数一次性写入多行，避免逐条往返。
//
// 冲突时跳过而非覆盖：分叉由 reorg 流程先用 DeleteFrom 清理再重写，
// 不依赖这里覆盖旧行——否则「父哈希变了」这件事会被静默吞掉。
const insertHeadersSQL = `
INSERT INTO block_header (number, hash, parent_hash, block_time, is_finalized)
SELECT * FROM unnest($1::bigint[], $2::bytea[], $3::bytea[], $4::timestamptz[], $5::boolean[])
ON CONFLICT (number) DO NOTHING`

// InsertHeaders 批量写入块头，返回实际新增的行数。
func (s *Store) InsertHeaders(ctx context.Context, tx pgx.Tx, records []HeaderRecord) (int64, error) {
	if len(records) == 0 {
		return 0, nil
	}

	numbers := make([]int64, len(records))
	hashes := make([][]byte, len(records))
	parentHashes := make([][]byte, len(records))
	blockTimes := make([]time.Time, len(records))
	isFinalized := make([]bool, len(records))

	for i, record := range records {
		numbers[i] = int64(record.Number)
		hashes[i] = record.Hash.Bytes()
		parentHashes[i] = record.ParentHash.Bytes()
		blockTimes[i] = record.BlockTime
		isFinalized[i] = record.IsFinalized
	}

	tag, err := tx.Exec(ctx, insertHeadersSQL, numbers, hashes, parentHashes, blockTimes, isFinalized)
	if err != nil {
		return 0, fmt.Errorf("store: insert %d headers: %w", len(records), err)
	}

	return tag.RowsAffected(), nil
}
