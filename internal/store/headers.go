package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// insertHeadersSQL 用数组参数一次性写入多行，避免逐条往返。
//
// 冲突时跳过而非覆盖：已落库的块头在 W1 阶段视为事实，分叉处置属于 reorg 逻辑（W2）。
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
