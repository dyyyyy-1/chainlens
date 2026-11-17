// Package profile 提供地址画像查询：代币净流入流出、授权记录、合约交互汇总。
//
// 三类查询都只读本地 chain_log，不走实时 eth_getLogs
// （docs/SESSION-HANDOFF.md §5.1 第 4 条：免费端点单次 getLogs 跨度上限只有 8,000~10,000 块，
// 实时查询只能覆盖约 33 小时，产品语义失效）。
//
// 用户硬约束是「必须有过滤，禁止全量扫描」，落实为三点：
//   - 每个方法都带 LIMIT，且由 clampLimit 收敛上限；
//   - 每条查询都在只读事务里设置 statement_timeout，超时由数据库中断；
//   - 地址查询一律左填充成 32 字节 topic，保证走 idx_log_topic1 / idx_log_topic2。
//
// 不建预计算表、不建缓存表、不建物化视图：查询即计算。
package profile

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// defaultLimit 是未指定条数时的默认值，maxLimit 是调用方可请求的上限。
	// 收敛上限是为了避免「一次把整个地址的流水拉出来」。
	defaultLimit = 50
	maxLimit     = 200

	// queryTimeout 是单条画像查询的硬上限。超时由 PostgreSQL 中断，
	// 而不是让慢查询一直占着连接池。
	queryTimeout = "3s"

	// queryWorkMem 让最热地址的聚合排序留在内存。
	// 实测 5.6 万行需要约 9 MB，默认 4 MB 会溢写 7 MB 到磁盘。
	queryWorkMem = "16MB"
)

// Querier 是画像查询所需的最小数据访问能力，由 *pgxpool.Pool 满足。
// 接口定义在使用方（HANDOFF.md 第 11 节），profile 不 import store。
type Querier interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

// Profile 是地址画像查询的入口。
type Profile struct {
	db Querier
}

// New 用只读数据源构造画像查询器。
func New(db Querier) *Profile {
	return &Profile{db: db}
}

// Holding 是地址在某个代币合约上的净流入流出。
//
// Net 是**已索引区间内的净变化**，不是链上余额：本地只覆盖部分区块，
// 区间外的历史流水不可见，因此活跃地址的 Net 完全可能为负。
// 想知道真实余额必须查链上 balanceOf()，不属本包职责。
type Holding struct {
	Contract common.Address
	Net      *big.Int
	LogCount int64
}

// 画像只关心 Transfer 与 Approval 两个事件。
//
// 签名串在初始化时算哈希，不手抄 topic0 常量——手抄的 32 字节常量没有校验手段，
// 一旦写错，查询会静默返回空结果。不直接复用 internal/decoder 的签名表，
// 是因为那是解码器的内部细节，画像只需两个事件，耦合过去反而扩大依赖面。
var (
	transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	approvalTopic = crypto.Keccak256Hash([]byte("Approval(address,address,uint256)"))
)

// maxUint256 是 2^256-1，ERC20 用它表示无限额度。
var maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// u256Expr 把 chain_log.data 的前 32 字节按无符号 256 位整数转成 numeric。
//
// PostgreSQL 17 不支持 bit(256)::numeric（会报「不能把类型 bit 转换为 numeric」），
// 只能拆成 8 个 32 位分肢再乘 2 的幂。32 位分肢的取值一定在 bigint 范围内，不会溢出，
// 所以中间量用 bigint、结果用 numeric，全程精确。
//
// 该表达式已与 decoded_event 的十进制字符串在 24 万条 Transfer 上逐条对账，零差异。
// 引用它的查询必须把目标列命名为 data。
const u256Expr = `(
      ('x' || encode(substring(data from 1 for 4), 'hex'))::bit(32)::bigint * power(2::numeric, 224)
    + ('x' || encode(substring(data from 5 for 4), 'hex'))::bit(32)::bigint * power(2::numeric, 192)
    + ('x' || encode(substring(data from 9 for 4), 'hex'))::bit(32)::bigint * power(2::numeric, 160)
    + ('x' || encode(substring(data from 13 for 4), 'hex'))::bit(32)::bigint * power(2::numeric, 128)
    + ('x' || encode(substring(data from 17 for 4), 'hex'))::bit(32)::bigint * power(2::numeric, 96)
    + ('x' || encode(substring(data from 21 for 4), 'hex'))::bit(32)::bigint * power(2::numeric, 64)
    + ('x' || encode(substring(data from 25 for 4), 'hex'))::bit(32)::bigint * power(2::numeric, 32)
    + ('x' || encode(substring(data from 29 for 4), 'hex'))::bit(32)::bigint
)`

