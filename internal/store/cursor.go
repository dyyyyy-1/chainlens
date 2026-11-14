package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	ensureCursorSQL = `
INSERT INTO index_cursor (id, chain_id, next_block, finalized_block)
VALUES (1, $1, $2, $2)
ON CONFLICT (id) DO NOTHING`

	loadCursorSQL = `SELECT chain_id, next_block, finalized_block FROM index_cursor WHERE id = 1`

	advanceCursorSQL = `
UPDATE index_cursor
SET next_block = $1, finalized_block = $2, updated_at = now()
WHERE id = 1`
)

// EnsureCursor 保证单行游标存在。
//
// index_cursor 的 CHECK (id = 1) 使「两个游标」在数据库层面不可能出现，
// 因此首次启动的初始化可以安全地依赖 ON CONFLICT DO NOTHING。
func (s *Store) EnsureCursor(ctx context.Context, chainID, startBlock uint64) error {
	if _, err := s.pool.Exec(ctx, ensureCursorSQL, int64(chainID), int64(startBlock)); err != nil {
		return fmt.Errorf("store: ensure cursor for chain %d: %w", chainID, err)
	}
	return nil
}

// LoadCursor 读取当前游标。
func (s *Store) LoadCursor(ctx context.Context) (Cursor, error) {
	var cursor Cursor
	if err := s.pool.QueryRow(ctx, loadCursorSQL).Scan(&cursor.ChainID, &cursor.NextBlock, &cursor.FinalizedBlock); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Cursor{}, fmt.Errorf("store: cursor row missing: %w", err)
		}
		return Cursor{}, fmt.Errorf("store: load cursor: %w", err)
	}
	return cursor, nil
}

// Advance 在调用方事务内推进游标，必须与区块数据写入同时提交（HANDOFF.md ADR-1）。
//
// nextBlock 是下一个待扫描高度，finalizedBlock 是已确认水位。
func (s *Store) Advance(ctx context.Context, tx pgx.Tx, nextBlock, finalizedBlock uint64) error {
	tag, err := tx.Exec(ctx, advanceCursorSQL, int64(nextBlock), int64(finalizedBlock))
	if err != nil {
		return fmt.Errorf("store: advance cursor to %d: %w", nextBlock, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: advance cursor to %d: updated %d rows, expected 1", nextBlock, tag.RowsAffected())
	}
	return nil
}
