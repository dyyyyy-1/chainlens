package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// 本文件是写路径的集成测试。
//
// 为什么必须是集成测试：InsertLogs 走「COPY 到临时表 + ON CONFLICT 合并」，
// UpsertDecoded 走 `DO UPDATE`，DeleteFrom 依赖三条语句的**顺序**——
// 这些语义都在 PostgreSQL 里，用 mock 测等于测自己写的假实现。
//
// 隔离方式：TestMain 在 CHAINLENS_TEST_DSN 指向的库里建一个临时 schema，
// 所有连接都带 search_path。这样与 internal/profile 的只读集成测试可以同时跑，
// 也不会碰 public schema 里的开发数据。
var testDSN string

const testContractHex = "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"

var testContract = common.HexToAddress(testContractHex)

func TestMain(m *testing.M) {
	code, err := runStoreTests(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store 测试准备失败：%v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runStoreTests(m *testing.M) (int, error) {
	baseDSN := os.Getenv("CHAINLENS_TEST_DSN")
	if baseDSN == "" {
		fmt.Fprintln(os.Stderr, "未设置 CHAINLENS_TEST_DSN，跳过 store 集成测试")
		return 0, nil
	}

	ctx := context.Background()
	schema := fmt.Sprintf("store_test_%d_%d", os.Getpid(), time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		return 0, fmt.Errorf("连接测试库：%w", err)
	}
	defer admin.Close(ctx)

	if _, err := admin.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		return 0, fmt.Errorf("创建 schema %s：%w", schema, err)
	}
	// 无论用例成功与否都清掉，避免测试库堆积 schema。
	defer func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
	}()

	scopedDSN, err := withSearchPath(baseDSN, schema)
	if err != nil {
		return 0, err
	}
	if err := applyMigrations(ctx, scopedDSN); err != nil {
		return 0, err
	}

	testDSN = scopedDSN
	return m.Run(), nil
}