// holdingsSQL 按合约聚合地址的 Transfer 净额。
//
// 关键点：
//   - topic1 = 地址是转出，topic2 = 地址是转入，单次 BitmapOr 扫描同时命中两个索引；
//   - 自转（topic1 = topic2）按 0 计，否则会把同一笔钱算成单边流出；
//   - length(data) = 32 过滤掉非标准布局的 Transfer，避免 substring 取到残缺值。
const holdingsSQL = `
WITH signed AS (
    SELECT contract,
           CASE
               WHEN topic1 = topic2 THEN 0::numeric
               WHEN topic1 = $1     THEN -` + u256Expr + `
               ELSE ` + u256Expr + `
           END AS delta
    FROM chain_log
    WHERE topic0 = $2
      AND (topic1 = $1 OR topic2 = $1)
      AND length(data) = 32
)
SELECT contract, round(sum(delta))::text, count(*)
FROM signed
GROUP BY contract
ORDER BY abs(sum(delta)) DESC
LIMIT $3`

// applyLimitsSQL 在事务内设置超时与内存上限。
//
// set_config 的第三个参数为 true 等价于 SET LOCAL：事务结束自动失效，
// 因此不会因为连接池复用把设置残留给后续请求。
const applyLimitsSQL = `SELECT set_config('statement_timeout', $1, true), set_config('work_mem', $2, true)`

// ErrQueryTimeout 表示查询被 statement_timeout 中断。
//
// 这是「禁止全量扫描」的兜底行为：地址历史过重时宁可失败，也不占住连接。
// API 层应把它映射成 503，而不是 500。
var ErrQueryTimeout = errors.New("profile: query exceeded statement_timeout")

// Holdings 返回地址在已索引区间内的各合约净变化，按绝对值降序。
//
// limit <= 0 用默认值，超过 maxLimit 按 maxLimit 截断。
func (p *Profile) Holdings(ctx context.Context, addr common.Address, limit int) ([]Holding, error) {
	holdings := make([]Holding, 0, clampLimit(limit))

	err := p.runReadOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, holdingsSQL, topic(addr), transferTopic.Bytes(), clampLimit(limit))
		if err != nil {
			return fmt.Errorf("query holdings: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				contract []byte
				net      string
				logCount int64
			)
			if err := rows.Scan(&contract, &net, &logCount); err != nil {
				return fmt.Errorf("scan holding: %w", err)
			}
			amount, err := parseAmount(net)
			if err != nil {
				return err
			}
			holdings = append(holdings, Holding{
				Contract: common.BytesToAddress(contract),
				Net:      amount,
				LogCount: logCount,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("profile: holdings of %s: %w", addr.Hex(), err)
	}

	return holdings, nil
}

// runReadOnly 在一个只读事务里执行单条查询，并统一设置资源上限。
func (p *Profile) runReadOnly(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin read-only tx: %w", err)
	}
	// 只读事务，回滚与提交等价；用回滚避免误提交未预期的写操作。
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, applyLimitsSQL, queryTimeout, queryWorkMem); err != nil {
		return fmt.Errorf("apply query limits: %w", err)
	}

	if err := fn(tx); err != nil {
		return classify(err)
	}

	return nil
}

// classify 把 statement_timeout 触发的中断（SQLSTATE 57014）翻译成 ErrQueryTimeout。
func classify(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return fmt.Errorf("%w: %s", ErrQueryTimeout, pgErr.Message)
	}
	return err
}

// clampLimit 把调用方传入的条数收敛到 [1, maxLimit]。
func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultLimit
	case limit > maxLimit:
		return maxLimit
	default:
		return limit
	}
}

// topic 把 20 字节地址左填充成 32 字节 topic。
//
// chain_log.topic1/topic2 存的是完整 32 字节 topic。直接拿 20 字节地址去查，
// 比较值长度不一致，索引失效退化成全表扫描。
func topic(addr common.Address) []byte {
	return common.BytesToHash(addr.Bytes()).Bytes()
}

// parseAmount 把数据库返回的十进制文本转成 big.Int。
//
// 数值在库内已完成 round()，文本是纯整数。走字符串而不是 float64，
// 是因为 USDC 之外还有 18 位小数的代币，uint256 的量级必然超出 float64 的精度。
func parseAmount(s string) (*big.Int, error) {
	amount, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("parse amount %q: not a decimal integer", s)
	}
	return amount, nil
}
