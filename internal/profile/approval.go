package profile

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// Approval 是地址作为 owner 的一条授权记录。
//
// 取的是**已索引区间内**该 (contract, spender) 的最新一条 Approval 日志，
// 它不代表链上当前 allowance：区间之外的后续授权或撤销都不可见。
// 要判断真实额度必须查链上 allowance()，不属本包职责。
type Approval struct {
	Contract common.Address
	Spender  common.Address
	Value    *big.Int
	// IsUnlimited 表示 Value 为 2^256-1，即 ERC20 的无限额度惯例。
	IsUnlimited bool
	BlockNumber uint64
	BlockTime   time.Time
	TxHash      common.Hash
}

// approvalsSQL 取每个 (contract, spender) 在区间内的最新一条授权。
//
// DISTINCT ON 的排序必须是「分组键 + 时间倒序」：先按 (contract, spender) 分组，
// 组内按 (block_number, log_index) 倒序，第一行就是最新一条。
// log_index 参与排序是必要的——同一笔交易里可以有多条 Approval。
const approvalsSQL = `
WITH latest AS (
    SELECT DISTINCT ON (contract, topic2)
           contract, topic2 AS spender, data, block_number, block_time, tx_hash, log_index
    FROM chain_log
    WHERE topic0 = $2
      AND topic1 = $1
      AND length(data) = 32
    ORDER BY contract, topic2, block_number DESC, log_index DESC
)
SELECT contract, spender, round(` + u256Expr + `)::text, block_number, block_time, tx_hash
FROM latest
ORDER BY block_number DESC, log_index DESC
LIMIT $3`

// Approvals 返回地址作为 owner 的授权列表，按最新授权所在块倒序。
//
// limit <= 0 用默认值，超过 maxLimit 按 maxLimit 截断。
func (p *Profile) Approvals(ctx context.Context, addr common.Address, limit int) ([]Approval, error) {
	approvals := make([]Approval, 0, clampLimit(limit))

	err := p.runReadOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, approvalsSQL, topic(addr), approvalTopic.Bytes(), clampLimit(limit))
		if err != nil {
			return fmt.Errorf("query approvals: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				contract  []byte
				spender   []byte
				value     string
				txHash    []byte
				approval  Approval
				blockTime time.Time
			)
			if err := rows.Scan(&contract, &spender, &value, &approval.BlockNumber, &blockTime, &txHash); err != nil {
				return fmt.Errorf("scan approval: %w", err)
			}

			amount, err := parseAmount(value)
			if err != nil {
				return err
			}

			approval.Contract = common.BytesToAddress(contract)
			approval.Spender = common.BytesToAddress(spender)
			approval.Value = amount
			approval.IsUnlimited = amount.Cmp(maxUint256) == 0
			approval.BlockTime = blockTime
			approval.TxHash = common.BytesToHash(txHash)

			approvals = append(approvals, approval)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("profile: approvals of %s: %w", addr.Hex(), err)
	}

	return approvals, nil
}