// withSearchPath 给 DSN 加上 search_path，让所有连接默认落在测试 schema。
func withSearchPath(dsn, schema string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("解析 DSN：%w", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// applyMigrations 在测试 schema 里建表。
//
// 用 simple protocol：migration 文件包含多条语句与 BEGIN/COMMIT，
// 扩展协议会把整个文件当成一个预处理语句而报错。
func applyMigrations(ctx context.Context, scopedDSN string) error {
	cfg, err := pgx.ParseConfig(scopedDSN)
	if err != nil {
		return fmt.Errorf("解析测试 DSN：%w", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("连接测试 schema：%w", err)
	}
	defer conn.Close(ctx)

	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		return fmt.Errorf("列出 migration：%w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("没有找到 migrations/*.sql，工作目录是否在 internal/store？")
	}
	sort.Strings(files)

	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("读取 %s：%w", file, err)
		}
		if _, err := conn.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("应用 %s：%w", file, err)
		}
	}
	return nil
}

// newTestStore 建一个干净的表集合。每个用例开头清表，使用例之间互不影响。
func newTestStore(t *testing.T) *Store {
	t.Helper()

	if testDSN == "" {
		t.Skip("未设置 CHAINLENS_TEST_DSN，跳过需要数据库的测试")
	}

	ctx := context.Background()
	st, err := New(ctx, testDSN)
	if err != nil {
		t.Fatalf("连接测试库失败：%v", err)
	}
	t.Cleanup(st.Close)

	// 显式列出表而不是用 CASCADE：将来新增表时，这里漏掉会立刻暴露为脏数据，
	// 而不是被 CASCADE 静默清理掉。
	if _, err := st.Pool().Exec(ctx,
		`TRUNCATE chain_log, block_header, decoded_event, index_cursor, contract_meta`); err != nil {
		t.Fatalf("清空测试表失败：%v", err)
	}
	return st
}

// mustTx 开一个事务，用例结束时兜底回滚（已提交的事务回滚是空操作）。
func mustTx(t *testing.T, st *Store) pgx.Tx {
	t.Helper()

	tx, err := st.Begin(context.Background())
	if err != nil {
		t.Fatalf("开启事务失败：%v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func sampleLog(block uint64, txByte byte, index uint32) LogRecord {
	txHash := common.Hash{}
	txHash[31] = txByte

	topic0 := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	topic1 := common.Hash{}
	topic1[31] = 0xaa

	return LogRecord{
		BlockNumber: block,
		BlockTime:   time.Unix(1_700_000_000+int64(block), 0).UTC(),
		TxHash:      txHash,
		LogIndex:    index,
		Contract:    testContract,
		Topic0:      &topic0,
		Topic1:      &topic1,
		Data:        []byte{0x01, 0x02, 0x03},
	}
}

func sampleHeader(number uint64, hashByte byte) HeaderRecord {
	hash := common.Hash{}
	hash[31] = hashByte
	parent := common.Hash{}
	parent[31] = hashByte - 1

	return HeaderRecord{
		Number:     number,
		Hash:       hash,
		ParentHash: parent,
		BlockTime:  time.Unix(1_700_000_000+int64(number), 0).UTC(),
	}
}

func sampleDecoded(txByte byte, index uint32, status string) DecodedRecord {
	txHash := common.Hash{}
	txHash[31] = txByte

	return DecodedRecord{
		TxHash:    txHash,
		LogIndex:  index,
		EventName: "Transfer",
		Status:    status,
		Payload:   map[string]any{"value": "1"},
	}
}

func countRows(t *testing.T, st *Store, table string) int64 {
	t.Helper()

	var count int64
	if err := st.Pool().QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
		t.Fatalf("统计 %s 失败：%v", table, err)
	}
	return count
}

func TestInsertLogsIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	records := []LogRecord{sampleLog(100, 1, 0), sampleLog(100, 2, 0)}

	tx := mustTx(t, st)
	inserted, err := st.InsertLogs(ctx, tx, records)
	if err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	if inserted != 2 {
		t.Fatalf("首次写入 %d 行，want 2", inserted)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	tx2 := mustTx(t, st)
	again, err := st.InsertLogs(ctx, tx2, records)
	if err != nil {
		t.Fatalf("重复写入失败：%v", err)
	}
	if again != 0 {
		t.Fatalf("重复写入返回 %d 行，幂等要求 0（HANDOFF 8.4）", again)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	if got := countRows(t, st, "chain_log"); got != 2 {
		t.Fatalf("chain_log = %d 行，want 2", got)
	}
}

func TestInsertLogsInsertsOnlyNewRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tx := mustTx(t, st)
	if _, err := st.InsertLogs(ctx, tx, []LogRecord{sampleLog(100, 1, 0), sampleLog(100, 2, 0)}); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	// 一条旧、一条新：只应新增 1 行，且旧行不被覆盖。
	tx2 := mustTx(t, st)
	mixed := []LogRecord{sampleLog(100, 1, 0), sampleLog(101, 3, 0)}
	inserted, err := st.InsertLogs(ctx, tx2, mixed)
	if err != nil {
		t.Fatalf("混合写入失败：%v", err)
	}
	if inserted != 1 {
		t.Fatalf("混合写入 %d 行，want 1", inserted)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	if got := countRows(t, st, "chain_log"); got != 3 {
		t.Fatalf("chain_log = %d 行，want 3", got)
	}
}

func TestInsertLogsPreservesTopicsAndNulls(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	record := sampleLog(200, 9, 3)
	record.Topic2 = nil
	record.Topic3 = nil

	tx := mustTx(t, st)
	if _, err := st.InsertLogs(ctx, tx, []LogRecord{record}); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	var (
		blockNumber int64
		logIndex    int32
		topic2      []byte
		topic3      []byte
		data        []byte
	)
	if err := tx.QueryRow(ctx,
		`SELECT block_number, log_index, topic2, topic3, data FROM chain_log WHERE tx_hash = $1`,
		record.TxHash.Bytes(),
	).Scan(&blockNumber, &logIndex, &topic2, &topic3, &data); err != nil {
		t.Fatalf("读取失败：%v", err)
	}

	if blockNumber != 200 || logIndex != 3 {
		t.Fatalf("block_number/log_index = %d/%d, want 200/3", blockNumber, logIndex)
	}
	if topic2 != nil || topic3 != nil {
		t.Fatalf("缺失的 topic 必须写成 NULL，得到 topic2=%v topic3=%v", topic2, topic3)
	}
	if string(data) != string(record.Data) {
		t.Fatalf("data = %x, want %x", data, record.Data)
	}
}

func TestInsertDecodedDoesNotOverwrite(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tx := mustTx(t, st)
	if _, err := st.InsertDecoded(ctx, tx, []DecodedRecord{sampleDecoded(1, 0, "undecodable")}); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	tx2 := mustTx(t, st)
	overwritten, err := st.InsertDecoded(ctx, tx2, []DecodedRecord{sampleDecoded(1, 0, "decoded")})
	if err != nil {
		t.Fatalf("重复写入失败：%v", err)
	}
	if overwritten != 0 {
		t.Fatalf("索引器的写入必须 DO NOTHING，返回 %d 行", overwritten)
	}

	// 同事务内检查：还没提交也能看到 DO NOTHING 之后仍是旧值。
	var status string
	if err := tx2.QueryRow(ctx, `SELECT status FROM decoded_event`).Scan(&status); err != nil {
		t.Fatalf("读取状态失败：%v", err)
	}
	if status != "undecodable" {
		t.Fatalf("status = %q，DO NOTHING 应保留 undecodable", status)
	}
}

func TestUpsertDecodedOverwrites(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tx := mustTx(t, st)
	if _, err := st.InsertDecoded(ctx, tx, []DecodedRecord{sampleDecoded(1, 0, "undecodable")}); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	tx2 := mustTx(t, st)
	updated, err := st.UpsertDecoded(ctx, tx2, []DecodedRecord{sampleDecoded(1, 0, "decoded")})
	if err != nil {
		t.Fatalf("覆盖写入失败：%v", err)
	}
	if updated != 1 {
		t.Fatalf("覆盖写入返回 %d 行，want 1", updated)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	var status string
	if err := st.Pool().QueryRow(ctx, `SELECT status FROM decoded_event`).Scan(&status); err != nil {
		t.Fatalf("读取状态失败：%v", err)
	}
	if status != "decoded" {
		t.Fatalf("status = %q，重解必须覆盖为 decoded", status)
	}
	if got := countRows(t, st, "decoded_event"); got != 1 {
		t.Fatalf("decoded_event = %d 行，覆盖写不应新增行", got)
	}
}

func TestAdvanceRequiresExistingCursor(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 游标行不存在时推进必须报错：静默成功会让索引器「以为自己在前进」。
	tx := mustTx(t, st)
	if err := st.Advance(ctx, tx, 100, 90); err == nil {
		t.Fatal("没有游标行时 Advance 应报错")
	}
	_ = tx.Rollback(ctx)

	if err := st.EnsureCursor(ctx, 1, 50); err != nil {
		t.Fatalf("EnsureCursor 失败：%v", err)
	}

	cursor, err := st.LoadCursor(ctx)
	if err != nil {
		t.Fatalf("LoadCursor 失败：%v", err)
	}
	if cursor.ChainID != 1 || cursor.NextBlock != 50 || cursor.FinalizedBlock != 50 {
		t.Fatalf("初始游标 = %+v, want chain=1 next=50 finalized=50", cursor)
	}

	tx2 := mustTx(t, st)
	if err := st.Advance(ctx, tx2, 200, 180); err != nil {
		t.Fatalf("Advance 失败：%v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	cursor, err = st.LoadCursor(ctx)
	if err != nil {
		t.Fatalf("LoadCursor 失败：%v", err)
	}
	if cursor.NextBlock != 200 || cursor.FinalizedBlock != 180 {
		t.Fatalf("推进后游标 = %+v, want next=200 finalized=180", cursor)
	}
}

func TestEnsureCursorDoesNotResetExisting(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.EnsureCursor(ctx, 1, 50); err != nil {
		t.Fatalf("首次 EnsureCursor 失败：%v", err)
	}

	tx := mustTx(t, st)
	if err := st.Advance(ctx, tx, 300, 280); err != nil {
		t.Fatalf("Advance 失败：%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	// 重启时再次 EnsureCursor 不能把游标拨回去——那等于静默丢数据重新扫。
	if err := st.EnsureCursor(ctx, 1, 50); err != nil {
		t.Fatalf("重复 EnsureCursor 失败：%v", err)
	}
	cursor, err := st.LoadCursor(ctx)
	if err != nil {
		t.Fatalf("LoadCursor 失败：%v", err)
	}
	if cursor.NextBlock != 300 {
		t.Fatalf("next_block = %d，EnsureCursor 不应覆盖已存在的游标", cursor.NextBlock)
	}
}

func TestDeleteFromRemovesTailInOrder(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tx := mustTx(t, st)
	if _, err := st.InsertHeaders(ctx, tx, []HeaderRecord{
		sampleHeader(100, 1), sampleHeader(101, 2), sampleHeader(102, 3),
	}); err != nil {
		t.Fatalf("写入块头失败：%v", err)
	}
	if _, err := st.InsertLogs(ctx, tx, []LogRecord{
		sampleLog(100, 1, 0), sampleLog(101, 2, 0), sampleLog(102, 3, 0),
	}); err != nil {
		t.Fatalf("写入日志失败：%v", err)
	}
	if _, err := st.InsertDecoded(ctx, tx, []DecodedRecord{
		sampleDecoded(1, 0, "decoded"), sampleDecoded(2, 0, "decoded"), sampleDecoded(3, 0, "decoded"),
	}); err != nil {
		t.Fatalf("写入解码结果失败：%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	tx2 := mustTx(t, st)
	deletedHeaders, err := st.DeleteFrom(ctx, tx2, 101)
	if err != nil {
		t.Fatalf("DeleteFrom 失败：%v", err)
	}
	if deletedHeaders != 2 {
		t.Fatalf("删除块头 %d 行，want 2", deletedHeaders)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	if got := countRows(t, st, "block_header"); got != 1 {
		t.Fatalf("block_header 剩 %d 行，want 1", got)
	}
	if got := countRows(t, st, "chain_log"); got != 1 {
		t.Fatalf("chain_log 剩 %d 行，want 1", got)
	}

	// decoded_event 没有 block_number，只能通过 chain_log 反查删除。
	// 顺序写反会留下无法归属的孤儿行，这里就是在守这件事。
	if got := countRows(t, st, "decoded_event"); got != 1 {
		t.Fatalf("decoded_event 剩 %d 行，want 1（分叉点之后必须清理干净）", got)
	}
}

func TestPromoteFinalizedIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	headers := make([]HeaderRecord, 0, 5)
	for i := uint64(0); i < 5; i++ {
		headers = append(headers, sampleHeader(100+i, byte(i+1)))
	}

	tx := mustTx(t, st)
	if _, err := st.InsertHeaders(ctx, tx, headers); err != nil {
		t.Fatalf("写入块头失败：%v", err)
	}
	promoted, err := st.PromoteFinalized(ctx, tx, 100, 102)
	if err != nil {
		t.Fatalf("PromoteFinalized 失败：%v", err)
	}
	if promoted != 2 {
		t.Fatalf("晋升 %d 行，want 2（101、102）", promoted)
	}
	again, err := st.PromoteFinalized(ctx, tx, 100, 102)
	if err != nil {
		t.Fatalf("重复 PromoteFinalized 失败：%v", err)
	}
	if again != 0 {
		t.Fatalf("重复晋升返回 %d 行，want 0（只动 is_finalized=false 的行）", again)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	var finalized int64
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM block_header WHERE is_finalized`).Scan(&finalized); err != nil {
		t.Fatalf("统计失败：%v", err)
	}
	if finalized != 2 {
		t.Fatalf("已确认块头 = %d，want 2", finalized)
	}
}

func TestInsertHeadersDoesNotOverwrite(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tx := mustTx(t, st)
	if _, err := st.InsertHeaders(ctx, tx, []HeaderRecord{sampleHeader(100, 1)}); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}

	// 同高度、不同哈希：必须保留旧行。分叉由 DeleteFrom 显式清理，
	// 若这里静默覆盖，「父哈希变了」这件事就被吞掉了。
	conflicting := sampleHeader(100, 9)
	inserted, err := st.InsertHeaders(ctx, tx, []HeaderRecord{conflicting})
	if err != nil {
		t.Fatalf("冲突写入失败：%v", err)
	}
	if inserted != 0 {
		t.Fatalf("冲突写入返回 %d 行，want 0", inserted)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	hash, found, err := st.HeaderHashAt(ctx, 100)
	if err != nil || !found {
		t.Fatalf("HeaderHashAt 失败：found=%v err=%v", found, err)
	}
	if hash != sampleHeader(100, 1).Hash {
		t.Fatalf("哈希被覆盖：%s", hash.Hex())
	}
}

func TestLocalTipAndHeaderHashAt(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, found, err := st.LocalTip(ctx); err != nil || found {
		t.Fatalf("空表时 LocalTip 应返回 found=false，得到 found=%v err=%v", found, err)
	}
	if _, found, err := st.HeaderHashAt(ctx, 100); err != nil || found {
		t.Fatalf("空表时 HeaderHashAt 应返回 found=false，得到 found=%v err=%v", found, err)
	}

	tx := mustTx(t, st)
	if _, err := st.InsertHeaders(ctx, tx, []HeaderRecord{sampleHeader(100, 1), sampleHeader(101, 2)}); err != nil {
		t.Fatalf("写入块头失败：%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	tip, found, err := st.LocalTip(ctx)
	if err != nil || !found {
		t.Fatalf("LocalTip 失败：found=%v err=%v", found, err)
	}
	if tip.Number != 101 {
		t.Fatalf("tip = %d, want 101", tip.Number)
	}
	if tip.Hash != sampleHeader(101, 2).Hash {
		t.Fatalf("tip 哈希 = %s", tip.Hash.Hex())
	}
}

func TestCoverageAndStats(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.EnsureCursor(ctx, 1, 100); err != nil {
		t.Fatalf("EnsureCursor 失败：%v", err)
	}

	tx := mustTx(t, st)
	if _, err := st.InsertLogs(ctx, tx, []LogRecord{sampleLog(100, 1, 0), sampleLog(102, 2, 0)}); err != nil {
		t.Fatalf("写入日志失败：%v", err)
	}
	if _, err := st.InsertDecoded(ctx, tx, []DecodedRecord{
		sampleDecoded(1, 0, "decoded"),
		sampleDecoded(2, 0, "undecodable"),
	}); err != nil {
		t.Fatalf("写入解码结果失败：%v", err)
	}
	if err := st.Advance(ctx, tx, 103, 90); err != nil {
		t.Fatalf("Advance 失败：%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	coverage, err := st.Coverage(ctx)
	if err != nil {
		t.Fatalf("Coverage 失败：%v", err)
	}
	if coverage.FromBlock != 100 || coverage.ToBlock != 102 {
		t.Fatalf("区间 = %d..%d, want 100..102", coverage.FromBlock, coverage.ToBlock)
	}
	if coverage.NextBlock != 103 || coverage.FinalizedBlock != 90 {
		t.Fatalf("水位 = next %d finalized %d, want 103/90", coverage.NextBlock, coverage.FinalizedBlock)
	}
	if coverage.LatestBlockTime.IsZero() {
		t.Fatal("LatestBlockTime 不应为零值")
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats 失败：%v", err)
	}
	if stats.TotalLogs != 2 || stats.DecodedCount != 1 || stats.UndecodableCount != 1 {
		t.Fatalf("统计 = total %d decoded %d undecodable %d, want 2/1/1",
			stats.TotalLogs, stats.DecodedCount, stats.UndecodableCount)
	}
}

func TestUndecodableSummaryAndLogs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tx := mustTx(t, st)
	if _, err := st.InsertLogs(ctx, tx, []LogRecord{
		sampleLog(100, 1, 0), sampleLog(100, 2, 0), sampleLog(105, 3, 0),
	}); err != nil {
		t.Fatalf("写入日志失败：%v", err)
	}
	if _, err := st.InsertDecoded(ctx, tx, []DecodedRecord{
		sampleDecoded(1, 0, "undecodable"),
		sampleDecoded(2, 0, "decoded"),
		sampleDecoded(3, 0, "decoded"),
	}); err != nil {
		t.Fatalf("写入解码结果失败：%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	summary, err := st.UndecodableSummary(ctx, testContract)
	if err != nil {
		t.Fatalf("UndecodableSummary 失败：%v", err)
	}
	if summary.Rows != 1 || summary.Blocks != 1 || summary.MinBlock != 100 || summary.MaxBlock != 100 {
		t.Fatalf("summary = %+v, want 1 条 / 1 块 / 100..100", summary)
	}

	logs, err := st.UndecodableLogs(ctx, 100, 100, testContract)
	if err != nil {
		t.Fatalf("UndecodableLogs 失败：%v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("区间 100..100 内未解码日志 = %d 条，want 1", len(logs))
	}
	if logs[0].TxHash != sampleLog(100, 1, 0).TxHash {
		t.Fatalf("取到了错误的日志：%s", logs[0].TxHash.Hex())
	}

	// 已解码的行不能被选中，否则重解会做无意义的覆盖写。
	if got, err := st.UndecodableLogs(ctx, 105, 105, testContract); err != nil || len(got) != 0 {
		t.Fatalf("已解码区间应返回空，得到 %d 条 err=%v", len(got), err)
	}

	// 多合约隔离：另一个合约的未解码日志不能被算进来。
	// 拿 A 的 ABI 去解 B 的日志只会得到「全部 undecodable」，看起来像重解失败。
	other := common.HexToAddress("0x1111111111111111111111111111111111111111")
	otherLog := sampleLog(100, 4, 0)
	otherLog.Contract = other

	tx2 := mustTx(t, st)
	if _, err := st.InsertLogs(ctx, tx2, []LogRecord{otherLog}); err != nil {
		t.Fatalf("写入其他合约日志失败：%v", err)
	}
	if _, err := st.InsertDecoded(ctx, tx2, []DecodedRecord{sampleDecoded(4, 0, "undecodable")}); err != nil {
		t.Fatalf("写入其他合约解码结果失败：%v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	if summary, err := st.UndecodableSummary(ctx, testContract); err != nil || summary.Rows != 1 {
		t.Fatalf("按合约统计应仍为 1 条，得到 %+v err=%v", summary, err)
	}
	if got, err := st.UndecodableLogs(ctx, 100, 100, other); err != nil || len(got) != 1 {
		t.Fatalf("其他合约应只返回 1 条，得到 %d 条 err=%v", len(got), err)
	}
}

func TestUpsertContractMetaKeepsABIWhenEmpty(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	abi := []byte(`[{"type":"event","name":"Transfer"}]`)
	if err := st.UpsertContractMeta(ctx, ContractMeta{
		Address:   testContract,
		ABI:       abi,
		ABISource: "sourcify",
	}); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}

	// 第二次解析失败时 ABI 为空：不能用空值覆盖已拿到的 ABI。
	if err := st.UpsertContractMeta(ctx, ContractMeta{
		Address:   testContract,
		ABI:       nil,
		ABISource: "fallback",
	}); err != nil {
		t.Fatalf("二次写入失败：%v", err)
	}

	meta, found, err := st.LoadContractMeta(ctx, testContract)
	if err != nil || !found {
		t.Fatalf("LoadContractMeta 失败：found=%v err=%v", found, err)
	}
	if len(meta.ABI) == 0 {
		t.Fatal("ABI 被空值覆盖了")
	}
	if meta.ABISource != "fallback" {
		t.Fatalf("abi_source = %q, want fallback（来源应随最新一次解析更新）", meta.ABISource)
	}
}

// 保证测试里用到的 DSN 拼接不会破坏原有参数。
func TestWithSearchPathPreservesQuery(t *testing.T) {
	got, err := withSearchPath("postgres://u:p@localhost:5432/db?sslmode=disable", "store_test_x")
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if !strings.Contains(got, "sslmode=disable") {
		t.Fatalf("sslmode 丢失：%s", got)
	}
	if !strings.Contains(got, "search_path=store_test_x") {
		t.Fatalf("search_path 未加上：%s", got)
	}
}
