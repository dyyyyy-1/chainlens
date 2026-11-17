package profile

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// Interaction 是地址与某个合约的交互汇总。
//
// 当前索引只覆盖一个目标合约，因此结果通常只有一行；等索引器支持多合约，
// 这个汇总才有分布意义。统计口径是**该合约发出的所有事件**，不限 Transfer。
type Interaction struct {
	Contract   common.Address
	EventCount int64
	LastBlock  uint64
	LastTime   time.Time
}

// interactionsSQL 按合约汇总地址参与过的事件数与最近活动块。
//
// 不按 topic0 过滤：交互历史看的是「这个地址在这个合约上出现过多少次」，
// 限定成 Transfer 会漏掉 Approval、Mint、Burn 等事件。
//
// LastTime 取 max(block_time)：block_time 由块号决定，与 block_number 单调一致，
// 不会出现「最大块号配了别的时间」。
const interactionsSQL = `
SELECT contract, count(*), max(block_number), max(block_time)
FROM chain_log
WHERE topic1 = $1 OR topic2 = $1
GROUP BY contract
ORDER BY count(*) DESC, max(block_number) DESC
LIMIT $2`

// Interactions 返回地址交互过的合约，按事件数降序。
//
// limit <= 0 用默认值，超过 maxLimit 按 maxLimit 截断。
func (p *Profile) Interactions(ctx context.Context, addr common.Address, limit int) ([]Interaction, error) {
	interactions := make([]Interaction, 0, clampLimit(limit))

	err := p.runReadOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, interactionsSQL, topic(addr), clampLimit(limit))
		if err != nil {
			return fmt.Errorf("query interactions: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				contract   []byte
				lastTime   time.Time
				interacted Interaction
			)
			if err := rows.Scan(&contract, &interacted.EventCount, &interacted.LastBlock, &lastTime); err != nil {
				return fmt.Errorf("scan interaction: %w", err)
			}

			interacted.Contract = common.BytesToAddress(contract)
			interacted.LastTime = lastTime

			interactions = append(interactions, interacted)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("profile: interactions of %s: %w", addr.Hex(), err)
	}

	return interactions, nil
}
