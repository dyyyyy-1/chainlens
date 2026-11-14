package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"

	"github.com/dyyyyy-1/chainlens/internal/store"
)

// LogFetcher 由索引器声明、由 internal/chain 实现（接口定义在使用方，HANDOFF.md 第 11 节）。
type LogFetcher interface {
	Fetch(ctx context.Context, from, to uint64, addr common.Address) ([]types.Log, error)
}

// HeaderFetcher 提供块头与链头高度。
type HeaderFetcher interface {
	LatestBlockNumber(ctx context.Context) (uint64, error)
	HeadersByRange(ctx context.Context, numbers []uint64) (map[uint64]*types.Header, error)
}

// LogStore 是索引器所需的持久化能力。写方法接受调用方事务，以便与游标推进同事务提交。
type LogStore interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	EnsureCursor(ctx context.Context, chainID, startBlock uint64) error
	LoadCursor(ctx context.Context) (store.Cursor, error)
	InsertHeaders(ctx context.Context, tx pgx.Tx, records []store.HeaderRecord) (int64, error)
	InsertLogs(ctx context.Context, tx pgx.Tx, records []store.LogRecord) (int64, error)
	Advance(ctx context.Context, tx pgx.Tx, nextBlock, finalizedBlock uint64) error
}

// Config 是索引器运行参数，取自 HANDOFF.md 第 10 节的环境变量。
type Config struct {
	ChainID       uint64
	Contract      common.Address
	StartBlock    uint64
	Confirmations uint64
	BatchBlocks   uint64
	PollInterval  time.Duration
}

// Scanner 驱动索引主循环。
type Scanner struct {
	logs    LogFetcher
	headers HeaderFetcher
	store   LogStore
	cfg     Config
	logger  *slog.Logger
}

// New 构造索引器。logger 为 nil 时回落到 slog 默认实例。
func New(logs LogFetcher, headers HeaderFetcher, st LogStore, cfg Config, logger *slog.Logger) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scanner{logs: logs, headers: headers, store: st, cfg: cfg, logger: logger}
}

// Run 持续扫描直到 ctx 结束。
//
// 单批失败不终止进程：记录错误后等待下一轮重试。游标未推进，因此重试不会产生重复数据。
func (s *Scanner) Run(ctx context.Context) error {
	if err := s.store.EnsureCursor(ctx, s.cfg.ChainID, s.cfg.StartBlock); err != nil {
		return err
	}

	for {
		if err := s.scanOnce(ctx); err != nil {
			if ctx.Err() != nil {
				s.logger.Info("indexer stopped", "reason", ctx.Err())
				return nil
			}
			s.logger.Error("scan batch failed", "err", err)
		}

		select {
		case <-ctx.Done():
			s.logger.Info("indexer stopped", "reason", ctx.Err())
			return nil
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

// scanOnce 完成一个批次的完整流程：取区间、拉日志、补块头、落库、推进游标。
func (s *Scanner) scanOnce(ctx context.Context) error {
	cursor, err := s.store.LoadCursor(ctx)
	if err != nil {
		return err
	}
	if cursor.ChainID != s.cfg.ChainID {
		return fmt.Errorf("indexer: cursor chain id %d does not match configured chain id %d", cursor.ChainID, s.cfg.ChainID)
	}

	head, err := s.headers.LatestBlockNumber(ctx)
	if err != nil {
		return err
	}

	blockRange, hasWork := nextRange(cursor.NextBlock, head, s.cfg.Confirmations, s.cfg.BatchBlocks)
	if !hasWork {
		return nil
	}

	started := time.Now()

	rawLogs, err := s.logs.Fetch(ctx, blockRange.From, blockRange.To, s.cfg.Contract)
	if err != nil {
		return err
	}

	headers, err := s.headers.HeadersByRange(ctx, distinctBlockNumbers(rawLogs))
	if err != nil {
		return err
	}

	logRecords := make([]store.LogRecord, 0, len(rawLogs))
	for _, raw := range rawLogs {
		header, hasHeader := headers[raw.BlockNumber]
		if !hasHeader {
			return fmt.Errorf("indexer: block %d returned logs but no header", raw.BlockNumber)
		}
		logRecords = append(logRecords, store.NewLogRecord(raw, blockTime(header)))
	}

	// ADR-1：区块数据与游标必须在同一事务内提交，否则崩溃会导致游标前进而数据缺失。
	tx, err := s.store.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	insertedHeaders, err := s.store.InsertHeaders(ctx, tx, headerRecords(headers, blockRange.To))
	if err != nil {
		return err
	}
	insertedLogs, err := s.store.InsertLogs(ctx, tx, logRecords)
	if err != nil {
		return err
	}
	if err := s.store.Advance(ctx, tx, blockRange.To+1, blockRange.To); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("indexer: commit batch %d..%d: %w", blockRange.From, blockRange.To, err)
	}

	s.logger.Info("batch committed",
		"from", blockRange.From,
		"to", blockRange.To,
		"logs_fetched", len(rawLogs),
		"logs_inserted", insertedLogs,
		"headers_inserted", insertedHeaders,
		"elapsed_ms", time.Since(started).Milliseconds(),
	)

	return nil
}

// distinctBlockNumbers 返回日志涉及的块号，去重并升序，供批量拉取块头使用。
func distinctBlockNumbers(logs []types.Log) []uint64 {
	seen := make(map[uint64]struct{}, len(logs))
	numbers := make([]uint64, 0, len(logs))
	for _, entry := range logs {
		if _, hasSeen := seen[entry.BlockNumber]; hasSeen {
			continue
		}
		seen[entry.BlockNumber] = struct{}{}
		numbers = append(numbers, entry.BlockNumber)
	}

	slices.Sort(numbers)
	return numbers
}

// headerRecords 把块头映射转换为待写入记录，按块号升序保证写入顺序稳定。
func headerRecords(headers map[uint64]*types.Header, finalizedUpTo uint64) []store.HeaderRecord {
	records := make([]store.HeaderRecord, 0, len(headers))
	for number, header := range headers {
		records = append(records, store.HeaderRecord{
			Number:      number,
			Hash:        header.Hash(),
			ParentHash:  header.ParentHash,
			BlockTime:   blockTime(header),
			IsFinalized: number <= finalizedUpTo,
		})
	}

	slices.SortFunc(records, func(a, b store.HeaderRecord) int {
		switch {
		case a.Number < b.Number:
			return -1
		case a.Number > b.Number:
			return 1
		default:
			return 0
		}
	})

	return records
}

// blockTime 把块头中的 unix 秒转为 UTC 时间。
func blockTime(header *types.Header) time.Time {
	return time.Unix(int64(header.Time), 0).UTC()
}
