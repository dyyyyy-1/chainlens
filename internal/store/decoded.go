package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// insertDecodedSQL 用 unnest 一次写入多行，payload 以 JSON 文本传入后在库内转 jsonb。
const insertDecodedSQL = `
INSERT INTO decoded_event (tx_hash, log_index, event_name, status, payload)
SELECT tx_hash, log_index, event_name, status, payload::jsonb
FROM unnest($1::bytea[], $2::integer[], $3::text[], $4::text[], $5::text[])
     AS staged(tx_hash, log_index, event_name, status, payload)
ON CONFLICT (tx_hash, log_index) DO NOTHING`

// InsertDecoded 批量写入解码结果，返回实际新增的行数。
//
// undecodable 的记录同样会写入，只是 payload 为空 —— 这样「哪些日志没解出来」
// 可以直接用 status 列查出，不需要靠 chain_log 与 decoded_event 的左连接反推。
func (s *Store) InsertDecoded(ctx context.Context, tx pgx.Tx, records []DecodedRecord) (int64, error) {
	if len(records) == 0 {
		return 0, nil
	}

	txHashes := make([][]byte, len(records))
	logIndexes := make([]int32, len(records))
	names := make([]string, len(records))
	statuses := make([]string, len(records))
	payloads := make([]string, len(records))

	for i, record := range records {
		payload, err := json.Marshal(record.Payload)
		if err != nil {
			return 0, fmt.Errorf("store: marshal payload of %s#%d: %w", record.TxHash.Hex(), record.LogIndex, err)
		}
		txHashes[i] = record.TxHash.Bytes()
		logIndexes[i] = int32(record.LogIndex)
		names[i] = record.EventName
		statuses[i] = record.Status
		payloads[i] = string(payload)
	}

	tag, err := tx.Exec(ctx, insertDecodedSQL, txHashes, logIndexes, names, statuses, payloads)
	if err != nil {
		return 0, fmt.Errorf("store: insert %d decoded events: %w", len(records), err)
	}

	return tag.RowsAffected(), nil
}
